package retention

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
	for _, m := range []store.Module{eventlog.Module, auth.Module, command.Module, core.Module, kpolicy.Module, registry.Module} {
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
	if err := registry.Register(e.x, registry.Options{Declared: func(s domain.Source) string { return s.ID }}); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	if e.srv, err = core.New(core.Deps{Store: st, Auth: e.au, Groups: []core.RouteGroup{Routes(e.x)}}); err != nil {
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
		registry.CmdRegisterExec: `{"id":"src_ret","command":"echo","schedule":"1m","timeout":30}`,
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

func (e *env) days() *int {
	e.t.Helper()
	var s domain.Source
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { s, err = registry.Get(tx, "src_ret"); return }); err != nil {
		e.t.Fatal(err)
	}
	return s.ContentRetentionDays
}

func (e *env) updates() (n int) {
	e.t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, en := range es {
			if en.Event.Type == registry.EvUpdated {
				n++
			}
		}
		return err
	})
	return
}

// ACT-RET-01：Web 能设、读回，并记一条 Event；null 读回为空（不清理）；非法取值 400；来源不存在 404。
func TestOwnerSetsContentRetention(t *testing.T) {
	e := setup(t)
	path := "/v1/settings/sources/src_ret/content-retention"
	before := e.updates()
	rec := e.web("PUT", path, `{"days":1}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"days":1}` {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if d := e.days(); d == nil || *d != 1 || e.updates() != before+1 {
		t.Fatalf("days=%v updates=%d (was %d)", d, e.updates(), before)
	}
	if rec := e.web("PUT", path, `{"days":null}`); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"days":null}` || e.days() != nil {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{`{"days":0}`, `{"days":"7"}`, `{}`, `[]`, `{"days":2,"x":1}`} {
		if rec := e.web("PUT", path, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s: %d", bad, rec.Code)
		}
	}
	if rec := e.web("PUT", "/v1/settings/sources/nope/content-retention", `{"days":3}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown source: %d", rec.Code)
	}
}

// ACT-RET-01：凭据不能设——delegate、worker、observer 都被拒（403），天数不变。
func TestCredentialsCannotSetContentRetention(t *testing.T) {
	e := setup(t)
	for _, kind := range []auth.Kind{auth.Delegate, auth.Worker, auth.Observer} {
		if rec := e.do("PUT", "/v1/settings/sources/src_ret/content-retention", `{"days":1}`, e.token(kind)); rec.Code != http.StatusForbidden {
			t.Errorf("PUT as %s: %d %s", kind, rec.Code, rec.Body)
		}
	}
	if e.days() != nil {
		t.Fatal("retention changed by a credential")
	}
}
