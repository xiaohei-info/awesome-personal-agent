package projects

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

type env struct {
	t   *testing.T
	st  *store.Store
	au  *auth.Auth
	srv *core.Server
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, kp.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, nil)
	if err := kp.Register(x); err != nil {
		t.Fatal(err)
	}
	au := auth.New(nil)
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{Routes(st, x)}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, st: st, au: au, srv: srv}
}

func (e *env) token(kind auth.Kind) string {
	e.t.Helper()
	var secret string
	spec := auth.Spec{Kind: kind}
	if kind == auth.Ext || kind == auth.Webhook {
		spec.Scope = "src_1"
	}
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		_, s, err := e.au.Issue(tx, spec)
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return secret
}

type resp struct{ *httptest.ResponseRecorder }

func (r resp) into(v any) {
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		panic("not json: " + r.Body.String())
	}
}

func (r resp) code() string {
	var m struct{ Code string }
	r.into(&m)
	return m.Code
}

func (e *env) do(secret, method, path, body string, hdr ...string) resp {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(core.EvidenceHeader, "owner: go ahead") // delegate 代理写需要证据；其他凭据不读
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-APA-Session", "s1")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return resp{rec}
}

func (e *env) project(secret, id string) domain.Project {
	e.t.Helper()
	r := e.do(secret, "GET", "/v1/projects/"+id, "")
	if r.Code != 200 {
		e.t.Fatalf("get %s: %d %s", id, r.Code, r.Body)
	}
	var p domain.Project
	r.into(&p)
	return p
}

// create 以 delegate 建项目，返回 id。
func (e *env) create(dlg string) string {
	e.t.Helper()
	r := e.do(dlg, "POST", "/v1/projects", `{"name":"Work","intro":"x","tags":["a"]}`)
	if r.Code != 201 {
		e.t.Fatalf("create: %d %s", r.Code, r.Body)
	}
	var p domain.Project
	r.into(&p)
	return p.ID
}

func TestProjectCRUDAndPermissions(t *testing.T) {
	e := setup(t)
	obs, wrk, dlg := e.token(auth.Observer), e.token(auth.Worker), e.token(auth.Delegate)

	if r := e.do(wrk, "POST", "/v1/projects", `{"name":"n"}`); r.Code != 403 || r.code() != "permission_denied" {
		t.Fatalf("worker create must be denied: %d %s", r.Code, r.Body)
	}
	if r := e.do("", "GET", "/v1/projects", ""); r.Code != 401 {
		t.Fatalf("anonymous list: %d", r.Code)
	}
	id := e.create(dlg)
	p := e.project(obs, id)
	if p.Name != "Work" || p.Status != domain.ProjectActive || p.AllowedExecutors != nil {
		t.Fatalf("created project: %+v", p)
	}

	r := e.do(dlg, "PATCH", "/v1/projects/"+id, `{"name":"Renamed","status":"paused"}`)
	if r.Code != 200 {
		t.Fatalf("patch: %d %s", r.Code, r.Body)
	}
	if p := e.project(obs, id); p.Name != "Renamed" || p.Status != domain.ProjectPaused || p.Intro != "x" {
		t.Fatalf("patched project: %+v", p)
	}
	if r := e.do(obs, "PATCH", "/v1/projects/"+id, `{"name":"z"}`); r.Code != 403 {
		t.Fatalf("observer patch: %d", r.Code)
	}
	if r := e.do(dlg, "PATCH", "/v1/projects/"+id, `{}`); r.Code != 400 {
		t.Fatalf("empty patch: %d", r.Code)
	}
	if r := e.do(dlg, "PATCH", "/v1/projects/"+id, `{"bogus":1}`); r.Code != 400 {
		t.Fatalf("unknown field: %d", r.Code)
	}
	if r := e.do(obs, "GET", "/v1/projects/prj_missing", ""); r.Code != 404 {
		t.Fatalf("missing project: %d", r.Code)
	}

	var list struct{ Projects []domain.Project }
	lr := e.do(obs, "GET", "/v1/projects", "")
	lr.into(&list)
	if lr.Code != 200 || len(list.Projects) != 1 || list.Projects[0].ID != id {
		t.Fatalf("list: %d %s", lr.Code, lr.Body)
	}
}

func TestCreateIsIdempotentWithKey(t *testing.T) {
	e := setup(t)
	dlg := e.token(auth.Delegate)
	a := e.do(dlg, "POST", "/v1/projects", `{"name":"Once"}`, "Idempotency-Key", "k1")
	b := e.do(dlg, "POST", "/v1/projects", `{"name":"Once"}`, "Idempotency-Key", "k1")
	var pa, pb domain.Project
	a.into(&pa)
	b.into(&pb)
	if a.Code != 201 || b.Code != 201 || pa.ID != pb.ID {
		t.Fatalf("replay must return the first project: %s / %s", a.Body, b.Body)
	}
	var list struct{ Projects []domain.Project }
	e.do(dlg, "GET", "/v1/projects", "").into(&list)
	if len(list.Projects) != 1 {
		t.Fatalf("replay must not create twice: %d", len(list.Projects))
	}
}

// 普通 PATCH / POST 带 allowed_executors 一律被拒绝；名单不变。
func TestAllowedExecutorsCannotBeWrittenThroughProjectRoutes(t *testing.T) {
	e := setup(t)
	dlg, wrk := e.token(auth.Delegate), e.token(auth.Worker)
	id := e.create(dlg)
	body := `{"name":"x","allowed_executors":["evil.host"]}`

	if r := e.do(dlg, "PATCH", "/v1/projects/"+id, body); r.Code != 403 || r.code() != "web_only" {
		t.Fatalf("delegate PATCH with allowed_executors must be web_only: %d %s", r.Code, r.Body)
	}
	if r := e.do(wrk, "PATCH", "/v1/projects/"+id, body); r.Code != 403 || r.code() != "permission_denied" {
		t.Fatalf("worker PATCH with allowed_executors must be permission_denied: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", "/v1/projects", `{"name":"y","allowed_executors":[]}`); r.Code != 403 || r.code() != "web_only" {
		t.Fatalf("POST with allowed_executors: %d %s", r.Code, r.Body)
	}
	if p := e.project(dlg, id); p.AllowedExecutors != nil || p.Name != "Work" {
		t.Fatalf("rejected request must change nothing: %+v", p)
	}
	// 内核管线为 delegate 的尝试留拒绝事件。
	var rejected int
	if err := e.st.View(context.Background(), func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, en := range es {
			if en.Event.Type == domain.EvCommandRejected && en.Event.Object == id {
				rejected++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rejected != 1 {
		t.Fatalf("want exactly 1 rejection event, got %d", rejected)
	}
}

func TestWebOwnerPatchWithAllowedExecutorsIsStillRejected(t *testing.T) {
	e := setup(t)
	dlg := e.token(auth.Delegate)
	id := e.create(dlg)

	login := httptest.NewRequest("POST", "/v1/auth/login", strings.NewReader(`{"password":"`+ownerPW+`"}`))
	login.RemoteAddr = "127.0.0.1:50000" // plain-HTTP login is only accepted from this host
	lrec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(lrec, login)
	lr := resp{lrec}
	if lr.Code != 200 {
		t.Fatalf("login: %d %s", lr.Code, lr.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	lr.into(&out)
	req := httptest.NewRequest("PATCH", "/v1/projects/"+id, strings.NewReader(`{"allowed_executors":[]}`))
	req.AddCookie(lr.Result().Cookies()[0])
	req.Header.Set(core.CSRFHeader, out.CSRF)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "settings/projects/") {
		t.Fatalf("web PATCH must point to the settings route: %d %s", rec.Code, rec.Body)
	}
	if p := e.project(dlg, id); p.AllowedExecutors != nil {
		t.Fatalf("allowed_executors changed: %+v", p.AllowedExecutors)
	}
}

func TestReposWorkerEditsCheckoutsDelegateEditsRemotes(t *testing.T) {
	e := setup(t)
	dlg, wrk, obs := e.token(auth.Delegate), e.token(auth.Worker), e.token(auth.Observer)
	id := e.create(dlg)
	path := "/v1/projects/" + id + "/repos"
	const remote = "git@github.com:me/app.git"

	// worker 不能增删远程地址
	for _, body := range []string{
		`{"op":"add_remote","remote":"` + remote + `"}`,
		`{"op":"remove_remote","remote":"` + remote + `"}`,
	} {
		if r := e.do(wrk, "POST", path, body); r.Code != 403 {
			t.Fatalf("worker %s: %d %s", body, r.Code, r.Body)
		}
	}
	if r := e.do(dlg, "POST", path, `{"op":"add_remote","remote":"`+remote+`"}`); r.Code != 200 {
		t.Fatalf("delegate add_remote: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", path, `{"op":"add_remote","remote":"`+remote+`"}`); r.Code != 409 {
		t.Fatalf("duplicate remote: %d %s", r.Code, r.Body)
	}
	// worker 可增改本机检出
	co := `{"op":"set_checkout","remote":"` + remote + `","host":"mac","path":"/src/app"}`
	r := e.do(wrk, "POST", path, co)
	var p domain.Project
	r.into(&p)
	if r.Code != 200 || len(p.Repos) != 1 || len(p.Repos[0].Checkouts) != 1 || p.Repos[0].Checkouts[0].Path != "/src/app" {
		t.Fatalf("worker set_checkout: %d %s", r.Code, r.Body)
	}
	if r := e.do(wrk, "POST", path, `{"op":"set_checkout","remote":"`+remote+`","host":"mac","path":"relative"}`); r.Code != 400 {
		t.Fatalf("relative path: %d", r.Code)
	}
	if r := e.do(obs, "POST", path, co); r.Code != 403 {
		t.Fatalf("observer set_checkout: %d", r.Code)
	}
	if r := e.do(wrk, "POST", path, `{"op":"nope"}`); r.Code != 400 {
		t.Fatalf("unknown op: %d", r.Code)
	}
	if r := e.do(wrk, "POST", path, `{"remote":"x"}`); r.Code != 400 {
		t.Fatalf("missing op: %d", r.Code)
	}
	if r := e.do(wrk, "POST", "/v1/projects/prj_missing/repos", co); r.Code != 404 {
		t.Fatalf("missing project: %d", r.Code)
	}
	if r := e.do(wrk, "POST", path, `{"op":"remove_checkout","remote":"`+remote+`","host":"mac","path":"/src/app"}`); r.Code != 200 {
		t.Fatalf("remove_checkout: %d %s", r.Code, r.Body)
	}
	if got := e.project(obs, id); len(got.Repos[0].Checkouts) != 0 {
		t.Fatalf("checkout not removed: %+v", got.Repos)
	}
	if r := e.do(dlg, "POST", path, `{"op":"remove_remote","remote":"`+remote+`"}`); r.Code != 200 {
		t.Fatalf("remove_remote: %d %s", r.Code, r.Body)
	}
	if got := e.project(obs, id); len(got.Repos) != 0 {
		t.Fatalf("remote not removed: %+v", got.Repos)
	}
}

func TestResolveMapsDirectoryToProject(t *testing.T) {
	e := setup(t)
	dlg, wrk, obs := e.token(auth.Delegate), e.token(auth.Worker), e.token(auth.Observer)
	id := e.create(dlg)
	path := "/v1/projects/" + id + "/repos"
	e.do(dlg, "POST", path, `{"op":"add_remote","remote":"git@h:a/b.git"}`)
	e.do(wrk, "POST", path, `{"op":"set_checkout","remote":"git@h:a/b.git","host":"mac","path":"/src/app"}`)

	resolve := func(q string) (int, struct{ Project *domain.Project }) {
		r := e.do(obs, "GET", "/v1/projects/resolve?"+q, "")
		var out struct{ Project *domain.Project }
		r.into(&out)
		return r.Code, out
	}
	if c, out := resolve("path=/src/app/pkg/x&host=mac"); c != 200 || out.Project == nil || out.Project.ID != id {
		t.Fatalf("inside checkout: %d %+v", c, out)
	}
	if c, out := resolve("path=/src/other&host=mac"); c != 200 || out.Project != nil {
		t.Fatalf("outside checkout must be no project: %d %+v", c, out)
	}
	if c, out := resolve("path=/src/app&host=other"); c != 200 || out.Project != nil {
		t.Fatalf("other host must not match: %d %+v", c, out)
	}
	if c, out := resolve("project=" + id); c != 200 || out.Project == nil {
		t.Fatalf("explicit project: %d %+v", c, out)
	}
	if c, _ := resolve("project=prj_missing"); c != 404 {
		t.Fatalf("explicit missing project must not fall back: %d", c)
	}
	if c, _ := resolve("path=/src/app"); c != 400 {
		t.Fatalf("path without host: %d", c)
	}
	if c, _ := resolve("path=rel&host=mac"); c != 400 {
		t.Fatalf("relative path: %d", c)
	}
}

func TestMethodsOutsideContractAreNotRouted(t *testing.T) {
	e := setup(t)
	dlg := e.token(auth.Delegate)
	if r := e.do(dlg, "DELETE", "/v1/projects/prj_x", ""); r.Code != http.StatusNotFound && r.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: %d", r.Code)
	}
}

// ext 与 webhook 凭据另有用途，不能读项目账本（与 api/items 一致）。
func TestExtAndWebhookCredentialsCannotReadProjects(t *testing.T) {
	e := setup(t)
	dlg := e.token(auth.Delegate)
	id := e.create(dlg)
	for _, kind := range []auth.Kind{auth.Ext, auth.Webhook} {
		tok := e.token(kind)
		for _, path := range []string{"/v1/projects", "/v1/projects/" + id, "/v1/projects/resolve?project=" + id} {
			r := e.do(tok, "GET", path, "")
			if r.Code != 403 || r.code() != "permission_denied" || strings.Contains(r.Body.String(), "Work") {
				t.Fatalf("%s GET %s: %d %s", kind, path, r.Code, r.Body)
			}
		}
	}
	for _, kind := range []auth.Kind{auth.Observer, auth.Worker, auth.Delegate} {
		if r := e.do(e.token(kind), "GET", "/v1/projects/"+id, ""); r.Code != 200 {
			t.Fatalf("%s must still read: %d", kind, r.Code)
		}
	}
}
