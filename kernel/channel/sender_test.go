package channel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/connections"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const secretVal = "tok-PLAINTEXT-42"

// fakeChannel 是测试里的假渠道插件：记下收到的消息，按 fail 决定成败。
type fakeChannel struct {
	mu   sync.Mutex
	got  []conn.Message
	fail error
}

func (f *fakeChannel) plugin() conn.Plugin {
	t := conn.Text{Zh: "假", En: "fake"}
	return conn.Plugin{Decl: conn.Decl{Type: "fake", Direction: conn.DirectionChannel, Title: t, Tagline: t, Default: t, Via: t,
		Fields: []conn.Field{{Key: "token", Kind: conn.KindSecret, Label: t, Required: true}}},
		New: func(cfg conn.Config) (conn.Channel, error) { return &fakeInst{f, cfg}, nil }}
}

type fakeInst struct {
	f   *fakeChannel
	cfg conn.Config
}

func (i *fakeInst) Send(_ context.Context, m conn.Message) (conn.Receipt, error) {
	i.f.mu.Lock()
	defer i.f.mu.Unlock()
	if i.f.fail != nil {
		return conn.Receipt{}, errors.New(i.f.fail.Error() + " token=" + i.cfg.Secrets["token"])
	}
	i.f.got = append(i.f.got, m)
	return conn.Receipt{Ref: "ref-" + m.Key}, nil
}
func (i *fakeInst) Test(context.Context) error { return nil }

type senv struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	q    *delivery.Queue
	cs   *connections.Service
	s    *channel.Sender
	fake *fakeChannel
	now  time.Time
}

func newSenv(t *testing.T) *senv {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, delivery.Module, connections.Module, secrets.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &senv{t: t, st: st, now: t0, fake: &fakeChannel{}}
	now := func() time.Time { return e.now }
	e.x = command.New(st, now)
	v := secrets.Generate()
	call, err := conn.New(conn.Options{Store: st, Vault: v, Plugins: []conn.Plugin{e.fake.plugin()}})
	if err != nil {
		t.Fatal(err)
	}
	e.cs = connections.New(v, call.Decl)
	if err := e.cs.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.q = delivery.New(delivery.Options{Now: now, Backoff: []time.Duration{time.Minute, time.Minute}, Deliverable: connections.Deliverable})
	e.s = &channel.Sender{Store: st, Queue: e.q, Conns: e.cs, Call: call, Base: "https://apa.example.invalid", Now: now}
	return e
}

func (e *senv) do(id command.Identity, cmd string, in any) (json.RawMessage, error) {
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b})
	return r.Value, err
}

func (e *senv) create(enabled bool) string {
	e.t.Helper()
	v, err := e.do(owner, connections.CmdCreate, map[string]any{"type": "fake", "name": "我的手机", "secrets": map[string]string{"token": secretVal}, "enabled": enabled})
	if err != nil {
		e.t.Fatal(err)
	}
	var c connections.Connection
	json.Unmarshal(v, &c)
	return c.ID
}

func (e *senv) enqueue(id, route string, content domain.DeliveryContent) domain.Delivery {
	e.t.Helper()
	var d domain.Delivery
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		d, _, err = e.q.Create(tx, domain.Delivery{ID: id, Item: "it-" + id, Class: domain.DeliveryDecision, Route: route, Version: 1, Content: content})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *senv) get(id string) (d domain.Delivery) {
	e.st.View(bg, func(tx *store.Tx) (err error) { d, err = e.q.Get(tx, id); return })
	return
}

func (e *senv) conn(id string) (c connections.Connection) {
	e.st.View(bg, func(tx *store.Tx) (err error) { c, err = e.cs.Get(tx, id); return })
	return
}

func (e *senv) drain() {
	e.t.Helper()
	if err := e.s.Drain(bg); err != nil {
		e.t.Fatal(err)
	}
}

// 入队的投递被发出并转已送达；web_only 的中立消息没有选项、推荐与表单链接；链接补成绝对地址。
func TestSenderDeliversAndKeepsWebOnlyBare(t *testing.T) {
	e := newSenv(t)
	id := e.create(true)
	route := delivery.ChannelRoute(id)
	e.enqueue("d1", route, domain.DeliveryContent{Title: "要不要合并", Options: []string{"合并", "等等"}, Recommend: "合并", WebURL: "/items/it-d1", FormURL: "/forms/x"})
	e.enqueue("d2", route, domain.DeliveryContent{Title: "高风险", WebURL: "/items/it-d2", WebOnly: true})
	e.drain()
	if d := e.get("d1"); d.State != domain.DeliveryDelivered || d.ExternalRef != "ref-d1@1" {
		t.Fatalf("d1 = %s ref %q", d.State, d.ExternalRef)
	}
	if len(e.fake.got) != 2 {
		t.Fatalf("sent %d", len(e.fake.got))
	}
	m1, m2 := e.fake.got[0], e.fake.got[1]
	if len(m1.Options) != 2 || m1.WebURL != "https://apa.example.invalid/items/it-d1" || m1.FormURL != "https://apa.example.invalid/forms/x" {
		t.Fatalf("m1 = %+v", m1)
	}
	if !m2.WebOnly || len(m2.Options) != 0 || m2.Recommend != "" || m2.FormURL != "" || m2.WebURL == "" {
		t.Fatalf("web_only message = %+v", m2)
	}
	if c := e.conn(id); c.State != connections.StateOK || c.LastOKAt == nil {
		t.Fatalf("conn = %+v", c)
	}
}

// 失败按退避表重试，超限转 Web 未读；连接显示问题，报错里没有凭据明文。
func TestSenderBacksOffThenFallsBackToWeb(t *testing.T) {
	e := newSenv(t)
	id := e.create(true)
	e.fake.fail = conn.Fail(conn.ProblemAuthFailed, "401")
	e.enqueue("d1", delivery.ChannelRoute(id), domain.DeliveryContent{Title: "x"})
	e.drain()
	if d := e.get("d1"); d.State != domain.DeliveryPending || d.Attempts != 1 {
		t.Fatalf("after 1st failure: %s attempts %d", d.State, d.Attempts)
	}
	e.drain() // 退避中：不领取
	if d := e.get("d1"); d.Attempts != 1 {
		t.Fatalf("claimed during backoff: attempts %d", d.Attempts)
	}
	for range 2 {
		e.now = e.now.Add(time.Minute)
		e.drain()
	}
	if d := e.get("d1"); d.State != domain.DeliveryFallbackWeb {
		t.Fatalf("after retries: %s", d.State)
	}
	c := e.conn(id)
	if c.State != connections.StateProblem || strings.Contains(c.Detail, secretVal) {
		t.Fatalf("conn = %+v", c)
	}
	var lastErr string
	e.st.View(bg, func(tx *store.Tx) error {
		r, _ := tx.Query(`SELECT last_error FROM delivery_delivery WHERE id = 'd1'`)
		defer r.Close()
		r.Next()
		return r.Scan(&lastErr)
	})
	if lastErr == "" || strings.Contains(lastErr, secretVal) {
		t.Fatalf("last_error = %q", lastErr)
	}
}

// 版本已作废的投递不发；停用的连接其路由直接落 Web 未读，发送器也不领取。
func TestSenderSkipsSupersededAndDisabled(t *testing.T) {
	e := newSenv(t)
	id := e.create(true)
	e.enqueue("d1", delivery.ChannelRoute(id), domain.DeliveryContent{Title: "x"})
	e.st.Update(bg, func(tx *store.Tx) error { _, err := e.q.Supersede(tx, "it-d1", delivery.AllVersions); return err })
	e.drain()
	if len(e.fake.got) != 0 {
		t.Fatalf("superseded delivery sent")
	}
	if _, err := e.do(owner, connections.CmdDisable, map[string]string{"id": id}); err != nil {
		t.Fatal(err)
	}
	d := e.enqueue("d2", delivery.ChannelRoute(id), domain.DeliveryContent{Title: "y"})
	if d.State != domain.DeliveryFallbackWeb || d.Route != delivery.RouteWeb {
		t.Fatalf("disabled route: %s %s", d.State, d.Route)
	}
	// 删除后同样落 Web 未读
	if _, err := e.do(owner, connections.CmdRemove, map[string]string{"id": id}); err != nil {
		t.Fatal(err)
	}
	if d := e.enqueue("d3", delivery.ChannelRoute(id), domain.DeliveryContent{Title: "z"}); d.State != domain.DeliveryFallbackWeb {
		t.Fatalf("removed route: %s", d.State)
	}
	e.drain()
	if len(e.fake.got) != 0 {
		t.Fatal("sent on a disabled connection")
	}
}

// 连接命令是 3b：任何钥匙都得硬线 web_only（退出码 20）。读接口与 Event 里没有凭据明文。
func TestConnectionCommandsAreWebOnlyAndNeverEchoSecrets(t *testing.T) {
	e := newSenv(t)
	delegate := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	in := map[string]any{"type": "fake", "secrets": map[string]string{"token": secretVal}}
	if _, err := e.do(delegate, connections.CmdCreate, in); exitOf(err) != 20 {
		t.Fatalf("delegate create: %v", err)
	}
	if _, err := e.do(worker, connections.CmdCreate, in); exitOf(err) != 20 {
		t.Fatalf("worker create: %v", err)
	}
	id := e.create(false)
	if _, err := e.do(owner, connections.CmdConfigure, map[string]any{"id": id, "secrets": map[string]string{"token": secretVal + "-2"}}); err != nil {
		t.Fatal(err)
	}
	var all []connections.Connection
	var evs []eventlog.Entry
	e.st.View(bg, func(tx *store.Tx) (err error) {
		if all, err = e.cs.List(tx, ""); err != nil {
			return err
		}
		evs, err = eventlog.Query(tx, eventlog.Filter{}, 0, 1000)
		return err
	})
	b, _ := json.Marshal(all)
	eb, _ := json.Marshal(evs)
	if strings.Contains(string(b), secretVal) || strings.Contains(string(eb), secretVal) {
		t.Fatalf("plaintext leaked: %s / %s", b, eb)
	}
	if !strings.Contains(string(b), `"token":true`) || !strings.Contains(string(eb), `"secrets_changed":["token"]`) {
		t.Fatalf("secret presence not reported: %s / %s", b, eb)
	}
	// 删除只在停用后
	e.do(owner, connections.CmdEnable, map[string]string{"id": id})
	if _, err := e.do(owner, connections.CmdRemove, map[string]string{"id": id}); exitOf(err) == 0 {
		t.Fatal("removed an enabled connection")
	}
}

func exitOf(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}
