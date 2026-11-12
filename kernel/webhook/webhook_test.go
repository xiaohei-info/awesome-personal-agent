package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const secret = "s3cret"

type fakePacks struct{ p *packs.Pack }

func (f *fakePacks) List() []*packs.Pack { return []*packs.Pack{f.p} }

type fakeSecrets struct{ keys [][]byte }

func (f fakeSecrets) Secrets(*store.Tx, string) ([][]byte, error) { return f.keys, nil }
func (fakeSecrets) Keep(*store.Tx, string, string) error          { return nil }

// failJudge：本测试里规则直接映射 kind，判断型不应被调用。
type failJudge struct{ t *testing.T }

func (j failJudge) Judge(context.Context, direct.JudgeCall) (direct.Verdict, error) {
	j.t.Error("judge must not be called")
	return direct.Verdict{}, nil
}

func (j failJudge) Do(context.Context, direct.Call) (direct.Outcome, error) {
	j.t.Error("judge must not be called")
	return direct.Outcome{}, nil
}

func scheme() []packs.WebhookScheme {
	return []packs.WebhookScheme{
		{ID: "hmac", SignatureHeader: "X-Sig-256", SignaturePrefix: "sha256=", Algorithm: "hmac-sha256", SignedPayload: "body", DeliveryIDHeader: "X-Delivery"},
		{ID: "stamped", SignatureHeader: "X-Sig", TimestampHeader: "X-Ts", Algorithm: "hmac-sha256", SignedPayload: "v0:{timestamp}:{body}",
			Handshake: &packs.Handshake{When: map[string]any{"json_path": "$.type", "equals": "url_verification"}, Respond: map[string]any{"json_path": "$.challenge"}}},
		{ID: "shared", SignatureHeader: "Authorization", SignaturePrefix: "Bearer ", Algorithm: "token"},
	}
}

func mappings() []packs.WebhookMapping {
	return []packs.WebhookMapping{{
		ID: "issues", Match: map[string]any{"header": map[string]any{"X-Event": "issues"}},
		Fields: map[string]any{"id": "{header.X-Delivery}", "type": "issue.{$.action}", "subject": "{$.issue.title}", "summary": "{$.issue.body}",
			"link": "{$.issue.html_url}", "occurred_at": "{$.issue.updated_at}", "sender": "{$.sender.login}",
			"correlation_keys": []any{"{$.repo.full_name}#{$.issue.number}"}},
		SenderVerified: "on_signature",
	}}
}

type env struct {
	t     *testing.T
	st    *store.Store
	x     *command.Executor
	svc   *Service
	auth  *auth.Auth
	pk    *fakePacks
	disp  *proc.Dispatcher
	in    *inbound.Service
	dir   string
	now   time.Time
	errs  chan error
	done  chan proc.Result
	keys  fakeSecrets
	count int
}

func (e *env) Now() time.Time { return e.now }

func newEnv(t *testing.T, mut func(*Options)) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module,
		proc.Module, delivery.Module, registry.Module, runner.Module, rules.Module, direct.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, dir: t.TempDir(), now: t0, errs: make(chan error, 32), done: make(chan proc.Result, 32), keys: fakeSecrets{[][]byte{[]byte(secret)}}}
	e.pk = &fakePacks{&packs.Pack{Name: "hooks", WebhookSchemes: scheme(), WebhookMaps: mappings(),
		SourceRules: []packs.SourceRule{{ID: "any", Match: map[string]any{"source_type": "forge"}, Kind: "task", Then: "proposed"}}}}
	e.x = command.New(st, e.Now)
	e.auth = auth.New(e.Now)
	lay := config.LayoutOf(filepath.Join(e.dir, "home"))
	if err := lay.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{projects.Register(e.x), items.Register(e.x, items.NewKinds()), registry.Register(e.x, registry.Options{Layout: lay})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	rn, err := runner.New(runner.Options{Store: st, Exec: e.x, Layout: lay, Now: e.Now,
		OnError: func(err error) { e.errs <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if err := rn.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.in, err = inbound.New(inbound.Options{Store: st, Exec: e.x, Packs: e.pk, Judge: failJudge{t}, Owner: "owner", OnError: func(err error) { e.errs <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.in.Register(e.x); err != nil {
		t.Fatal(err)
	}
	o := Options{Store: st, Exec: e.x, Packs: e.pk, Auth: e.auth, Secrets: e.keys, Now: e.Now, ScriptRoot: e.dir,
		OnError: func(err error) { e.errs <- err }}
	if mut != nil {
		mut(&o)
	}
	e.svc, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.disp = proc.New(st, config.Config{MaxSubprocesses: 2, MaxShortSubprocesses: 2}, proc.Options{Now: e.Now, OnResult: func(res proc.Result) {
		if err := e.svc.Handle(bg, res); err != nil {
			e.errs <- err
		}
		e.done <- res
	}})
	t.Cleanup(e.disp.Close)
	e.svc.Attach(e.disp)
	t.Cleanup(e.svc.Wait)
	return e
}

type m = map[string]any

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (e *env) register(id string, cfg m) {
	e.t.Helper()
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: registry.CmdRegister, Owner: "owner", Evidence: "x",
		Input: js(m{"id": id, "type": "webhook", "source_type": "forge", "config": cfg})}); err != nil {
		e.t.Fatal(err)
	}
}

func sign(key, body string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

func issueBody(n int, action string) string {
	return string(js(m{"action": action, "issue": m{"number": n, "title": "bug " + strconv.Itoa(n), "body": "details", "html_url": "https://forge.example/o/r/issues/" + strconv.Itoa(n),
		"updated_at": "2026-03-01T00:00:05Z"}, "repo": m{"full_name": "o/r"}, "sender": m{"login": "alice"}}))
}

func (e *env) post(src, delivery, body string, extra map[string]string) Response {
	h := map[string]string{"X-Event": "issues", "X-Sig-256": "sha256=" + sign(secret, body)}
	if delivery != "" {
		h["X-Delivery"] = delivery
	}
	for k, v := range extra {
		h[k] = v
	}
	r := e.svc.Receive(bg, Request{SourceID: src, Header: h, Body: []byte(body)})
	e.svc.Wait()
	e.noErr()
	return r
}

func (e *env) noErr() {
	e.t.Helper()
	select {
	case err := <-e.errs:
		e.t.Fatalf("async error: %v", err)
	default:
	}
}

func (e *env) ingests() (out []Ingest) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = Ingests(tx, ""); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) events(src string) (out []domain.SourceEvent) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = runner.Events(tx, src); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) itemCount() (n int) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		its, err := items.List(tx, items.Filter{})
		n = len(its)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func status(t *testing.T, r Response, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status = %d (%s), want %d", r.Status, r.Body, want)
	}
}

// ACT-HOOK-09：验签通过的请求先落盘再回 2xx；事件经转换进入与拉取型同一条流水线，成为 proposed 事项。
func TestSignedDeliveryIsPersistedThenReachesPipeline(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	body := issueBody(7, "opened")
	status(t, e.post("src_hook", "d-1", body, nil), 200)

	gs := e.ingests()
	if len(gs) != 1 || gs[0].State != StateProcessed || string(gs[0].Body) != body || !gs[0].Signed || gs[0].Events != 1 {
		t.Fatalf("ingest = %+v", gs)
	}
	if _, leaked := gs[0].Headers["authorization"]; leaked {
		t.Fatal("credentials must not be stored")
	}
	evs := e.events("src_hook")
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	ev := evs[0]
	if ev.Type != "issue.opened" || ev.Subject != "bug 7" || ev.Sender != "alice" || !ev.SenderVerified ||
		ev.DedupeKey.EventID != "d-1" || len(ev.CorrelationKeys) != 1 || ev.CorrelationKeys[0] != "o/r#7" || ev.Link == "" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Disposition != domain.DispositionProposed || ev.Item == "" || e.itemCount() != 1 {
		t.Fatalf("pipeline did not propose an item: %+v items=%d", ev, e.itemCount())
	}
}

// ACT-HOOK-09：同一投递 id 重复送达只落一条记录、一条事件、一个事项；没有投递 id 时用 payload 哈希。
func TestDeliveryIsIdempotentByIDOrPayloadHash(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	body := issueBody(1, "opened")
	first := e.post("src_hook", "d-1", body, nil)
	again := e.post("src_hook", "d-1", body, nil)
	status(t, first, 200)
	status(t, again, 200)
	if !strings.Contains(string(again.Body), `"duplicate":true`) || strings.Contains(string(first.Body), `"duplicate":true`) {
		t.Fatalf("duplicate flag: first=%s again=%s", first.Body, again.Body)
	}
	if len(e.ingests()) != 1 || len(e.events("src_hook")) != 1 || e.itemCount() != 1 {
		t.Fatalf("ingests=%d events=%d items=%d", len(e.ingests()), len(e.events("src_hook")), e.itemCount())
	}
	// 无投递 id：同一 payload 重发算同一投递，不同 payload 是新投递。
	e.post("src_hook", "", issueBody(2, "opened"), nil)
	e.post("src_hook", "", issueBody(2, "opened"), nil)
	e.post("src_hook", "", issueBody(3, "opened"), nil)
	if got := len(e.ingests()); got != 3 {
		t.Fatalf("ingests = %d, want 3", got)
	}
	for _, g := range e.ingests()[1:] {
		if !strings.HasPrefix(g.DeliveryKey, "sha256:") {
			t.Fatalf("delivery key = %q", g.DeliveryKey)
		}
	}
}

// ACT-HOOK-09：校验失败回 401，只记元数据（无请求体、无事件）；缺头、错密钥、过期时间戳都算失败。
func TestVerificationFailuresReturn401AndStoreNoBody(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	e.register("src_stamped", m{"scheme": "stamped", "mappings": []string{"issues"}})
	body := issueBody(1, "opened")

	status(t, e.svc.Receive(bg, Request{SourceID: "src_hook", Body: []byte(body)}), 401) // 缺签名头
	status(t, e.post("src_hook", "d", body, map[string]string{"X-Sig-256": "sha256=" + sign("wrong", body)}), 401)
	status(t, e.post("src_hook", "d", body, map[string]string{"X-Sig-256": "sha256=zz"}), 401)
	status(t, e.post("src_hook", "d", body+" ", map[string]string{"X-Sig-256": "sha256=" + sign(secret, body)}), 401) // 签名对不上改动过的体

	stale := strconv.FormatInt(t0.Add(-time.Hour).Unix(), 10)
	status(t, e.svc.Receive(bg, Request{SourceID: "src_stamped", Body: []byte(body),
		Header: map[string]string{"X-Ts": stale, "X-Sig": sign(secret, "v0:"+stale+":"+body)}}), 401)

	gs := e.ingests()
	if len(gs) != 5 || len(e.events("src_hook")) != 0 || e.itemCount() != 0 {
		t.Fatalf("ingests=%d", len(gs))
	}
	for _, g := range gs {
		if g.State != StateRejected || len(g.Body) != 0 || g.Reason == "" || g.BodySize == 0 && g.Reason == "" {
			t.Fatalf("rejected record = %+v", g)
		}
	}
	// 被拒绝的投递不占用投递键：随后合法的同 id 请求正常接收。
	status(t, e.post("src_hook", "d", body, nil), 200)
}

// 未知或停用的来源回 404；超大体回 413；超频回 429。
func TestUnknownSourceBodyLimitAndRateLimit(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MaxBody, o.RatePerMin = 64, 2 })
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	status(t, e.svc.Receive(bg, Request{SourceID: "src_nope", Body: []byte("{}")}), 404)
	status(t, e.post("src_hook", "d", strings.Repeat("x", 65), nil), 413)
	small := `{"a":1}`
	status(t, e.post("src_hook", "d1", small, nil), 200) // 第 2 次请求（413 也计数）
	status(t, e.post("src_hook", "d2", small, nil), 429)
	e.now = e.now.Add(time.Minute)
	status(t, e.post("src_hook", "d2", small, nil), 200)

	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: registry.CmdSetEnabled, Owner: "owner", Evidence: "x", Input: js(m{"id": "src_hook", "enabled": false})}); err != nil {
		t.Fatal(err)
	}
	status(t, e.post("src_hook", "d3", small, nil), 404)
}

// 校验方案里的握手由方案同步应答，不落盘、不入事件。
func TestHandshakeAnsweredSynchronously(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_stamped", m{"scheme": "stamped", "mappings": []string{"issues"}})
	body := `{"type":"url_verification","challenge":"abc123"}`
	ts := strconv.FormatInt(t0.Unix(), 10)
	r := e.svc.Receive(bg, Request{SourceID: "src_stamped", Body: []byte(body), Header: map[string]string{"x-ts": ts, "X-Sig": sign(secret, "v0:"+ts+":"+body)}})
	status(t, r, 200)
	if string(r.Body) != "abc123" || len(e.ingests()) != 0 {
		t.Fatalf("body=%q ingests=%d", r.Body, len(e.ingests()))
	}
	// 握手不绕过校验。
	status(t, e.svc.Receive(bg, Request{SourceID: "src_stamped", Body: []byte(body), Header: map[string]string{"x-ts": ts, "X-Sig": "00"}}), 401)
}

// 通用共享令牌：令牌是该来源的 webhook 凭据，轮换宽限期内新旧并存，过期后旧令牌失效，别的来源的令牌无效；令牌不算签名，发送者不算已核实。
func TestSharedTokenWithRotation(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_a", m{"scheme": "shared", "mappings": []string{"issues"}})
	e.register("src_b", m{"scheme": "shared", "mappings": []string{"issues"}})
	var oldTok, newTok, otherTok string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		c, s, err := e.auth.Issue(tx, auth.Spec{Kind: auth.Webhook, Scope: "src_a"})
		if err != nil {
			return err
		}
		oldTok = s
		_, newTok, err = e.auth.Rotate(tx, c.ID, time.Hour)
		if err != nil {
			return err
		}
		_, otherTok, err = e.auth.Issue(tx, auth.Spec{Kind: auth.Webhook, Scope: "src_b"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	bearer := func(tok string) map[string]string {
		return map[string]string{"X-Sig-256": "", "Authorization": "Bearer " + tok}
	}
	body := issueBody(1, "opened")
	post := func(src, tok, d string) Response {
		r := e.svc.Receive(bg, Request{SourceID: src, Header: map[string]string{"Authorization": bearer(tok)["Authorization"], "X-Event": "issues", "X-Delivery": d}, Body: []byte(body)})
		e.svc.Wait()
		return r
	}
	status(t, post("src_a", oldTok, "1"), 200)
	status(t, post("src_a", newTok, "2"), 200)
	status(t, post("src_a", otherTok, "3"), 401)
	status(t, post("src_a", "", "4"), 401)
	e.now = e.now.Add(2 * time.Hour)
	status(t, post("src_a", oldTok, "5"), 401)
	status(t, post("src_a", newTok, "6"), 200)
	for _, ev := range e.events("src_a") {
		if ev.SenderVerified {
			t.Fatalf("a shared token must not verify the sender: %+v", ev)
		}
	}
}

// ACT-HOOK-09：启动时重新处理已落盘但未处理的记录；转换到一半丢失的退回重做。重启后不重复。
func TestRecoverReprocessesPersistedUnprocessedRecords(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	body := issueBody(9, "opened")
	hj := `{"x-event":"issues","x-delivery":"d-crash"}`
	// 模拟落盘后、处理前崩溃（received），以及转换途中崩溃（transforming）。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for i, st := range []string{StateReceived, StateTransforming} {
			if _, err := tx.Exec(`INSERT INTO webhook_ingest (id, source_id, delivery_key, received_at, state, signed, gen, run_id, events, reason, headers, body, body_size, purged)
				VALUES (?,?,?,?,?,1,1,'run_lost',0,'',?,?,?,0)`, "ing_crash"+strconv.Itoa(i), "src_hook", "d-crash"+strconv.Itoa(i), t0.UnixNano(), st,
				strings.ReplaceAll(hj, "d-crash", "d-crash"+strconv.Itoa(i)), []byte(body), int64(len(body))); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Recover(bg); err != nil {
		t.Fatal(err)
	}
	for _, g := range e.ingests() {
		if g.State != StateProcessed || g.Events != 1 {
			t.Fatalf("after recover: %+v", g)
		}
	}
	if got := len(e.events("src_hook")); got != 2 {
		t.Fatalf("events = %d, want 2 (one per delivery)", got)
	}
	if err := e.svc.Recover(bg); err != nil || len(e.events("src_hook")) != 2 {
		t.Fatalf("second recover must be a no-op: %v", err)
	}
}

// ACT-HOOK-13：映射对不上的 payload 记 failed 并说明原因；修好映射后重放得到事件，再重放不产生重复事件或事项。
func TestReplayAfterFixingMappingDoesNotDuplicate(t *testing.T) {
	e := newEnv(t, nil)
	good := e.pk.p.WebhookMaps
	e.pk.p.WebhookMaps = []packs.WebhookMapping{{ID: "issues", Match: map[string]any{"header": map[string]any{"X-Event": "other"}}, Fields: good[0].Fields}}
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	status(t, e.post("src_hook", "d-1", issueBody(5, "opened"), nil), 200)
	g := e.ingests()[0]
	if g.State != StateFailed || !strings.Contains(g.Reason, "no mapping matched") || len(e.events("src_hook")) != 0 {
		t.Fatalf("ingest = %+v", g)
	}
	e.pk.p.WebhookMaps = good
	if err := e.svc.Replay(bg, g.ID); err != nil {
		t.Fatal(err)
	}
	if g = e.ingests()[0]; g.State != StateProcessed || g.Gen != 2 || len(e.events("src_hook")) != 1 || e.itemCount() != 1 {
		t.Fatalf("after replay: %+v events=%d items=%d", g, len(e.events("src_hook")), e.itemCount())
	}
	for range 2 {
		if err := e.svc.Replay(bg, g.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.events("src_hook")) != 1 || e.itemCount() != 1 {
		t.Fatalf("replay duplicated: events=%d items=%d", len(e.events("src_hook")), e.itemCount())
	}
	if err := e.svc.Replay(bg, "ing_nope"); err == nil {
		t.Fatal("unknown ingest must be an error")
	}
	// 重放绕过投递 id 幂等：同一投递 id 的再次送达仍只是重复，不会因此多一条记录。
	status(t, e.post("src_hook", "d-1", issueBody(5, "opened"), nil), 200)
	if len(e.ingests()) != 1 {
		t.Fatalf("ingests = %d", len(e.ingests()))
	}
}

func (e *env) script(name, body string) string {
	e.t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) waitRun() proc.Result {
	e.t.Helper()
	select {
	case r := <-e.done:
		return r
	case err := <-e.errs:
		e.t.Fatalf("async error: %v", err)
	case <-time.After(20 * time.Second):
		e.t.Fatal("timed out waiting for the transform run")
	}
	return proc.Result{}
}

// ACT-HOOK-13：特殊格式走转换脚本（stdin JSON → stdout 标准事件 JSONL），经外呼调用器；payload 只经 stdin 当资料，
// 内容里的 shell 元字符不会被执行；产出的事件进入同一流水线。
func TestTransformScriptProducesEventsFromPayloadAsData(t *testing.T) {
	e := newEnv(t, nil)
	marker := filepath.Join(e.dir, "pwned")
	cmd := e.script("xform.sh", `in=$(cat)
echo "$in" > `+filepath.Join(e.dir, "stdin.json")+`
echo '{"id":"x-1","type":"custom.created","subject":"from script","occurred_at":"2026-03-01T00:00:00Z","sender":"bob"}'
echo '{"id":"x-2","type":"custom.created","subject":"second","occurred_at":"2026-03-01T00:00:01Z"}'
`)
	e.register("src_hook", m{"scheme": "hmac", "transform": m{"command": cmd}})
	body := "$(touch " + marker + ") `touch " + marker + "` ; plain text, not json"
	status(t, e.post("src_hook", "d-1", body, nil), 200)
	e.waitRun()
	e.svc.Wait()
	e.noErr()

	if g := e.ingests()[0]; g.State != StateProcessed || g.Events != 2 {
		t.Fatalf("ingest = %+v", g)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("payload content was executed")
	}
	var stdin transformStdin
	b, _ := os.ReadFile(filepath.Join(e.dir, "stdin.json"))
	if err := json.Unmarshal(b, &stdin); err != nil || stdin.Body != body || stdin.SourceID != "src_hook" || stdin.Payload != nil {
		t.Fatalf("stdin = %s (%v)", b, err)
	}
	if got := e.transformExplicit(); len(got) != 1 || got[0] {
		t.Fatalf("webhook arrival must not be explicit: %v", got)
	}
	evs := e.events("src_hook")
	if len(evs) != 2 || evs[0].Subject != "from script" || evs[0].Disposition != domain.DispositionProposed || e.itemCount() != 2 {
		t.Fatalf("events = %+v items=%d", evs, e.itemCount())
	}
	// 重放：脚本再跑一次，事件去重，不产生重复。
	if err := e.svc.Replay(bg, e.ingests()[0].ID); err != nil {
		t.Fatal(err)
	}
	e.waitRun()
	e.svc.Wait()
	e.noErr()
	if len(e.events("src_hook")) != 2 || e.itemCount() != 2 || e.ingests()[0].Gen != 2 {
		t.Fatalf("replay duplicated: events=%d items=%d", len(e.events("src_hook")), e.itemCount())
	}
	if got := e.transformExplicit(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("only the replay run is explicit: %v", got)
	}
}

// transformExplicit 按入队顺序列出转换 Run 的 Explicit。
func (e *env) transformExplicit() (out []bool) {
	e.t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		rs, err := proc.List(tx)
		for _, r := range rs {
			if r.Run.Category == domain.RunTransform {
				out = append(out, r.Explicit)
			}
		}
		return err
	})
	return out
}

// 转换脚本的输出超过 64KB 时开头的事件也要入账；超过总上限记 failed，不入账半截结果。
func TestTransformLargeOutput(t *testing.T) {
	big := `i=0
while [ $i -lt 700 ]; do
  echo "{\"id\":\"x-$i\",\"type\":\"t\",\"subject\":\"s\",\"detail\":\"` + strings.Repeat("x", 120) + `\",\"occurred_at\":\"2026-03-01T00:00:00Z\"}"
  i=$((i+1))
done
`
	e := newEnv(t, nil)
	e.register("src_hook", m{"scheme": "hmac", "transform": m{"command": e.script("x.sh", big)}})
	status(t, e.post("src_hook", "d-1", `{"a":1}`, nil), 200)
	e.waitRun()
	e.svc.Wait()
	e.noErr()
	if g := e.ingests()[0]; g.State != StateProcessed || g.Events != 700 {
		t.Fatalf("ingest = %+v", g)
	}

	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO webhook_ingest (id, source_id, delivery_key, received_at, state, signed, gen, run_id, events, reason, headers, body, body_size, purged)
			VALUES ('ing_over','src_hook','d-over',?,?,1,1,'run_over',0,'','{}',?,7,0)`, t0.UnixNano(), StateTransforming, []byte(`{"a":1}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Handle(bg, proc.Result{Overflow: true, Stdout: []byte(`{"id":"x-over","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}` + "\n"),
		Run: domain.Run{ID: "run_over", Category: domain.RunTransform, Target: "ing_over", State: domain.RunSucceeded}}); err != nil {
		t.Fatal(err)
	}
	for _, g := range e.ingests() {
		if g.ID == "ing_over" && (g.State != StateFailed || !strings.Contains(g.Reason, "max_source_output_mb")) {
			t.Fatalf("ingest = %+v", g)
		}
	}
	if n := len(e.events("src_hook")); n != 700 {
		t.Fatalf("overflowed output must not be ingested: %d events", n)
	}
}

// 转换脚本退出码语义同拉取型：1 临时失败保持 received（启动恢复时重试）；2、3 记 failed；坏输出记 failed。
func TestTransformExitCodes(t *testing.T) {
	cases := []struct {
		name, script, state, reason string
	}{
		{"transient", `exit 1`, StateReceived, "transform failed"},
		{"needs-input", `exit 2`, StateFailed, "exit status 2"},
		{"permanent", `exit 3`, StateFailed, "exit status 3"},
		{"bad-output", `echo 'not json'`, StateFailed, "script output line 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.register("src_hook", m{"scheme": "hmac", "transform": m{"command": e.script("x.sh", c.script)}})
			status(t, e.post("src_hook", "d-1", `{"a":1}`, nil), 200)
			e.waitRun()
			e.svc.Wait()
			g := e.ingests()[0]
			if g.State != c.state || !strings.Contains(g.Reason, c.reason) || len(e.events("src_hook")) != 0 {
				t.Fatalf("ingest = %+v", g)
			}
		})
	}
}

// 转换脚本必须在宿主脚本目录之下：delegate 可登记的 config 不能借此执行任意命令。
func TestTransformOutsideScriptRootIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_hook", m{"scheme": "hmac", "transform": m{"command": "/bin/sh -c id"}})
	status(t, e.post("src_hook", "d-1", `{"a":1}`, nil), 200)
	if g := e.ingests()[0]; g.State != StateFailed || !strings.Contains(g.Reason, "script directory") {
		t.Fatalf("ingest = %+v", g)
	}
}

// 原始 payload 只保留保留期：过期后只剩元数据，不能再重放。
func TestExpireKeepsMetadataOnly(t *testing.T) {
	e := newEnv(t, nil)
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	status(t, e.post("src_hook", "d-1", issueBody(1, "opened"), nil), 200)
	e.now = e.now.Add(29 * 24 * time.Hour)
	status(t, e.post("src_hook", "d-2", issueBody(2, "opened"), nil), 200)
	e.now = e.now.Add(2 * 24 * time.Hour)
	if err := e.svc.Expire(bg); err != nil {
		t.Fatal(err)
	}
	gs := e.ingests()
	if !gs[0].Purged || len(gs[0].Body) != 0 || gs[0].BodySize == 0 || gs[0].State != StateProcessed || gs[1].Purged {
		t.Fatalf("ingests = %+v", gs)
	}
	if err := e.svc.Replay(bg, gs[0].ID); err == nil {
		t.Fatal("replaying an expired payload must fail")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing dependencies must be an error")
	}
}

// 映射模板：取不到的值展开为空；JSON 路径支持数组下标；坏时间与不认识的 match 条件是明确的错误。
func TestMappingTemplates(t *testing.T) {
	c := newExprCtx(map[string]string{"x-h": "hv"}, []byte(`{"a":{"b":[{"c":12345678901}, "s"]},"t":true,"n":null}`))
	for tpl, want := range map[string]string{"{$.a.b[0].c}": "12345678901", "{$.a.b[1]}": "s", "{header.X-H}/{$.t}": "hv/true", "{$.n}{$.zz}{$.a.b[9]}": "", "no braces": "no braces", "open {": "open {"} {
		if got := c.render(tpl); got != want {
			t.Errorf("render(%q) = %q, want %q", tpl, got, want)
		}
	}
	if _, err := c.matches(map[string]any{"bogus": 1}); err == nil {
		t.Error("unknown match condition must be an error")
	}
	bad := packs.WebhookMapping{ID: "b", Fields: map[string]any{"id": "x", "type": "t", "occurred_at": "yesterday"}}
	if _, err := mapEvent(bad, c, []byte("{}"), "k", t0, false); err == nil {
		t.Error("bad occurred_at must be an error")
	}
	ok := packs.WebhookMapping{ID: "o", Fields: map[string]any{"type": "t"}}
	ev, err := mapEvent(ok, c, []byte("{}"), "key-1", t0, false)
	if err != nil || ev.ID != "key-1" || ev.Subject != "t" || !ev.OccurredAt.Equal(t0) || ev.Detail != "{}" {
		t.Fatalf("defaults: %+v %v", ev, err)
	}
}

// 转换脚本瞬时失败（退出码 1）后记录留在 received，再次处理会用新的 Run 重试并成功产出事件。
func TestTransientTransformFailureIsRetriedWithNewRun(t *testing.T) {
	e := newEnv(t, nil)
	flag := filepath.Join(e.dir, "flag")
	cmd := e.script("flaky.sh", `cat >/dev/null
if [ ! -e `+flag+` ]; then touch `+flag+`; exit 1; fi
echo '{"id":"x-1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'
`)
	e.register("src_hook", m{"scheme": "hmac", "transform": m{"command": cmd}})
	status(t, e.post("src_hook", "d-1", `{"a":1}`, nil), 200)
	e.waitRun()
	e.svc.Wait()
	first := e.ingests()[0]
	if first.State != StateReceived || len(e.events("src_hook")) != 0 {
		t.Fatalf("after exit 1: %+v", first)
	}
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	e.waitRun()
	e.svc.Wait()
	e.noErr()
	if g := e.ingests()[0]; g.State != StateProcessed || g.Events != 1 || len(e.events("src_hook")) != 1 {
		t.Fatalf("after retry: %+v", g)
	}
}

// 转换到一半崩溃（记录是 transforming，旧 Run 已丢）：Recover 之后重新入队并完成转换。
func TestRecoverCompletesInterruptedTransform(t *testing.T) {
	e := newEnv(t, nil)
	cmd := e.script("ok.sh", `cat >/dev/null
echo '{"id":"x-1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'
`)
	e.register("src_hook", m{"scheme": "hmac", "transform": m{"command": cmd}})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO webhook_ingest (id, source_id, delivery_key, received_at, state, signed, gen, run_id, events, reason, headers, body, body_size, purged)
			VALUES ('ing_mid','src_hook','d-mid',?,?,1,1,'run_lost',0,'','{}',?,7,0)`, t0.UnixNano(), StateTransforming, []byte(`{"a":1}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Recover(bg); err != nil {
		t.Fatal(err)
	}
	e.waitRun()
	e.svc.Wait()
	e.noErr()
	if g := e.ingests()[0]; g.State != StateProcessed || len(e.events("src_hook")) != 1 {
		t.Fatalf("after recover: %+v", g)
	}
}

// 并发接收：每个已回 2xx 的记录最终都被处理，不会因处理循环的收尾竞争停在 received。
func TestConcurrentReceiveLeavesNothingUnprocessed(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.RatePerMin = 1 << 20 })
	e.register("src_hook", m{"scheme": "hmac", "mappings": []string{"issues"}})
	const n = 60
	errc := make(chan Response, n)
	for i := range n {
		go func() {
			body := issueBody(i, "opened")
			errc <- e.svc.Receive(bg, Request{SourceID: "src_hook", Body: []byte(body),
				Header: map[string]string{"X-Event": "issues", "X-Delivery": "d" + strconv.Itoa(i), "X-Sig-256": "sha256=" + sign(secret, body)}})
		}()
	}
	for range n {
		status(t, <-errc, 200)
	}
	e.svc.Wait()
	e.noErr()
	for _, g := range e.ingests() {
		if g.State != StateProcessed {
			t.Fatalf("stuck record: %+v", g)
		}
	}
	if got := len(e.ingests()); got != n {
		t.Fatalf("ingests = %d", got)
	}
}
