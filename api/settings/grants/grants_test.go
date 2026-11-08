package grants_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/settings/grants"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, auth: auth.New(nil)}
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	group, err := grants.Mount(command.New(st, time.Now), e.auth, st)
	if err != nil {
		t.Fatal(err)
	}
	if e.srv, err = core.New(core.Deps{Store: st, Auth: e.auth, Groups: []core.RouteGroup{group}}); err != nil {
		t.Fatal(err)
	}
	r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(r.Body.Bytes(), &out)
	e.csrf = out.CSRF
	for _, c := range r.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	return e
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
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

func as(secret string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

var scopes = map[auth.Kind]string{auth.Ext: "export", auth.Webhook: "src1"}

func (e *env) token(k auth.Kind) (auth.Credential, string) {
	e.t.Helper()
	var (
		c auth.Credential
		s string
	)
	if err := e.st.Update(context.Background(), func(tx *store.Tx) (err error) {
		c, s, err = e.auth.Issue(tx, auth.Spec{Kind: k, Label: string(k), Scope: scopes[k]})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return c, s
}

func code(t *testing.T, r *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var d domain.Error
	if err := json.Unmarshal(r.Body.Bytes(), &d); err != nil {
		t.Fatalf("not an error body: %s", r.Body)
	}
	return d.Code
}

func body(cred string, days int) string {
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
	return `{"grantee":{"credential":"` + cred + `"},"scope":{"projects":["p1"]},"actions":["decision.answer"],"expires_at":"` + exp + `","reason":"away"}`
}

func (e *env) mine(secret string) []policy.Grant {
	e.t.Helper()
	r := e.do("GET", "/v1/grants", "", as(secret))
	if r.Code != 200 {
		e.t.Fatalf("GET /grants: %d %s", r.Code, r.Body)
	}
	var out struct {
		Grants []policy.Grant `json:"grants"`
	}
	json.Unmarshal(r.Body.Bytes(), &out)
	return out.Grants
}

func TestOwnerIssuesListsAndRevokesGrantAndDelegateSeesOnlyOwn(t *testing.T) {
	e := setup(t)
	d1, s1 := e.token(auth.Delegate)
	d2, s2 := e.token(auth.Delegate)

	r := e.web("POST", "/v1/settings/grants", body(d1.ID, 7))
	if r.Code != 201 {
		t.Fatalf("issue: %d %s", r.Code, r.Body)
	}
	var g policy.Grant
	json.Unmarshal(r.Body.Bytes(), &g)
	if g.ID == "" || g.Grantee.Credential != d1.ID || g.Principal == "" {
		t.Fatalf("%+v", g)
	}
	if r := e.web("POST", "/v1/settings/grants", body(d2.ID, 7)); r.Code != 201 {
		t.Fatalf("issue d2: %d %s", r.Code, r.Body)
	}

	if got := e.mine(s1); len(got) != 1 || got[0].ID != g.ID {
		t.Fatalf("d1 must see exactly its own grant: %+v", got)
	}
	if got := e.mine(s2); len(got) != 1 || got[0].ID == g.ID {
		t.Fatalf("d2 must not see d1's grant: %+v", got)
	}

	r = e.web("GET", "/v1/settings/grants", "")
	var all struct {
		Grants []policy.Grant `json:"grants"`
	}
	json.Unmarshal(r.Body.Bytes(), &all)
	if r.Code != 200 || len(all.Grants) != 2 {
		t.Fatalf("web list: %d %s", r.Code, r.Body)
	}

	if r := e.web("POST", "/v1/settings/grants/"+g.ID+"/revoke", `{"reason":"back home"}`); r.Code != 200 {
		t.Fatalf("revoke: %d %s", r.Code, r.Body)
	}
	if got := e.mine(s1); len(got) != 0 {
		t.Fatalf("revoked grant must disappear from GET /grants: %+v", got)
	}
	r = e.web("GET", "/v1/settings/grants", "")
	json.Unmarshal(r.Body.Bytes(), &all)
	if len(all.Grants) != 2 || all.Grants[0].RevokedAt.IsZero() {
		t.Fatalf("Web keeps the revoked grant for audit: %s", r.Body)
	}

	var types []string
	e.st.View(context.Background(), func(tx *store.Tx) error {
		es, _ := eventlog.Read(tx, 0, 100)
		for _, en := range es {
			if en.Event.Grant != "" {
				types = append(types, en.Event.Type)
			}
		}
		return nil
	})
	if strings.Join(types, ",") != "grant.issued,grant.issued,grant.revoked" {
		t.Fatalf("events: %v", types)
	}
}

func TestRevokingTheTokenHidesItsGrants(t *testing.T) {
	e := setup(t)
	c, secret := e.token(auth.Delegate)
	e.web("POST", "/v1/settings/grants", body(c.ID, 7))
	// 另一个 delegate 仍有效：吊销 c 后，c 自己已无法认证（3），授权单也不再对任何人可见。
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error { return e.auth.Revoke(tx, c.ID) }); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/grants", "", as(secret)); r.Code != 401 {
		t.Fatalf("revoked token: %d", r.Code)
	}
	_, other := e.token(auth.Delegate)
	if got := e.mine(other); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestOnlyWebSessionCanIssueOrRevoke(t *testing.T) {
	e := setup(t)
	d, ds := e.token(auth.Delegate)
	_, ws := e.token(auth.Worker)
	_, os := e.token(auth.Observer)

	for name, tc := range map[string]struct {
		secret string
		want   domain.Code
	}{"delegate": {ds, domain.CodeWebOnly}, "worker": {ws, domain.CodePermissionDenied}, "observer": {os, domain.CodePermissionDenied}} {
		if r := e.do("POST", "/v1/settings/grants", body(d.ID, 7), as(tc.secret)); r.Code != 403 || code(t, r) != tc.want {
			t.Errorf("%s issue: %d %s", name, r.Code, r.Body)
		}
		if r := e.do("POST", "/v1/settings/grants/grt_x/revoke", `{}`, as(tc.secret)); r.Code != 403 || code(t, r) != tc.want {
			t.Errorf("%s revoke: %d %s", name, r.Code, r.Body)
		}
		if r := e.do("GET", "/v1/settings/grants", "", as(tc.secret)); r.Code != 403 {
			t.Errorf("%s list: %d", name, r.Code)
		}
	}
	if e.web("POST", "/v1/settings/grants", body(d.ID, 7)).Code != 201 {
		t.Fatal("setup")
	}
	if got := e.mine(ds); len(got) != 1 {
		t.Fatalf("agents must not have created or revoked anything: %+v", got)
	}
}

func TestGetGrantsIsDelegateOnly(t *testing.T) {
	e := setup(t)
	for _, k := range []auth.Kind{auth.Observer, auth.Worker, auth.Ext, auth.Webhook} {
		_, s := e.token(k)
		if r := e.do("GET", "/v1/grants", "", as(s)); r.Code != 403 || code(t, r) != domain.CodePermissionDenied {
			t.Errorf("%s: %d %s", k, r.Code, r.Body)
		}
	}
	if r := e.web("GET", "/v1/grants", ""); r.Code != 403 || code(t, r) != domain.CodePermissionDenied {
		t.Errorf("web: %d %s", r.Code, r.Body)
	}
	if r := e.do("GET", "/v1/grants", "", nil); r.Code != 401 {
		t.Errorf("anonymous: %d", r.Code)
	}
}

func TestIssueValidationFailuresAreInvalidRequestAndWriteNothing(t *testing.T) {
	e := setup(t)
	d, _ := e.token(auth.Delegate)
	w, _ := e.token(auth.Worker)
	for name, b := range map[string]string{
		"no expiry":      `{"grantee":{"credential":"` + d.ID + `"},"scope":{"projects":["p1"]},"actions":["decision.answer"]}`,
		"over 90 days":   body(d.ID, 91),
		"worker grantee": body(w.ID, 7),
		"wildcard":       strings.Replace(body(d.ID, 7), `"decision.answer"`, `"*"`, 1),
		"empty scope":    strings.Replace(body(d.ID, 7), `"projects":["p1"]`, `"projects":[]`, 1),
		"not json":       `{`,
	} {
		if r := e.web("POST", "/v1/settings/grants", b); r.Code != 400 || code(t, r) != domain.CodeInvalidRequest {
			t.Errorf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	if r := e.web("POST", "/v1/settings/grants/grt_missing/revoke", `{}`); r.Code != 404 {
		t.Errorf("revoke unknown: %d", r.Code)
	}
	var n int
	e.st.View(context.Background(), func(tx *store.Tx) error { g, _ := policy.ListGrants(tx); n = len(g); return nil })
	if n != 0 {
		t.Fatalf("rejected requests must write nothing, got %d grants", n)
	}
}
