package agentsmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	_ "modernc.org/sqlite"
)

const (
	ownerPW = "correct horse battery"
	repo    = "https://example.com/acme/app.git"
)

var bg = context.Background()

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	x      *command.Executor
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

// setup 装配真实账本、内核命令、AGENTS.md 服务与路由，并以本人口令登录一个 Web 会话；项目已登记 repo。
func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, materials.Module, hosted.Module, agentsmd.Module, core.Module} {
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
	for _, f := range []func() error{
		func() error { return projects.Register(x) },
		func() error { return items.Register(x, items.NewKinds()) },
		func() error { return materials.Register(x) },
		func() error { return hosted.Register(x) },
		func() error { return decisions.Register(x) },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	ld := kp.New(kp.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	if _, err := ld.Load(); err != nil {
		t.Fatal(err)
	}
	pack, ok := ld.Get("agents-md")
	if !ok {
		t.Fatal("agents-md pack missing")
	}
	svc, err := agentsmd.New(x, agentsmd.Options{Template: pack.Templates["agents"], OnError: func(err error) { t.Errorf("auto render: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(nil)
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(x, svc)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, auth: a, x: x, srv: srv}
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
	owner := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	var prj struct{ ID string }
	e.kernel(owner, "project.create", map[string]any{"name": "acc-work"}, &prj)
	e.kernel(owner, "project.repo.add", map[string]any{"id": prj.ID, "remote": repo}, nil)
	return e
}

func (e *env) kernel(id command.Identity, cmd string, in any, out any) {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", WebURL: "http://web/x"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	if out != nil {
		_ = json.Unmarshal(r.Value, out)
	}
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
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

func (e *env) token(kind auth.Kind) func(*http.Request) {
	e.t.Helper()
	scope := ""
	if kind == auth.Ext || kind == auth.Webhook {
		scope = "x"
	}
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func (e *env) get(t *testing.T, mod func(*http.Request)) agentsmd.View {
	t.Helper()
	rec := e.do("GET", "/v1/agents-md?repo="+repo, "", mod)
	if rec.Code != 200 {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	var v agentsmd.View
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) check(body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	return e.do("POST", "/v1/agents-md/check", body, mod)
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Code
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// flow-agents-md / ACT-AGM-02：GET 返回完整渲染；POST check 对同一 hash 判 current，并让 GET 的 last_check 反映它。
func TestGetThenCheckCurrent(t *testing.T) {
	e := setup(t)
	obs := e.token(auth.Observer)
	v := e.get(t, obs)
	if v.Content == "" || v.Hash != agentsmd.FileHash(v.Content) || v.LastCheck != nil {
		t.Fatalf("fresh render must carry full content, its hash and no last_check: %+v", v)
	}
	rec := e.check(mustJSON(map[string]any{"repo": repo, "host": "m1", "file_hash": v.Hash}), obs)
	var vd agentsmd.Verdict
	_ = json.Unmarshal(rec.Body.Bytes(), &vd)
	if rec.Code != 200 || vd.State != agentsmd.StateCurrent || vd.Exit != 0 || vd.RenderHash != v.Hash {
		t.Fatalf("check current: %d %s", rec.Code, rec.Body)
	}
	lc := e.get(t, obs).LastCheck
	if lc == nil || lc.State != agentsmd.StateCurrent || lc.Host != "m1" || lc.FileHash != v.Hash {
		t.Fatalf("last_check must reflect the report: %+v", lc)
	}
}

// 缺失文件判过期（exit 10）；Web 会话也能走同一路由。
func TestCheckMissingIsStale(t *testing.T) {
	e := setup(t)
	rec := e.check(`{"repo":"`+repo+`","host":"m1","missing":true}`, e.web)
	var vd agentsmd.Verdict
	_ = json.Unmarshal(rec.Body.Bytes(), &vd)
	if rec.Code != 200 || vd.State != agentsmd.StateStale || vd.Exit != agentsmd.ExitStale || !vd.Prompt {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body)
	}
}

// 手改内容判漂移（exit 11）并带出 needs_input 事项；只报未知 hash 无法附 diff，是 400。
func TestCheckDriftAndUnknownHash(t *testing.T) {
	e := setup(t)
	obs := e.token(auth.Observer)
	rec := e.check(mustJSON(map[string]any{"repo": repo, "host": "m1", "content": "# hand edited\n"}), obs)
	var vd agentsmd.Verdict
	_ = json.Unmarshal(rec.Body.Bytes(), &vd)
	if rec.Code != 200 || vd.State != agentsmd.StateDrift || vd.Exit != agentsmd.ExitDrift || vd.Item == "" || vd.Decision != "open" {
		t.Fatalf("drift: %d %s", rec.Code, rec.Body)
	}
	if lc := e.get(t, obs).LastCheck; lc == nil || lc.State != agentsmd.StateDrift {
		t.Fatalf("last_check after drift: %+v", lc)
	}
	rec = e.check(`{"repo":"`+repo+`","host":"m1","file_hash":"sha256:nope"}`, obs)
	if rec.Code != 400 || errCode(t, rec) != "invalid_request" {
		t.Fatalf("unknown hash: %d %s", rec.Code, rec.Body)
	}
}

// 失败语义：缺 repo、未登记仓库、非法 body、凭据种类不对。
func TestFailures(t *testing.T) {
	e := setup(t)
	obs := e.token(auth.Observer)
	for _, c := range []struct {
		name, method, path, body string
		mod                      func(*http.Request)
		status                   int
		code                     string
	}{
		{"get without repo", "GET", "/v1/agents-md", "", obs, 400, "invalid_request"},
		{"get unknown repo", "GET", "/v1/agents-md?repo=https://nope/x.git", "", obs, 404, "not_found"},
		{"check unknown repo", "POST", "/v1/agents-md/check", `{"repo":"https://nope/x.git","host":"h","missing":true}`, obs, 404, "not_found"},
		{"check two report forms", "POST", "/v1/agents-md/check", `{"repo":"` + repo + `","host":"h","missing":true,"file_hash":"x"}`, obs, 400, "invalid_request"},
		{"check bad json", "POST", "/v1/agents-md/check", `{`, obs, 400, "invalid_request"},
		{"check unknown field", "POST", "/v1/agents-md/check", `{"repo":"` + repo + `","host":"h","missing":true,"zzz":1}`, obs, 400, "invalid_request"},
		{"unauthenticated", "GET", "/v1/agents-md?repo=" + repo, "", nil, 401, ""},
		{"ext credential", "GET", "/v1/agents-md?repo=" + repo, "", e.token(auth.Ext), 403, "permission_denied"},
		{"webhook credential", "POST", "/v1/agents-md/check", `{}`, e.token(auth.Webhook), 403, "permission_denied"},
	} {
		rec := e.do(c.method, c.path, c.body, c.mod)
		if rec.Code != c.status || (c.code != "" && errCode(t, rec) != c.code) {
			t.Errorf("%s: want %d %s, got %d %s", c.name, c.status, c.code, rec.Code, rec.Body)
		}
	}
}
