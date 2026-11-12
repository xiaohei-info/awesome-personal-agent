package ingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
	_ "modernc.org/sqlite"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const secret = "s3cret"

type fakePacks struct{ p *packs.Pack }

func (f *fakePacks) List() []*packs.Pack { return []*packs.Pack{f.p} }

type fakeSecrets struct{}

func (fakeSecrets) Secrets(*store.Tx, string) ([][]byte, error) { return [][]byte{[]byte(secret)}, nil }
func (fakeSecrets) Keep(*store.Tx, string, string) error        { return nil }

type failJudge struct{ t *testing.T }

func (j failJudge) Judge(context.Context, direct.JudgeCall) (direct.Verdict, error) {
	j.t.Error("judge must not be called")
	return direct.Verdict{}, nil
}

func (j failJudge) Do(context.Context, direct.Call) (direct.Outcome, error) {
	j.t.Error("judge must not be called")
	return direct.Outcome{}, nil
}

type m = map[string]any

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	svc  *webhook.Service
	auth *auth.Auth
	pk   *fakePacks
	srv  *core.Server
	lay  config.Layout
	in   *inbound.Service
}

func registerModules(t *testing.T) *store.Registry {
	reg := store.NewRegistry()
	for _, mod := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module,
		proc.Module, delivery.Module, registry.Module, runner.Module, rules.Module, direct.Module, webhook.Module, core.Module} {
		if err := reg.Register(mod); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func newEnv(t *testing.T, maxBody, rate int) *env {
	t.Helper()
	st, err := store.Open(t.TempDir(), registerModules(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := func() time.Time { return t0 }
	e := &env{t: t, st: st, auth: auth.New(now)}
	e.pk = &fakePacks{&packs.Pack{Name: "hooks",
		WebhookSchemes: []packs.WebhookScheme{
			{ID: "gh", SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256=", Algorithm: "hmac-sha256", SignedPayload: "body", DeliveryIDHeader: "X-GitHub-Delivery"},
			{ID: "slack", SignatureHeader: "X-Sig", TimestampHeader: "X-Ts", Algorithm: "hmac-sha256", SignedPayload: "v0:{timestamp}:{body}",
				Handshake: &packs.Handshake{When: map[string]any{"json_path": "$.type", "equals": "url_verification"}, Respond: map[string]any{"json_path": "$.challenge"}}},
			{ID: "shared", SignatureHeader: "X-Token", Algorithm: "token", DeliveryIDHeader: "X-GitHub-Delivery"},
		},
		WebhookMaps: []packs.WebhookMapping{{
			ID: "issues", Match: map[string]any{"header": map[string]any{"X-GitHub-Event": "issues"}},
			Fields: map[string]any{"id": "{header.X-GitHub-Delivery}", "type": "issue.{$.action}", "subject": "{$.issue.title}", "summary": "{$.issue.body}",
				"link": "{$.issue.html_url}", "occurred_at": "{$.issue.updated_at}", "sender": "{$.sender.login}",
				"correlation_keys": []any{"{$.repo.full_name}#{$.issue.number}"}},
			SenderVerified: "on_signature",
		}},
		SourceRules: []packs.SourceRule{{ID: "any", Match: map[string]any{"source_type": "forge"}, Kind: "task", Then: "proposed"}}}}
	e.x = command.New(st, now)
	e.lay = config.LayoutOf(filepath.Join(t.TempDir(), "home"))
	if err := e.lay.Ensure(); err != nil {
		t.Fatal(err)
	}
	e.wire(st, e.x)
	var opts []func(*webhook.Options)
	opts = append(opts, func(o *webhook.Options) { o.MaxBody, o.RatePerMin = maxBody, rate })
	e.svc = e.newService(st, e.x, opts...)
	e.serve(maxBody)
	return e
}

// wire 在 x 上登记来源、事项和接入流水线的命令。
func (e *env) wire(st *store.Store, x *command.Executor) {
	t := e.t
	for _, err := range []error{projects.Register(x), items.Register(x, items.NewKinds()), registry.Register(x, registry.Options{Layout: e.lay})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	rn, err := runner.New(runner.Options{Store: st, Exec: x, Layout: e.lay, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if err := rn.Register(x); err != nil {
		t.Fatal(err)
	}
	in, err := inbound.New(inbound.Options{Store: st, Exec: x, Packs: e.pk, Judge: failJudge{t}, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Register(x); err != nil {
		t.Fatal(err)
	}
}

func (e *env) newService(st *store.Store, x *command.Executor, mut ...func(*webhook.Options)) *webhook.Service {
	o := webhook.Options{Store: st, Exec: x, Packs: e.pk, Auth: e.auth, Secrets: fakeSecrets{}, Now: func() time.Time { return t0 }}
	for _, f := range mut {
		f(&o)
	}
	svc, err := webhook.New(o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(svc.Wait)
	return svc
}

func (e *env) serve(maxBody int) {
	srv, err := core.New(core.Deps{Store: e.st, Auth: e.auth, Groups: []core.RouteGroup{Routes(e.svc, int64(maxBody)), apiitems.Routes(e.x, e.st)}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv = srv
}

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

func issueBody(n int) string {
	return string(js(m{"action": "opened", "issue": m{"number": n, "title": "bug " + strconv.Itoa(n), "body": "details", "html_url": "https://forge.example/o/r/issues/" + strconv.Itoa(n),
		"updated_at": "2026-03-01T00:00:05Z"}, "repo": m{"full_name": "o/r"}, "sender": m{"login": "alice"}}))
}

func (e *env) do(method, path, body string, h map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	e.svc.Wait()
	return rec
}

// ghPost 发 GitHub 类签名请求；delivery 为空则不带投递 id。
func (e *env) ghPost(src, delivery, body, sig string) *httptest.ResponseRecorder {
	h := map[string]string{"X-GitHub-Event": "issues", "X-Hub-Signature-256": "sha256=" + sig}
	if delivery != "" {
		h["X-GitHub-Delivery"] = delivery
	}
	return e.do("POST", "/ingest/"+src, body, h)
}

func (e *env) ingests() (out []webhook.Ingest) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = webhook.Ingests(tx, ""); return }); err != nil {
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

func code(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body, want)
	}
}

// ACT-HOOK-02：签名正确的请求落盘后回 2xx，原始 payload 与校验结果可查，事件进入同一流水线生成 proposed。
func TestSignedPostIsPersistedAndReachesPipeline(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_hook", m{"scheme": "gh", "mappings": []string{"issues"}})
	body := issueBody(7)
	code(t, e.ghPost("src_hook", "d-1", body, sign(secret, body)), 200)

	gs := e.ingests()
	if len(gs) != 1 || gs[0].State != webhook.StateProcessed || string(gs[0].Body) != body || !gs[0].Signed || gs[0].Events != 1 {
		t.Fatalf("ingests = %+v", gs)
	}
	evs := e.events("src_hook")
	if len(evs) != 1 || evs[0].Disposition != domain.DispositionProposed || evs[0].Item == "" || e.itemCount() != 1 {
		t.Fatalf("pipeline did not propose an item: %+v", evs)
	}
}

// ACT-HOOK-03：签名错误回 401，入站记录只有元数据、没有请求体，不产生事件。
func TestBadSignatureIs401WithoutStoringBody(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_hook", m{"scheme": "gh", "mappings": []string{"issues"}})
	body := issueBody(7)
	code(t, e.ghPost("src_hook", "d-2", body, sign("wrong", body)), 401)
	code(t, e.do("POST", "/ingest/src_hook", body, nil), 401) // 缺签名头

	gs := e.ingests()
	if len(gs) != 2 || len(e.events("src_hook")) != 0 || e.itemCount() != 0 {
		t.Fatalf("ingests=%d", len(gs))
	}
	for _, g := range gs {
		if g.State != webhook.StateRejected || len(g.Body) != 0 || g.Reason == "" {
			t.Fatalf("rejected record = %+v", g)
		}
	}
}

// ACT-HOOK-04：同一投递 id 重发回 2xx 且不重复；没有投递 id 时同 payload 按哈希幂等。
func TestRedeliveryIsIdempotent(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_hook", m{"scheme": "gh", "mappings": []string{"issues"}})
	body := issueBody(1)
	code(t, e.ghPost("src_hook", "d-1", body, sign(secret, body)), 200)
	again := e.ghPost("src_hook", "d-1", body, sign(secret, body))
	code(t, again, 200)
	if !strings.Contains(again.Body.String(), `"duplicate":true`) {
		t.Fatalf("body = %s", again.Body)
	}
	other := issueBody(2)
	code(t, e.ghPost("src_hook", "", other, sign(secret, other)), 200)
	code(t, e.ghPost("src_hook", "", other, sign(secret, other)), 200)
	if len(e.ingests()) != 2 || len(e.events("src_hook")) != 2 || e.itemCount() != 2 {
		t.Fatalf("ingests=%d events=%d items=%d", len(e.ingests()), len(e.events("src_hook")), e.itemCount())
	}
}

// ACT-HOOK-05：已回 2xx 的事件在处理前崩溃也不丢——记录已落盘，重启后 Recover 处理完成。
func TestAcknowledgedEventSurvivesCrashBeforeProcessing(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_hook", m{"scheme": "gh", "mappings": []string{"issues"}})
	// 这个服务的执行器没登记任何命令：落盘后的处理失败；随后把记录改回 received，等同落盘后、处理前崩溃。
	broken := e.newService(e.st, command.New(e.st, func() time.Time { return t0 }))
	e.svc = broken
	e.serve(0)
	body := issueBody(5)
	code(t, e.ghPost("src_hook", "d-5", body, sign(secret, body)), 200)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE webhook_ingest SET state = ?, reason = '' WHERE state = ?`, webhook.StateReceived, webhook.StateFailed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if gs := e.ingests(); len(gs) != 1 || gs[0].State != webhook.StateReceived || string(gs[0].Body) != body {
		t.Fatalf("ingests = %+v", gs)
	}
	if len(e.events("src_hook")) != 0 {
		t.Fatal("event must not exist before recovery")
	}
	// 重启：新服务对同一账本 Recover。
	restarted := e.newService(e.st, e.x)
	if err := restarted.Recover(bg); err != nil {
		t.Fatal(err)
	}
	if gs := e.ingests(); gs[0].State != webhook.StateProcessed || len(e.events("src_hook")) != 1 || e.itemCount() != 1 {
		t.Fatalf("after recover: %+v", gs)
	}
}

// ACT-HOOK-06：超过体积上限回 413，超过频率上限回 429，已有数据不受影响。
func TestBodyAndRateLimits(t *testing.T) {
	e := newEnv(t, 64, 3) // 413 在路由层拒绝，不占内核的频率配额
	e.register("src_hook", m{"scheme": "gh", "mappings": []string{"issues"}})
	code(t, e.ghPost("src_hook", "d-big", strings.Repeat("x", 65), "00"), 413)
	small := `{"a":1}`
	code(t, e.ghPost("src_hook", "d-1", small, sign(secret, small)), 200)
	code(t, e.ghPost("src_hook", "d-2", small, sign(secret, small)), 200)
	code(t, e.ghPost("src_hook", "d-3", small, sign(secret, small)), 200)
	code(t, e.ghPost("src_hook", "d-4", small, sign(secret, small)), 429)
	for _, g := range e.ingests() {
		if len(g.Body) > 64 {
			t.Fatalf("oversized body was stored: %d", len(g.Body))
		}
	}
}

// ACT-HOOK-07：Slack 类 url_verification 握手同步返回 challenge，不落盘；握手也要验签。
func TestPlatformHandshakeIsAnsweredSynchronously(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_slack", m{"scheme": "slack", "mappings": []string{"issues"}})
	body := `{"type":"url_verification","challenge":"abc123"}`
	ts := strconv.FormatInt(t0.Unix(), 10)
	rec := e.do("POST", "/ingest/src_slack", body, map[string]string{"X-Ts": ts, "X-Sig": sign(secret, "v0:"+ts+":"+body)})
	code(t, rec, 200)
	if rec.Body.String() != "abc123" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || len(e.ingests()) != 0 {
		t.Fatalf("body=%q ct=%q ingests=%d", rec.Body, rec.Header().Get("Content-Type"), len(e.ingests()))
	}
	code(t, e.do("POST", "/ingest/src_slack", body, map[string]string{"X-Ts": ts, "X-Sig": "00"}), 401)
}

func issueWebhookToken(e *env, src string) string {
	var tok string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		_, tok, err = e.auth.Issue(tx, auth.Spec{Kind: auth.Webhook, Scope: src})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// ACT-HOOK-08：通用共享 token 方案——正确 token 回 2xx，错误 token 回 401。
func TestSharedTokenScheme(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_tok", m{"scheme": "shared", "mappings": []string{"issues"}})
	tok := issueWebhookToken(e, "src_tok")
	body := issueBody(3)
	h := func(tok string) map[string]string {
		return map[string]string{"X-Token": tok, "X-GitHub-Event": "issues", "X-GitHub-Delivery": "t-1"}
	}
	code(t, e.do("POST", "/ingest/src_tok", body, h(tok)), 200)
	code(t, e.do("POST", "/ingest/src_tok", body, h("apa_whk_wrong")), 401)
	if len(e.events("src_tok")) != 1 {
		t.Fatalf("events = %d", len(e.events("src_tok")))
	}
}

// ACT-HOOK-12：接收密钥作 Bearer 读写账本都被拒绝；它也不能凭 Bearer 调 /ingest 之外的任何东西。
func TestReceiveKeyCannotReachOtherAPIs(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_tok", m{"scheme": "shared", "mappings": []string{"issues"}})
	tok := issueWebhookToken(e, "src_tok")
	bearer := map[string]string{"Authorization": "Bearer " + tok}
	for _, c := range []struct{ method, body string }{{"GET", ""}, {"POST", `{"kind":"task","title":"x"}`}} {
		rec := e.do(c.method, "/v1/items", c.body, bearer)
		if rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("%s /v1/items with receive key = %d %s", c.method, rec.Code, rec.Body)
		}
	}
	if e.itemCount() != 0 {
		t.Fatal("receive key created an item")
	}
}

// 路由不带版本前缀；/v1/ingest 不存在，GET 不是接收方法，未知来源回 404。
func TestRouteShape(t *testing.T) {
	e := newEnv(t, 0, 0)
	e.register("src_hook", m{"scheme": "gh", "mappings": []string{"issues"}})
	body := issueBody(1)
	code(t, e.do("POST", "/v1/ingest/src_hook", body, nil), 404)
	code(t, e.do("GET", "/ingest/src_hook", "", nil), 404)
	code(t, e.ghPost("src_nope", "d", body, sign(secret, body)), 404)
}

// 请求体超过路由上限时读取失败也回 413，且不泄露内部错误。
func TestOversizedStreamIs413(t *testing.T) {
	e := newEnv(t, 16, 0)
	e.register("src_hook", m{"scheme": "gh", "mappings": []string{"issues"}})
	req := httptest.NewRequest(http.MethodPost, "/ingest/src_hook", bytes.NewReader(make([]byte, 4096)))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	code(t, rec, 413)
}
