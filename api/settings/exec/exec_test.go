package exec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kexec "github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

var bg = context.Background()

type env struct {
	t      *testing.T
	st     *store.Store
	au     *auth.Auth
	x      *command.Executor
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, command.Module, core.Module, kexec.Module, kp.Module, kpolicy.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, au: auth.New(nil), x: command.New(st, nil)}
	if err := kexec.Register(e.x, kexec.Options{Owner: "me", Project: kp.Get}); err != nil {
		t.Fatal(err)
	}
	if err := kp.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	if e.srv, err = core.New(core.Deps{Store: st, Auth: e.au, Groups: []core.RouteGroup{Routes(st, e.x)}}); err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	e.csrf = out.CSRF
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	return e
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(method, path, body string) *httptest.ResponseRecorder {
	return e.do(method, path, body, func(r *http.Request) {
		r.AddCookie(e.cookie)
		r.Header.Set(core.CSRFHeader, e.csrf)
	})
}

func (e *env) token(kind auth.Kind) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		_, secret, err = e.au.Issue(tx, auth.Spec{Kind: kind, Scope: map[auth.Kind]string{auth.Ext: "chan"}[kind]})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func code(t *testing.T, r *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(r.Body.Bytes(), &de); err != nil {
		t.Fatalf("not an error body: %d %s", r.Code, r.Body)
	}
	return de.Code
}

func (e *env) events(typ string) []eventlog.Entry {
	e.t.Helper()
	var out []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) error {
		all, err := eventlog.Read(tx, 0, 1000)
		for _, en := range all {
			if en.Event.Type == typ {
				out = append(out, en)
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

const declJSON = `{"harness":"acme","headless":{"command":["acme","run","{prompt}"],"session":{"extract":"$.session_id"},"access":{"read":["--ro"]}},
"executors":[{"id":"acme.build","kind":"external-cli","host":"work-host"}]}`

func (e *env) project(name string) string {
	e.t.Helper()
	res, err := e.x.Do(bg, command.Request{Identity: command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb},
		Command: "project.create", Input: json.RawMessage(`{"name":"` + name + `"}`)})
	if err != nil {
		e.t.Fatal(err)
	}
	var ref struct{ ID string }
	_ = json.Unmarshal(res.Value, &ref)
	return ref.ID
}

// flow-allowed-executors：名单只能由 Web 会话改，改动记事件；null/[]/名单三态语义与读回一致。
func TestAllowedExecutorsWebOnlyAndAudited(t *testing.T) {
	e := setup(t)
	id := e.project("acc-work")
	path := "/v1/settings/projects/" + id + "/allowed-executors"

	for _, body := range []string{`{"executors":["work-host.cli"]}`, `{"executors":["work-host.cli","work-host.cli"]}`} {
		if r := e.web("PUT", path, body); r.Code != 200 || !strings.Contains(r.Body.String(), `"allowed_executors":["work-host.cli"]`) {
			t.Fatalf("web PUT %s: %d %s", body, r.Code, r.Body)
		}
	}
	if n := len(e.events(domain.EvProjectAllowedExecutorsChanged)); n != 2 {
		t.Fatalf("each change must be recorded as an event, got %d", n)
	}

	// 非 Web 入口：delegate 得 web_only(403)，worker/observer 得 permission_denied，名单不变。
	for kind, want := range map[auth.Kind]domain.Code{auth.Delegate: domain.CodeWebOnly, auth.Worker: domain.CodePermissionDenied, auth.Observer: domain.CodePermissionDenied} {
		r := e.do("PUT", path, `{"executors":null}`, e.token(kind))
		if r.Code < 400 || code(t, r) != want {
			t.Fatalf("%s PUT: %d %s, want %s", kind, r.Code, r.Body, want)
		}
		if r := e.do("GET", path, "", e.token(kind)); r.Code < 400 {
			t.Fatalf("%s must not read settings: %d %s", kind, r.Code, r.Body)
		}
	}
	if r := e.web("GET", path, ""); !strings.Contains(r.Body.String(), `["work-host.cli"]`) {
		t.Fatalf("list changed by a rejected write: %s", r.Body)
	}
	if n := len(e.events(domain.EvProjectAllowedExecutorsChanged)); n != 2 {
		t.Fatalf("rejected writes must not emit changed events, got %d", n)
	}

	// 空名单 = 只能指派给本人；null = 不限制；缺字段不能被当成 null。
	if r := e.web("PUT", path, `{"executors":[]}`); !strings.Contains(r.Body.String(), `"allowed_executors":[]`) {
		t.Fatalf("empty list: %d %s", r.Code, r.Body)
	}
	if r := e.web("PUT", path, `{"executors":null}`); !strings.Contains(r.Body.String(), `"allowed_executors":null`) {
		t.Fatalf("null: %d %s", r.Code, r.Body)
	}
	e.web("PUT", path, `{"executors":[]}`)
	if r := e.web("PUT", path, `{}`); r.Code != 400 || code(t, r) != domain.CodeInvalidRequest {
		t.Fatalf("missing executors: %d %s", r.Code, r.Body)
	}
	if r := e.web("GET", path, ""); !strings.Contains(r.Body.String(), `"allowed_executors":[]`) {
		t.Fatalf("missing field must not change the list: %s", r.Body)
	}
	if r := e.web("PUT", "/v1/settings/projects/nope/allowed-executors", `{"executors":[]}`); r.Code != 404 {
		t.Fatalf("unknown project: %d %s", r.Code, r.Body)
	}
	// 写请求没有 CSRF 令牌的 Web 会话同样被骨干拒绝。
	if r := e.do("PUT", path, `{"executors":null}`, func(r *http.Request) { r.AddCookie(e.cookie) }); r.Code != 403 {
		t.Fatalf("no CSRF: %d %s", r.Code, r.Body)
	}
}

// flow-command-registration：声明先待确认、确认后才进清单；直接登记命令只有 Web；delegate 登记不成且无副作用。
func TestDeclarationsAndCommandRegistration(t *testing.T) {
	e := setup(t)
	plugin := e.token(auth.Worker)
	if _, err := e.x.Do(bg, command.Request{Identity: command.Bearer{Auth: e.au, Secret: "x"}, Command: "executor.declare", Input: json.RawMessage(declJSON)}); err == nil {
		t.Fatal("bogus credential must not declare")
	}
	// 插件经 kernel 提交声明（HTTP 的 POST /declarations 属于别的路由分组）。
	var secret string
	_ = e.st.Update(bg, func(tx *store.Tx) (err error) { _, secret, err = e.au.Issue(tx, auth.Spec{Kind: auth.Worker}); return })
	if _, err := e.x.Do(bg, command.Request{Identity: command.Bearer{Auth: e.au, Secret: secret}, Command: "executor.declare", Input: json.RawMessage(declJSON)}); err != nil {
		t.Fatal(err)
	}

	r := e.web("GET", "/v1/settings/declarations", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"pending"`) {
		t.Fatalf("pending list: %d %s", r.Code, r.Body)
	}
	if r := e.web("GET", "/v1/settings/executors", ""); strings.Contains(r.Body.String(), "acme.build") {
		t.Fatalf("pending declaration must not be in the executor list: %s", r.Body)
	}
	if r := e.web("GET", "/v1/settings/commands", ""); strings.Contains(r.Body.String(), "acme") {
		t.Fatalf("pending declaration must not be an active command: %s", r.Body)
	}
	// worker 凭据不能确认；delegate 得 web_only。
	if r := e.do("POST", "/v1/settings/declarations/acme/1/confirm", "", plugin); r.Code != 403 || code(t, r) != domain.CodePermissionDenied {
		t.Fatalf("worker confirm: %d %s", r.Code, r.Body)
	}
	if r := e.do("POST", "/v1/settings/declarations/acme/1/confirm", "", e.token(auth.Delegate)); r.Code != 403 || code(t, r) != domain.CodeWebOnly {
		t.Fatalf("delegate confirm: %d %s", r.Code, r.Body)
	}
	if r := e.do("GET", "/v1/settings/declarations", "", plugin); r.Code != 403 {
		t.Fatalf("worker must not read declarations: %d", r.Code)
	}

	if r := e.web("POST", "/v1/settings/declarations/acme/x/confirm", ""); r.Code != 400 {
		t.Fatalf("bad rev: %d %s", r.Code, r.Body)
	}
	if r := e.web("POST", "/v1/settings/declarations/acme/1/confirm", ""); r.Code != 200 {
		t.Fatalf("confirm: %d %s", r.Code, r.Body)
	}
	if len(e.events(kexec.EvConfirmed)) != 1 {
		t.Fatal("confirm must be recorded")
	}
	if r := e.web("GET", "/v1/settings/executors", ""); !strings.Contains(r.Body.String(), "acme.build") {
		t.Fatalf("confirmed executor missing: %s", r.Body)
	}
	if r := e.web("GET", "/v1/settings/commands", ""); !strings.Contains(r.Body.String(), `"state":"active"`) {
		t.Fatalf("active command missing: %s", r.Body)
	}
	if r := e.web("POST", "/v1/settings/declarations/acme/1/reject", ""); r.Code != 409 {
		t.Fatalf("reject after confirm: %d %s", r.Code, r.Body)
	}

	// 直接登记命令：delegate 被拒（web_only），没有副作用；Web 登记立即生效。
	other := strings.Replace(declJSON, "acme", "evil", -1)
	if r := e.do("POST", "/v1/settings/commands", other, e.token(auth.Delegate)); r.Code != 403 || code(t, r) != domain.CodeWebOnly {
		t.Fatalf("delegate register: %d %s", r.Code, r.Body)
	}
	if r := e.web("GET", "/v1/settings/commands", ""); strings.Contains(r.Body.String(), "evil") {
		t.Fatalf("rejected registration left a trace: %s", r.Body)
	}
	if r := e.web("POST", "/v1/settings/commands", other); r.Code != 201 {
		t.Fatalf("web register: %d %s", r.Code, r.Body)
	}
	if r := e.web("GET", "/v1/settings/commands", ""); !strings.Contains(r.Body.String(), "evil") {
		t.Fatalf("registered command missing: %s", r.Body)
	}
	if r := e.web("POST", "/v1/settings/commands", `{"harness":"x","bogus":1}`); r.Code != 400 {
		t.Fatalf("unknown field: %d %s", r.Code, r.Body)
	}
	if r := e.web("POST", "/v1/settings/declarations/ghost/9/reject", ""); r.Code != 404 {
		t.Fatalf("unknown declaration: %d %s", r.Code, r.Body)
	}
}

// 执行者清单：Web 维护，带 project 时标明是否允许；声明来源的项不能被覆盖。
func TestExecutorsListPutRemove(t *testing.T) {
	e := setup(t)
	pid := e.project("acc-work")
	e.web("PUT", "/v1/settings/projects/"+pid+"/allowed-executors", `{"executors":["a.cli"]}`)

	for _, id := range []string{"a.cli", "b.cli"} {
		if r := e.web("POST", "/v1/settings/executors", `{"id":"`+id+`","kind":"external-cli"}`); r.Code != 200 {
			t.Fatalf("put %s: %d %s", id, r.Code, r.Body)
		}
	}
	if len(e.events(kexec.EvChanged)) != 2 {
		t.Fatal("list changes must be recorded")
	}
	r := e.web("GET", "/v1/settings/executors?project="+pid, "")
	var out struct {
		Executors []struct {
			ID      string `json:"id"`
			Allowed *bool  `json:"allowed"`
		}
	}
	_ = json.Unmarshal(r.Body.Bytes(), &out)
	if len(out.Executors) != 2 || out.Executors[0].Allowed == nil || !*out.Executors[0].Allowed || *out.Executors[1].Allowed {
		t.Fatalf("allowed flags wrong: %s", r.Body)
	}
	if r := e.web("GET", "/v1/settings/executors?project=nope", ""); r.Code != 404 {
		t.Fatalf("unknown project: %d", r.Code)
	}
	if r := e.do("POST", "/v1/settings/executors", `{"id":"c.cli","kind":"external-cli"}`, e.token(auth.Delegate)); r.Code != 403 || code(t, r) != domain.CodeWebOnly {
		t.Fatalf("delegate put: %d %s", r.Code, r.Body)
	}
	if r := e.do("DELETE", "/v1/settings/executors/a.cli", "", e.token(auth.Delegate)); r.Code != 403 {
		t.Fatalf("delegate remove: %d", r.Code)
	}
	if r := e.web("DELETE", "/v1/settings/executors/a.cli", ""); r.Code != 200 {
		t.Fatalf("remove: %d %s", r.Code, r.Body)
	}
	if r := e.web("DELETE", "/v1/settings/executors/a.cli", ""); r.Code != 404 {
		t.Fatalf("remove twice: %d", r.Code)
	}
	if r := e.web("POST", "/v1/settings/executors", `not json`); r.Code != 400 {
		t.Fatalf("bad body: %d", r.Code)
	}
	if len(e.events(kexec.EvRemoved)) != 1 {
		t.Fatal("removal must be recorded")
	}
}
