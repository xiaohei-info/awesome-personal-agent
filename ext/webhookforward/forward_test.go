package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKernel 是测试边界内的内核替身，只回 /v1/events，记录请求方法与鉴权。
type fakeKernel struct {
	mu     sync.Mutex
	events []map[string]any
	reqs   []string
	auths  []string
	status int
}

func (k *fakeKernel) add(seq int, typ, obj string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.events = append(k.events, map[string]any{"seq": seq, "type": typ, "object": obj, "title": "t", "at": "2026-01-01T00:00:00Z", "actor": map[string]any{}})
}

func (k *fakeKernel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reqs = append(k.reqs, r.Method+" "+r.URL.Path)
	k.auths = append(k.auths, r.Header.Get("Authorization"))
	if k.status != 0 {
		w.WriteHeader(k.status)
		return
	}
	cur, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	out, next := []map[string]any{}, cur
	for _, e := range k.events {
		if s := e["seq"].(int); s > cur {
			out = append(out, e)
			next = s
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"events": out, "next": next})
}

type delivery struct {
	id, sig, ts string
	body        []byte
	code        int
}

// receiver 是外部系统替身：先返回 fail 次 500，再 200；记录每次收到的 POST。
type receiver struct {
	mu   sync.Mutex
	fail int
	got  []delivery
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	b := make([]byte, r.ContentLength)
	r.Body.Read(b)
	d := delivery{id: r.Header.Get("X-APA-Event-Id"), sig: r.Header.Get("X-APA-Signature"), ts: r.Header.Get("X-APA-Timestamp"), body: b, code: 200}
	if r.Method != http.MethodPost {
		d.code = 405
	} else if rc.fail > 0 {
		rc.fail--
		d.code = 500
	}
	rc.got = append(rc.got, d)
	w.WriteHeader(d.code)
}

func (rc *receiver) snapshot() []delivery {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]delivery(nil), rc.got...)
}

func (rc *receiver) okIDs() (ids []string) {
	for _, d := range rc.snapshot() {
		if d.code == 200 {
			ids = append(ids, d.id)
		}
	}
	return
}

const secret = "s3cret"

func sign(ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + string(body)))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func setup(t *testing.T, fail int) (*fakeKernel, *receiver, Config) {
	t.Helper()
	k, rc := &fakeKernel{}, &receiver{fail: fail}
	ks, rs := httptest.NewServer(k), httptest.NewServer(rc)
	t.Cleanup(ks.Close)
	t.Cleanup(rs.Close)
	return k, rc, Config{URL: ks.URL, Key: "extkey", Target: rs.URL, Secret: secret, State: filepath.Join(t.TempDir(), "cursor"),
		Wait: 0, Idle: 5 * time.Millisecond, Backoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
}

func runBG(t *testing.T, cfg Config) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	return func() error { cancel(); return <-done }
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// flow-export-ext / ACT-EXP-03：接收方收到签名有效的 POST；500 时退避重试，同一事件 id 多次到达且不变，顺序不乱。
func TestSignedPostRetriesOn500WithStableEventID(t *testing.T) {
	k, rc, cfg := setup(t, 3)
	k.add(1, "item.created", "itm_a")
	k.add(2, "item.phase_changed", "itm_a")
	stop := runBG(t, cfg)
	waitFor(t, "both events acknowledged", func() bool { return len(rc.okIDs()) == 2 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	got := rc.snapshot()
	if len(got) != 5 {
		t.Fatalf("want 3 failed + 2 ok deliveries, got %d", len(got))
	}
	for i, want := range []string{"1", "1", "1", "1", "2"} {
		d := got[i]
		if d.id != want {
			t.Fatalf("delivery %d id=%s want %s", i, d.id, want)
		}
		if d.sig != sign(d.ts, d.body) {
			t.Fatalf("delivery %d signature invalid: %s", i, d.sig)
		}
		var env map[string]any
		if err := json.Unmarshal(d.body, &env); err != nil || fmt.Sprint(env["seq"]) != d.id || env["type"] == nil {
			t.Fatalf("body is not the event envelope: %s", d.body)
		}
	}
	if got[0].code != 500 || got[3].code != 200 {
		t.Fatalf("codes: %+v", got)
	}
}

func TestCursorPersistsAndResumes(t *testing.T) {
	k, rc, cfg := setup(t, 0)
	k.add(1, "item.created", "a")
	k.add(2, "item.created", "b")
	stop := runBG(t, cfg)
	waitFor(t, "2 events", func() bool { return len(rc.okIDs()) == 2 })
	stop()
	if b, _ := os.ReadFile(cfg.State); strings.TrimSpace(string(b)) != "2" {
		t.Fatalf("cursor file = %q", b)
	}
	k.add(3, "item.created", "c")
	stop = runBG(t, cfg)
	waitFor(t, "event 3", func() bool { return len(rc.okIDs()) == 3 })
	stop()
	if ids := rc.okIDs(); strings.Join(ids, ",") != "1,2,3" {
		t.Fatalf("resume redelivered or skipped: %v", ids)
	}
}

// 重试期间进程退出：游标不推进，重启后该事件重新送出（至少一次）。
func TestCursorHoldsWhileReceiverDown(t *testing.T) {
	k, rc, cfg := setup(t, 1000)
	k.add(1, "item.created", "a")
	stop := runBG(t, cfg)
	waitFor(t, "several attempts", func() bool { return len(rc.snapshot()) >= 3 })
	stop()
	if _, err := os.Stat(cfg.State); err == nil {
		t.Fatal("cursor must not advance before the receiver acknowledges")
	}
	rc.mu.Lock()
	rc.fail = 0
	rc.mu.Unlock()
	stop = runBG(t, cfg)
	waitFor(t, "delivery after recovery", func() bool { return len(rc.okIDs()) == 1 })
	stop()
}

func TestRevokedKeyIsFatalAndKernelOnlyGetsReads(t *testing.T) {
	k, _, cfg := setup(t, 0)
	k.status = 401
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want fatal 401, got %v", err)
	}
}

// flow-export-ext / ACT-EXP-04：外发都由 ext 发起——ext 对内核只发带密钥的 GET /v1/events，
// 红线检查保证内核代码没有网络外呼能力。
func TestOutboundOnlyFromExtAndKernelRedline(t *testing.T) {
	k, rc, cfg := setup(t, 0)
	k.add(1, "item.created", "a")
	stop := runBG(t, cfg)
	waitFor(t, "delivery", func() bool { return len(rc.okIDs()) == 1 })
	stop()
	k.mu.Lock()
	for i, r := range k.reqs {
		if r != "GET /v1/events" || k.auths[i] != "Bearer extkey" {
			t.Fatalf("unexpected kernel request %q auth %q", r, k.auths[i])
		}
	}
	k.mu.Unlock()
	cmd := exec.Command("go", "run", "./tools/redline")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("redline failed: %v\n%s", err, out)
	}
}

func TestConfigFromEnv(t *testing.T) {
	full := map[string]string{"APA_URL": "u", "APA_EXT_KEY": "k", "APA_WEBHOOK_URL": "w", "APA_WEBHOOK_SECRET": "s", "APA_WEBHOOK_STATE": "/x"}
	get := func(m map[string]string) func(string) string { return func(s string) string { return m[s] } }
	if _, err := ConfigFromEnv(get(full)); err != nil {
		t.Fatal(err)
	}
	for name := range full {
		m := map[string]string{}
		for k, v := range full {
			if k != name {
				m[k] = v
			}
		}
		if _, err := ConfigFromEnv(get(m)); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestCorruptCursorRefusesToStart(t *testing.T) {
	_, _, cfg := setup(t, 0)
	os.WriteFile(cfg.State, []byte("junk"), 0o644)
	if err := Run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("got %v", err)
	}
}

// 接收方回 3xx 不是确认：不跟随重定向，游标不前进，终点收不到任何请求。
func TestRedirectIsNotAnAcknowledgement(t *testing.T) {
	k, _, cfg := setup(t, 0)
	var hits int
	var mu sync.Mutex
	end := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer end.Close()
	var tries int
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tries++
		mu.Unlock()
		http.Redirect(w, r, end.URL, http.StatusFound)
	}))
	defer redir.Close()
	cfg.Target = redir.URL
	k.add(1, "item.created", "a")
	stop := runBG(t, cfg)
	waitFor(t, "repeated attempts", func() bool { mu.Lock(); defer mu.Unlock(); return tries >= 3 })
	stop()
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("redirect target was contacted %d times", hits)
	}
	if _, err := os.Stat(cfg.State); err == nil {
		t.Fatal("cursor advanced on a redirect")
	}
}

// ACT-PRH-18：只订阅 item.created 时，proposal.attached 与 item.progress_added 不投递，但游标越过它们，
// 之后的 item.created 照常送出；不配类型时两类新事件与其他类型一样送出。
func TestTypeSubscriptionSkipsUnselectedAndAdvancesCursor(t *testing.T) {
	k, rc, cfg := setup(t, 0)
	cfg.Types = []string{"item.created"}
	k.add(1, "item.created", "a")
	k.add(2, "proposal.attached", "t3")
	k.add(3, "item.progress_added", "t3")
	stop := runBG(t, cfg)
	waitFor(t, "cursor past the skipped events", func() bool {
		b, _ := os.ReadFile(cfg.State)
		return strings.TrimSpace(string(b)) == "3"
	})
	k.add(4, "item.created", "b")
	waitFor(t, "later item.created", func() bool { return len(rc.okIDs()) == 2 })
	stop()
	if got := strings.Join(rc.okIDs(), ","); got != "1,4" {
		t.Fatalf("delivered %q, want only the item.created events 1,4", got)
	}

	k2, rc2, cfg2 := setup(t, 0)
	k2.add(1, "proposal.attached", "t3")
	k2.add(2, "item.progress_added", "t3")
	stop = runBG(t, cfg2)
	waitFor(t, "default forwards every type", func() bool { return len(rc2.okIDs()) == 2 })
	stop()
}

func TestConfigTypesFromEnv(t *testing.T) {
	env := map[string]string{"APA_URL": "u", "APA_EXT_KEY": "k", "APA_WEBHOOK_URL": "w", "APA_WEBHOOK_SECRET": "s", "APA_WEBHOOK_STATE": "/x",
		"APA_WEBHOOK_TYPES": " item.created, ,proposal.attached"}
	c, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || strings.Join(c.Types, ",") != "item.created,proposal.attached" {
		t.Fatalf("%+v %v", c, err)
	}
}
