package delegation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kdel "github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const (
	ownerPW   = "correct horse battery"
	workHost  = "work-host.cli"
	otherHost = "other-host.cli"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	x      *command.Executor
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

// setup 装配真实账本、全部相关内核命令和 HTTP 服务，并以本人口令登录一个 Web 会话。
func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		kitems.Module, executors.Module, kdel.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	if err := projects.Register(x); err != nil {
		t.Fatal(err)
	}
	if err := kitems.Register(x, kitems.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := executors.Register(x, executors.Options{Owner: "owner", Project: projects.Get}); err != nil {
		t.Fatal(err)
	}
	if err := kdel.Register(x, kdel.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Error(err) }}); err != nil {
		t.Fatal(err)
	}
	a := auth.New(nil)
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), Routes(x)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, auth: a, x: x, srv: srv}
	for _, id := range []string{workHost, otherHost} {
		if _, err := x.Do(bg, command.Request{Identity: owner, Command: "executor.put", Input: js(map[string]any{"id": id, "kind": "external-cli"})}); err != nil {
			t.Fatal(err)
		}
	}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	e.csrf = out.CSRF
	return e
}

func js(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000" // 回环来源，明文登录才被接受
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(r *http.Request) {
	r.AddCookie(e.cookie)
	r.Header.Set(core.CSRFHeader, e.csrf)
}

func hdr(kv ...string) func(*http.Request) {
	return func(r *http.Request) {
		for i := 0; i < len(kv); i += 2 {
			r.Header.Set(kv[i], kv[i+1])
		}
	}
}

func (e *env) token(kind auth.Kind, executor string) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Executor: executor})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return hdr("Authorization", "Bearer "+secret)
}

func (e *env) cmd(name string, in any) json.RawMessage {
	e.t.Helper()
	res, err := e.x.Do(bg, command.Request{Identity: owner, Command: name, Input: js(in)})
	if err != nil {
		e.t.Fatal(name, err)
	}
	return res.Value
}

// project 建项目并设置允许的执行者（nil 表示不限制）。
func (e *env) project(name string, allowed []string) string {
	e.t.Helper()
	var o struct{ ID string }
	_ = json.Unmarshal(e.cmd("project.create", map[string]any{"name": name}), &o)
	if allowed != nil {
		e.cmd("project.allowed_executors.set", map[string]any{"id": o.ID, "executors": allowed})
	}
	return o.ID
}

// parent 经 HTTP 以本人身份建一个 open 的父事项。
func (e *env) parent(project string) string {
	e.t.Helper()
	rec := e.do("POST", "/v1/items", `{"kind":"task","title":"fix bug","phase":"open","project":"`+project+`"}`, e.web)
	if rec.Code != 201 {
		e.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return idOf(e.t, rec)
}

func idOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var w struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil || w.ID == "" {
		t.Fatalf("no id in %s", rec.Body)
	}
	return w.ID
}

func (e *env) item(id string) domain.Item {
	e.t.Helper()
	var g struct{ Item domain.Item }
	_ = json.Unmarshal(e.do("GET", "/v1/items/"+id, "", e.web).Body.Bytes(), &g)
	return g.Item
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder, status int) domain.Error {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" || rec.Code != status {
		t.Fatalf("want error %d, got %d %s", status, rec.Code, rec.Body)
	}
	return de
}

// flow-delegate-recover：委派建子事项、父事项转 waiting；绑定的 worker 凭据回执 done，父事项回 open；其他 worker 凭据不能回执。
func TestDelegateAndReportRecoverParent(t *testing.T) {
	e := setup(t)
	proj := e.project("acc-work", []string{workHost})
	parent := e.parent(proj)
	dlg, wrk, foreign := e.token(auth.Delegate, ""), e.token(auth.Worker, workHost), e.token(auth.Worker, otherHost)

	rec := e.do("POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+workHost+`","title":"do it"}`, dlg)
	if rec.Code != 201 {
		t.Fatalf("delegate = %d %s", rec.Code, rec.Body)
	}
	child := idOf(t, rec)
	if c := e.item(child); c.Assignee != workHost || c.Project != proj || c.Kind != domain.KindDelegation || c.Title != "do it" {
		t.Fatalf("child = %+v", c)
	}
	if p := e.item(parent); p.Phase != domain.PhaseWaiting || p.WaitingOn == nil || p.WaitingOn.Ref != workHost {
		t.Fatalf("parent after delegate = %+v", p)
	}

	body := `{"outcome":"done","summary":"shipped","evidence":{"pr":"https://x/pr/88","commit":"abc"}}`
	if de := errBody(t, e.do("POST", "/v1/items/"+child+"/report", body, foreign), 403); de.Code != domain.CodePermissionDenied {
		t.Fatalf("foreign worker: %+v", de)
	}
	if e.item(child).Phase == domain.PhaseDone {
		t.Fatal("foreign receipt changed the child")
	}
	// id 只能来自路径。
	errBody(t, e.do("POST", "/v1/items/"+child+"/report", `{"id":"x","outcome":"done","summary":"s"}`, wrk), 400)

	if rec := e.do("POST", "/v1/items/"+child+"/report", body, wrk); rec.Code != 200 {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	if c := e.item(child); c.Phase != domain.PhaseDone {
		t.Fatalf("child = %+v", c)
	}
	if p := e.item(parent); p.Phase != domain.PhaseOpen || p.WaitingOn != nil {
		t.Fatalf("parent after report = %+v", p)
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		if it, ok, err := kdel.Lookup(tx, "https://x/pr/88"); err != nil || !ok || it != child {
			t.Fatalf("PR link = %q %v %v", it, ok, err)
		}
		return nil
	})

	// 子事项已终态：再回执是 conflict，不重复写入。
	errBody(t, e.do("POST", "/v1/items/"+child+"/report", body, wrk), 409)
}

// flow-allowed-executors：名单外 403/executor_not_allowed 带允许的 id 且不建子事项；清单带 project 标明是否允许；observer 可读清单。
func TestAllowedExecutors(t *testing.T) {
	e := setup(t)
	proj := e.project("acc-work", []string{workHost})
	parent := e.parent(proj)
	dlg, obs := e.token(auth.Delegate, ""), e.token(auth.Observer, "")

	de := errBody(t, e.do("POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+otherHost+`"}`, dlg), 403)
	if de.Code != domain.CodeExecutorNotAllowed || !slices.Equal(de.Allowed, []string{workHost}) {
		t.Fatalf("rejection = %+v", de)
	}
	if p := e.item(parent); p.Phase != domain.PhaseOpen {
		t.Fatalf("parent changed: %+v", p)
	}
	// 不在清单里：404。
	errBody(t, e.do("POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"ghost.cli"}`, dlg), 404)

	rec := e.do("GET", "/v1/executors?project="+proj, "", obs)
	if rec.Code != 200 {
		t.Fatalf("executors = %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Executors []struct {
			ID      string
			Allowed *bool
		}
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	got := map[string]bool{}
	for _, x := range list.Executors {
		if x.Allowed == nil {
			t.Fatalf("allowed missing for %s", x.ID)
		}
		got[x.ID] = *x.Allowed
	}
	if len(got) != 2 || !got[workHost] || got[otherHost] {
		t.Fatalf("allowed = %v", got)
	}
	// 不带 project：不标 allowed。
	if body := e.do("GET", "/v1/executors", "", obs).Body.String(); strings.Contains(body, "allowed") || !strings.Contains(body, workHost) {
		t.Fatalf("unscoped list = %s", body)
	}
	if rec := e.do("GET", "/v1/executors", "", nil); rec.Code != 401 {
		t.Fatalf("anonymous list = %d", rec.Code)
	}
}

// 授权：observer 不能委派或回执；POST /declarations 只进待确认，worker 可提交、observer 不可，不会直接生效。
func TestAuthorizationAndDeclarations(t *testing.T) {
	e := setup(t)
	parent := e.parent("")
	obs, wrk := e.token(auth.Observer, ""), e.token(auth.Worker, workHost)

	if de := errBody(t, e.do("POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+workHost+`"}`, obs), 403); de.Code != domain.CodePermissionDenied {
		t.Fatalf("observer delegate: %+v", de)
	}
	rec := e.do("POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+workHost+`"}`, e.token(auth.Delegate, ""))
	if rec.Code != 201 {
		t.Fatalf("delegate = %d %s", rec.Code, rec.Body)
	}
	errBody(t, e.do("POST", "/v1/items/"+idOf(t, rec)+"/report", `{"outcome":"done","summary":"s"}`, obs), 403)
	errBody(t, e.do("POST", "/v1/delegations", `[]`, wrk), 400)
	errBody(t, e.do("POST", "/v1/delegations", `null`, wrk), 400)

	decl := `{"harness":"newhost","executors":[{"id":"newhost.cli","kind":"external-cli"}]}`
	errBody(t, e.do("POST", "/v1/declarations", decl, obs), 403)
	rec = e.do("POST", "/v1/declarations", decl, wrk)
	if rec.Code != 202 {
		t.Fatalf("declare = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(e.do("GET", "/v1/executors", "", wrk).Body.String(), "newhost.cli") {
		t.Fatal("an unconfirmed declaration is live")
	}
	errBody(t, e.do("POST", "/v1/declarations", `{"harness":"newhost"}`, wrk), 400)

	// 本人在 Web 确认后才进入清单。
	e.cmd("executor.confirm", map[string]any{"harness": "newhost", "rev": 1})
	if !strings.Contains(e.do("GET", "/v1/executors", "", wrk).Body.String(), "newhost.cli") {
		t.Fatal("confirmed declaration missing from the list")
	}
}

// 幂等与乐观版本：同键重放首次结果不重复建子事项；If-Match 不符 409、畸形 400。
func TestIdempotencyAndVersion(t *testing.T) {
	e := setup(t)
	parent := e.parent("")
	dlg := e.token(auth.Delegate, "")
	body := `{"parent":"` + parent + `","assignee":"` + workHost + `"}`
	first := e.do("POST", "/v1/delegations", body, func(r *http.Request) { dlg(r); r.Header.Set(IdempotencyHeader, "k1") })
	if first.Code != 201 {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	again := e.do("POST", "/v1/delegations", body, func(r *http.Request) { dlg(r); r.Header.Set(IdempotencyHeader, "k1") })
	if again.Code != 200 || idOf(t, again) != idOf(t, first) || !strings.Contains(again.Body.String(), `"replayed":true`) {
		t.Fatalf("replay = %d %s", again.Code, again.Body)
	}

	child := idOf(t, first)
	wrk := e.token(auth.Worker, workHost)
	report := `{"outcome":"progress","summary":"half way"}`
	errBody(t, e.do("POST", "/v1/items/"+child+"/report", report, func(r *http.Request) { wrk(r); r.Header.Set(IfMatchHeader, "999") }), 409)
	errBody(t, e.do("POST", "/v1/items/"+child+"/report", report, func(r *http.Request) { wrk(r); r.Header.Set(IfMatchHeader, "abc") }), 400)
	if rec := e.do("POST", "/v1/items/"+child+"/report", report, wrk); rec.Code != 200 {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	if e.item(child).StatusLabel != "half way" {
		t.Fatalf("status label = %q", e.item(child).StatusLabel)
	}
}
