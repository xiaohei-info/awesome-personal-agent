package outbound

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
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
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
	for _, m := range []store.Module{eventlog.Module, auth.Module, command.Module, core.Module, kp.Module, kpolicy.Module, registry.Module} {
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
	if err := kp.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(e.x, registry.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	if e.srv, err = core.New(core.Deps{Store: st, Auth: e.au, Groups: []core.RouteGroup{Routes(st, e.x)}}); err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
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
	web := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	for cmd, in := range map[string]string{
		registry.CmdRegisterExec: `{"id":"src_obd","command":"echo","schedule":"1m","timeout":30}`,
		"project.create":         `{"name":"acc-open","status":"active","capture":true}`,
	} {
		if _, err := e.x.Do(bg, command.Request{Identity: web, Command: cmd, Input: json.RawMessage(in)}); err != nil {
			t.Fatal(err)
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
		_, secret, err = e.au.Issue(tx, auth.Spec{Kind: kind})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("X-Apa-On-Behalf-Of", "owner")
	}
}

func (e *env) projectID() string {
	e.t.Helper()
	var id string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		ps, err := kp.List(tx)
		if len(ps) > 0 {
			id = ps[0].ID
		}
		return err
	}); err != nil || id == "" {
		e.t.Fatalf("no project: %v", err)
	}
	return id
}

func (e *env) source() domain.Source {
	e.t.Helper()
	var s domain.Source
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { s, err = registry.Get(tx, "src_obd"); return }); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// 验收 ACT-OBD-10：外发范围不能用凭据写——三个入口的 delegate 请求都是 403，范围不变。
func TestCredentialsCannotWriteOutbound(t *testing.T) {
	e := setup(t)
	proj := e.projectID()
	for _, tc := range []struct{ path, body string }{
		{"/v1/settings/sources/src_obd/outbound", `{"local":"full","external":"full"}`},
		{"/v1/settings/projects/" + proj + "/capture-outbound", `{"value":"full"}`},
	} {
		for _, kind := range []auth.Kind{auth.Delegate, auth.Worker, auth.Observer} {
			rec := e.do("PUT", tc.path, tc.body, e.token(kind))
			if rec.Code != http.StatusForbidden {
				t.Errorf("PUT %s as %s: %d %s", tc.path, kind, rec.Code, rec.Body)
			}
		}
		if rec := e.do("GET", tc.path, "", e.token(auth.Delegate)); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s as delegate: %d", tc.path, rec.Code)
		}
	}
	if s := e.source(); s.Outbound.External != nil {
		t.Fatalf("source outbound changed: %+v", s.Outbound)
	}
	var p domain.Project
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { p, err = kp.Get(tx, proj); return })
	if p.CaptureOutbound != nil {
		t.Fatalf("capture_outbound changed: %v", *p.CaptureOutbound)
	}
}

// Web 会话：读到未选择（null），选定后读回，改回 null，非法取值 400。
func TestOwnerSetsAndReadsOutbound(t *testing.T) {
	e := setup(t)
	path := "/v1/settings/sources/src_obd/outbound"
	rec := e.web("GET", path, "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"local":"full","external":null}` {
		t.Fatalf("initial: %d %s", rec.Code, rec.Body)
	}
	if rec := e.web("PUT", path, `{"local":"full","external":"excerpt"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"external":"excerpt"`) {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if s := e.source(); s.Outbound.External == nil || *s.Outbound.External != "excerpt" {
		t.Fatalf("source %+v", s.Outbound)
	}
	if rec := e.web("PUT", path, `{"external":null}`); rec.Code != 200 || e.source().Outbound.External != nil {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{`{"external":"everything"}`, `{"local":null}`, `[]`} {
		if rec := e.web("PUT", path, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d", bad, rec.Code)
		}
	}
	if rec := e.web("PUT", "/v1/settings/sources/nope/outbound", `{"external":"full"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown source: %d", rec.Code)
	}

	pp := "/v1/settings/projects/" + e.projectID() + "/capture-outbound"
	if rec := e.web("GET", pp, ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"value":null}` {
		t.Fatalf("project initial: %d %s", rec.Code, rec.Body)
	}
	if rec := e.web("PUT", pp, `{"value":"meta"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"value":"meta"`) {
		t.Fatalf("project put: %d %s", rec.Code, rec.Body)
	}
	if rec := e.web("PUT", pp, `{"value":"all"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("project bad value: %d", rec.Code)
	}
	if rec := e.web("PUT", pp, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("project missing value: %d", rec.Code)
	}
	// 项目的其他更新不会把已选的范围冲回未选择
	if _, err := e.x.Do(bg, command.Request{Identity: command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb},
		Command: "project.update", Input: json.RawMessage(`{"id":"` + e.projectID() + `","intro":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	if rec := e.web("GET", pp, ""); !strings.Contains(rec.Body.String(), `"value":"meta"`) {
		t.Fatalf("capture_outbound lost by an unrelated update: %s", rec.Body)
	}
}
