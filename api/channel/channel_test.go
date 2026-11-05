package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	kchannel "github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	wrk   = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: authz.LevelWorker}
)

type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	q   *delivery.Queue
	srv *core.Server
	a   *auth.Auth
	mu  sync.Mutex
	now time.Time
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

// setup 装配真实账本、命令、投递队列、触达与 HTTP 服务；渠道 chan 只能发文字，回答人白名单只有 alice。
func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, delivery.Module, kcontext.Module, kchannel.Module, attention.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, a: auth.New(nil), now: t0}
	e.x = command.New(st, e.clock)
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.q = delivery.New(delivery.Options{Now: e.clock, UnclaimedAfter: 5 * time.Minute, Lease: 24 * time.Hour})
	if err := kchannel.Register(e.x, kchannel.Options{Queue: e.q, Now: e.clock}); err != nil {
		t.Fatal(err)
	}
	att := attention.New(attention.Options{Queue: e.q, Now: e.clock})
	att.Attach(e.x, st, func(err error) { t.Errorf("attention: %v", err) })
	if _, err := clock.New(st, func(tx *store.Tx, c clock.Command) error {
		if att.Owns(c) {
			return att.HandleTimer(tx, c)
		}
		return items.HandleTimer(tx, c)
	}, e.clock); err != nil {
		t.Fatal(err)
	}
	e.update(att.Start)
	e.policy(func(p *policy.Policy) {
		p.Disturb.Routes["decision"] = "chan"
		p.Channels = map[string]policy.Channel{"chan": {TextOnly: true}}
		p.ChannelAnswerers = map[string][]string{"chan": {"alice"}}
		p.WebOnly.Rules = []policy.WebOnlyRule{{Tag: "money", Category: policy.CategoryFunds}}
	})
	e.srv, err = core.New(core.Deps{Store: st, Auth: e.a, Now: e.clock, Groups: []core.RouteGroup{Routes(e.x, st, e.clock)}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) update(fn func(*store.Tx) error) {
	e.t.Helper()
	if err := e.st.Update(bg, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) policy(f func(*policy.Policy)) {
	e.t.Helper()
	e.update(func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error { f(p); return nil })
		return err
	})
}

func (e *env) do(t *testing.T, id command.Identity, cmd string, in any) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner"})
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

// sent 建一个带决策请求的事项，按渠道 ext 的流程领取并回报送达，返回事项、投递与表单令牌。
func (e *env) sent(tags ...string) (item string, d domain.Delivery, token string) {
	e.t.Helper()
	var out struct{ ID string }
	_ = json.Unmarshal(e.do(e.t, owner, "item.create", map[string]any{"title": "t", "kind": "task", "phase": "open", "assignee": "wrk", "tags": tags}), &out)
	e.do(e.t, wrk, "decision.request", map[string]any{"id": out.ID, "ask": map[string]any{"question": "ship it?", "free_text": true,
		"options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}})
	e.update(func(tx *store.Tx) error {
		got, err := e.q.Claim(tx, delivery.ChannelRoute("chan"))
		if err != nil || got == nil || got.Item != out.ID {
			e.t.Fatalf("claim: %+v %v", got, err)
		}
		d = *got
		_, _, err = e.q.Ack(tx, d.ID, d.Version, "msg-"+out.ID)
		return err
	})
	return out.ID, d, strings.TrimPrefix(d.Content.FormURL, "/forms/")
}

func (e *env) key(name string) func(*http.Request) {
	e.t.Helper()
	var secret string
	e.update(func(tx *store.Tx) error {
		_, s, err := e.a.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "channel." + name})
		secret = s
		return err
	})
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func (e *env) req(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5000"
	if mod != nil {
		mod(r)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, r)
	return rec
}

func (e *env) item(id string) (it domain.Item) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) stillAsking(id string) {
	e.t.Helper()
	if it := e.item(id); it.Ask == nil || it.Ask.Resolution != nil {
		e.t.Fatalf("decision resolved unexpectedly: %+v", it.Ask)
	}
}

// want 断言状态码与统一错误体里的 code。
func want(t *testing.T, rec *httptest.ResponseRecorder, status int, code domain.Code) *domain.Error {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if code == "" {
		return nil
	}
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code != code {
		t.Fatalf("error body %s, want code %s", rec.Body, code)
	}
	return &de
}

func answerBody(sender, ref, choice string) string {
	b, _ := json.Marshal(map[string]string{"sender": sender, "ref": ref, "choice": choice})
	return string(b)
}

// ACT-CHP-05 渠道转达回答：回答人在白名单、ref 与投递一致，决定按渠道名落账，投递随之确认。
func TestAnswerRoute(t *testing.T) {
	e := setup(t)
	id, d, _ := e.sent()
	ch := e.key("chan")
	rec := e.req("POST", "/v1/deliveries/"+d.ID+"/answer", answerBody("alice", "msg-"+id, "A"), ch)
	want(t, rec, 200, "")
	if !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("body %s", rec.Body)
	}
	r := e.item(id).Ask.Resolution
	if r == nil || r.Choice != "A" || r.ResolvedVia != "chan" || r.AnsweredBy != "alice" {
		t.Fatalf("resolution %+v", r)
	}
	// 只认第一次回答（ACT-CHP-07）：第二次 409，已有结果不变。
	want(t, e.req("POST", "/v1/deliveries/"+d.ID+"/answer", answerBody("alice", "msg-"+id, "B"), ch), 409, domain.CodeConflict)
	if e.item(id).Ask.Resolution.Choice != "A" {
		t.Fatal("first answer overwritten")
	}
}

// 回答人、ref、密钥的校验结果映射成错误响应，且都不改变决策。
func TestAnswerRouteMapsRejections(t *testing.T) {
	e := setup(t)
	id, d, _ := e.sent()
	ch, other := e.key("chan"), e.key("other")
	path := "/v1/deliveries/" + d.ID + "/answer"
	ref := "msg-" + id
	for name, c := range map[string]struct {
		body string
		key  func(*http.Request)
		code domain.Code
	}{
		"sender outside allowlist": {answerBody("mallory", ref, "A"), ch, domain.CodePermissionDenied},
		"sender missing":           {answerBody("", ref, "A"), ch, domain.CodePermissionDenied},
		"ref differs":              {answerBody("alice", "msg-other", "A"), ch, domain.CodePermissionDenied},
		"ref missing":              {answerBody("alice", "", "A"), ch, domain.CodePermissionDenied},
		"another channel's key":    {answerBody("alice", ref, "A"), other, domain.CodePermissionDenied},
		"unknown field":            {`{"sender":"alice","ref":"` + ref + `","choice":"A","bogus":1}`, ch, domain.CodeInvalidRequest},
		"delivery id in body":      {`{"delivery":"x","sender":"alice","ref":"` + ref + `","choice":"A"}`, ch, domain.CodeInvalidRequest},
		"not an object":            {`null`, ch, domain.CodeInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.req("POST", path, c.body, c.key)
			want(t, rec, c.code.HTTPStatus(), c.code)
			e.stillAsking(id)
		})
	}
	// 没有凭据：401，不触达命令。
	want(t, e.req("POST", path, answerBody("alice", ref, "A"), nil), 401, domain.CodeUnauthenticated)
	e.stillAsking(id)
}

// 事项变化后旧消息里的回答 409；web_only 类回 Web 链接。
func TestAnswerRouteOutdatedAndWebOnly(t *testing.T) {
	e := setup(t)
	ch := e.key("chan")
	id, d, _ := e.sent()
	e.do(t, owner, "item.update", map[string]any{"id": id, "title": "changed"})
	want(t, e.req("POST", "/v1/deliveries/"+d.ID+"/answer", answerBody("alice", "msg-"+id, "A"), ch), 409, domain.CodeConflict)
	e.stillAsking(id)

	e = setup(t)
	ch = e.key("chan")
	id2, d2, token := e.sent("money")
	if token != "" {
		t.Fatalf("web_only decision got a form link")
	}
	de := want(t, e.req("POST", "/v1/deliveries/"+d2.ID+"/answer", answerBody("alice", "msg-"+id2, "A"), ch), domain.CodeWebOnly.HTTPStatus(), domain.CodeWebOnly)
	if de.WebURL == nil || *de.WebURL != "/items/"+id2 {
		t.Fatalf("web url %v", de.WebURL)
	}
	e.stillAsking(id2)
	want(t, e.req("POST", "/v1/deliveries/nope/answer", answerBody("alice", "x", "A"), ch), 404, domain.CodeNotFound)
}

// ACT-CHP-06 表单：GET 只回问题与选项且不兑现令牌；POST 作答一次，记发放时的渠道，再用即 409。
func TestFormRoutes(t *testing.T) {
	e := setup(t)
	id, _, token := e.sent()
	if token == "" {
		t.Fatal("no form link")
	}
	rec := e.req("GET", "/forms/"+token, "", nil)
	want(t, rec, 200, "")
	var shown struct {
		Question string
		Options  []struct{ ID, Label string }
		FreeText bool `json:"free_text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &shown); err != nil || shown.Question != "ship it?" || len(shown.Options) != 2 || shown.Options[0].ID != "A" || !shown.FreeText {
		t.Fatalf("form %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), id) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("form leaks ledger content or is cacheable: %s %v", rec.Body, rec.Header())
	}
	e.stillAsking(id) // GET 不兑现
	want(t, e.req("GET", "/forms/"+token, "", nil), 200, "")

	want(t, e.req("POST", "/forms/"+token, `{"choice":"B","token":"x"}`, nil), 400, domain.CodeInvalidRequest)
	want(t, e.req("POST", "/forms/"+token, `{"choice":"B"}`, nil), 200, "")
	if r := e.item(id).Ask.Resolution; r == nil || r.Choice != "B" || r.ResolvedVia != "chan" {
		t.Fatalf("resolution %+v", r)
	}
	want(t, e.req("POST", "/forms/"+token, `{"choice":"A"}`, nil), 409, domain.CodeConflict)
	want(t, e.req("GET", "/forms/"+token, "", nil), 409, domain.CodeConflict)
	want(t, e.req("GET", "/forms/no-such-token", "", nil), 404, domain.CodeNotFound)
	want(t, e.req("POST", "/forms/no-such-token", `{"choice":"A"}`, nil), 404, domain.CodeNotFound)
}

// 令牌过期、事项变更、策略后来改成 web_only：作答全部拒绝，且校验失败不消耗令牌。
func TestFormRoutesRefusals(t *testing.T) {
	e := setup(t)
	_, _, expired := e.sent()
	e.advance(kchannel.FormTTL + time.Second)
	want(t, e.req("POST", "/forms/"+expired, `{"choice":"A"}`, nil), 409, domain.CodeConflict)

	e2 := setup(t)
	id, _, token := e2.sent()
	e2.do(t, owner, "item.update", map[string]any{"id": id, "title": "changed"})
	want(t, e2.req("GET", "/forms/"+token, "", nil), 409, domain.CodeConflict)
	want(t, e2.req("POST", "/forms/"+token, `{"choice":"A"}`, nil), 409, domain.CodeConflict)
	e2.stillAsking(id)

	e3 := setup(t)
	id3, _, token3 := e3.sent()
	e3.policy(func(p *policy.Policy) {
		p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Kind: "task", Category: policy.CategoryFunds})
	})
	de := want(t, e3.req("POST", "/forms/"+token3, `{"choice":"A"}`, nil), domain.CodeWebOnly.HTTPStatus(), domain.CodeWebOnly)
	if de.WebURL == nil || *de.WebURL != "/items/"+id3 {
		t.Fatalf("web url %v", de.WebURL)
	}
	e3.stillAsking(id3)
}

// 表单令牌不是 Bearer：拿它访问 /v1 路由在骨干处 401。
func TestFormTokenIsNotABearer(t *testing.T) {
	e := setup(t)
	_, d, token := e.sent()
	rec := e.req("POST", "/v1/deliveries/"+d.ID+"/answer", answerBody("alice", "x", "A"), func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	want(t, rec, 401, domain.CodeUnauthenticated)
}
