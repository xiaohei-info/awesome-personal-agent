package conn

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

type fake struct {
	cfg  Config
	send func(context.Context, Config, Message) (Receipt, error)
	lis  func(context.Context) error
}

func (f *fake) Send(ctx context.Context, m Message) (Receipt, error) { return f.send(ctx, f.cfg, m) }
func (f *fake) Test(context.Context) error                           { return nil }

type listening struct{ *fake }

func (l listening) Listen(ctx context.Context, _ Inbound) error { return l.lis(ctx) }

func decl() Decl {
	t := Text{"x", "x"}
	return Decl{Type: "fake", Direction: DirectionChannel, Title: t, Tagline: t, Default: t, Via: t,
		Fields: []Field{{Key: "server", Kind: KindURL, Label: t, Default: "https://example.invalid", Required: true},
			{Key: "token", Kind: KindSecret, Label: t, Required: true}}}
}

func newCaller(t *testing.T, f *fake, listen bool) (*Caller, *store.Store, *secrets.Vault) {
	t.Helper()
	reg := store.NewRegistry()
	reg.Register(secrets.Module)
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v := secrets.Generate()
	c, err := New(Options{Store: st, Vault: v, Timeout: 200 * time.Millisecond, Plugins: []Plugin{{Decl: decl(), New: func(cfg Config) (Channel, error) {
		g := *f
		g.cfg = cfg
		if listen {
			return listening{&g}, nil
		}
		return &g, nil
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	return c, st, v
}

func putSecret(t *testing.T, st *store.Store, v *secrets.Vault, owner, field, val string) {
	t.Helper()
	if err := st.Update(ctx, func(tx *store.Tx) error { return v.Put(tx, owner, field, val, time.Now()) }); err != nil {
		t.Fatal(err)
	}
}

// 插件拿到本连接的凭据；报错里的凭据值被替换掉，问题代码保留。
func TestSendPassesSecretsAndScrubsErrors(t *testing.T) {
	f := &fake{send: func(_ context.Context, cfg Config, _ Message) (Receipt, error) {
		return Receipt{}, Fail(ProblemAuthFailed, "GET https://api.example.invalid/bot"+cfg.Secrets["token"]+"/send: 401")
	}}
	c, st, v := newCaller(t, f, false)
	putSecret(t, st, v, "ch-1", "token", "123:SECRET")
	_, err := c.Send(ctx, Target{ID: "ch-1", Type: "fake"}, Message{Key: "k"})
	if ProblemOf(err) != ProblemAuthFailed || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "***") {
		t.Fatalf("err = %v", err)
	}
	// 没有凭据：不调插件，报 credentials_missing
	if _, err := c.Send(ctx, Target{ID: "ch-2", Type: "fake"}, Message{}); ProblemOf(err) != ProblemCredentialsMissing {
		t.Fatalf("missing secret err = %v", err)
	}
}

// panic 被恢复成失败；不理会 ctx 的插件在超时后返回；同一连接串行。
func TestCallerSurvivesPanicTimeoutAndSerializes(t *testing.T) {
	var mode atomic.Int32
	var inflight, peak atomic.Int32
	release := make(chan struct{})
	f := &fake{send: func(ctx context.Context, _ Config, _ Message) (Receipt, error) {
		switch mode.Load() {
		case 0:
			panic("boom 123:SECRET")
		case 1:
			<-release // 不理会 ctx
			return Receipt{Ref: "late"}, nil
		}
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inflight.Add(-1)
		return Receipt{Ref: "ok"}, nil
	}}
	c, st, v := newCaller(t, f, false)
	putSecret(t, st, v, "ch-1", "token", "123:SECRET")
	tg := Target{ID: "ch-1", Type: "fake"}
	if _, err := c.Send(ctx, tg, Message{}); ProblemOf(err) != ProblemTemporary {
		t.Fatalf("panic err = %v", err)
	}
	mode.Store(1)
	start := time.Now()
	if _, err := c.Send(ctx, tg, Message{}); ProblemOf(err) != ProblemUnreachable || time.Since(start) > time.Second {
		t.Fatalf("timeout err = %v after %v", err, time.Since(start))
	}
	close(release)
	mode.Store(2)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Send(ctx, tg, Message{})
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("concurrent sends on one connection: peak %d", peak.Load())
	}
}

// 收听断开后重连；断开超过阈值报告一次没在收听。
func TestKeepReconnectsAndReportsNotListening(t *testing.T) {
	ListenBackoffMin, ListenBackoffMax, NotListeningAfter = 10*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		ListenBackoffMin, ListenBackoffMax, NotListeningAfter = 5*time.Second, 5*time.Minute, 2*time.Minute
	})
	var n atomic.Int32
	f := &fake{lis: func(ctx context.Context) error {
		n.Add(1)
		return errors.New("dropped")
	}}
	c, st, v := newCaller(t, f, true)
	putSecret(t, st, v, "ch-1", "token", "x")
	cx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var got []bool
	c.Keep(cx, Target{ID: "ch-1", Type: "fake"}, nil, func(l bool) { mu.Lock(); got = append(got, l); mu.Unlock() })
	down := 0
	for _, l := range got {
		if !l {
			down++
		}
	}
	if n.Load() < 3 || down == 0 {
		t.Fatalf("listens=%d reports=%v", n.Load(), got)
	}
}

func TestDeclCheckAndNormalize(t *testing.T) {
	d := decl()
	if err := d.Check(); err != nil {
		t.Fatal(err)
	}
	bad := d
	bad.Title = Text{Zh: "x"}
	if bad.Check() == nil {
		t.Fatal("missing en accepted")
	}
	got, err := d.Normalize(map[string]string{}, func(string) bool { return true })
	if err != nil || got["server"] != "https://example.invalid" {
		t.Fatalf("defaults: %v %v", got, err)
	}
	if _, err := d.Normalize(map[string]string{"token": "x"}, func(string) bool { return true }); err == nil {
		t.Fatal("secret accepted as setting")
	}
	if _, err := d.Normalize(map[string]string{"server": "https://u:p@h"}, func(string) bool { return true }); err == nil {
		t.Fatal("url with credentials accepted")
	}
	if _, err := d.Normalize(nil, func(string) bool { return false }); err == nil {
		t.Fatal("missing required secret accepted")
	}
}
