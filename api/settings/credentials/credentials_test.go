package credentials_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/settings/credentials"
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
	t    *testing.T
	st   *store.Store
	auth *auth.Auth
	srv  *core.Server
	// 登录后的 Web 会话
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
	now := func() time.Time { return time.Now() }
	e := &env{t: t, st: st, auth: auth.New(now)}
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	x := command.New(st, now)
	group, err := credentials.Mount(x, e.auth)
	if err != nil {
		t.Fatal(err)
	}
	echo := func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/echo", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			return core.WriteJSON(w, 200, map[string]any{"level": p.Caller.Level})
		}})
	}
	if e.srv, err = core.New(core.Deps{Store: st, Auth: e.auth, Groups: []core.RouteGroup{group, echo}}); err != nil {
		t.Fatal(err)
	}
	r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if r.Code != 200 {
		t.Fatalf("login: %d %s", r.Code, r.Body)
	}
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

func bearer(secret string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

type issued struct {
	Credential auth.Credential `json:"credential"`
	Secret     string          `json:"secret"`
}

func (e *env) issue(path, body string) issued {
	e.t.Helper()
	r := e.web("POST", path, body)
	if r.Code != 201 {
		e.t.Fatalf("issue %s: %d %s", body, r.Code, r.Body)
	}
	var out issued
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || out.Secret == "" {
		e.t.Fatalf("issue response lacks secret: %s", r.Body)
	}
	return out
}

func (e *env) list(path string) []auth.Credential {
	e.t.Helper()
	r := e.web("GET", path, "")
	if r.Code != 200 {
		e.t.Fatalf("list: %d %s", r.Code, r.Body)
	}
	var out struct {
		Credentials []auth.Credential `json:"credentials"`
	}
	json.Unmarshal(r.Body.Bytes(), &out)
	return out.Credentials
}

func errCode(t *testing.T, r *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var e domain.Error
	if err := json.Unmarshal(r.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %s", r.Body)
	}
	return e.Code
}

// 首次使用：Web 登录后签发 token，明文只回一次，吊销后立即失效。
func TestBootstrapIssueListRevokeToken(t *testing.T) {
	e := setup(t)
	tok := e.issue("/v1/settings/tokens", `{"kind":"worker","label":"codex","executor":"work-host.codex"}`)
	if !strings.HasPrefix(tok.Secret, "apa_wrk_") || tok.Credential.Executor != "work-host.codex" || tok.Credential.Label != "codex" {
		t.Fatalf("unexpected issue result: %+v", tok)
	}
	if r := e.do("GET", "/v1/echo", "", bearer(tok.Secret)); r.Code != 200 {
		t.Fatalf("issued token must authenticate: %d %s", r.Code, r.Body)
	}

	// 列表可见登记，但永远没有明文或哈希。
	r := e.web("GET", "/v1/settings/tokens", "")
	if strings.Contains(r.Body.String(), tok.Secret) || strings.Contains(r.Body.String(), "hash") {
		t.Fatalf("list leaks secret or hash: %s", r.Body)
	}
	if l := e.list("/v1/settings/tokens"); len(l) != 1 || l[0].ID != tok.Credential.ID {
		t.Fatalf("list = %+v", l)
	}

	// 账本里只有哈希：明文不在凭据表，也不在命令幂等结果里。
	e.st.View(context.Background(), func(tx *store.Tx) error {
		for _, q := range []string{"SELECT hash FROM auth_credential", "SELECT result FROM command_idem"} {
			rows, err := tx.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var s string
				rows.Scan(&s)
				if strings.Contains(s, tok.Secret) {
					t.Fatalf("plaintext stored by %q", q)
				}
			}
			rows.Close()
		}
		return nil
	})

	// 吊销立即生效，重复吊销幂等并保留首次时间。
	d := e.web("DELETE", "/v1/settings/tokens/"+tok.Credential.ID, "")
	if d.Code != 200 {
		t.Fatalf("revoke: %d %s", d.Code, d.Body)
	}
	first := e.list("/v1/settings/tokens")[0].RevokedAt
	if first.IsZero() {
		t.Fatal("revoked credential must show revoked_at in the list")
	}
	if r := e.do("GET", "/v1/echo", "", bearer(tok.Secret)); r.Code != 401 {
		t.Fatalf("revoked token must be rejected at once, got %d", r.Code)
	}
	if d := e.web("DELETE", "/v1/settings/tokens/"+tok.Credential.ID, ""); d.Code != 200 {
		t.Fatalf("repeat revoke: %d", d.Code)
	}
	if again := e.list("/v1/settings/tokens")[0].RevokedAt; !again.Equal(first) {
		t.Fatalf("repeat revoke moved revoked_at: %v -> %v", first, again)
	}
}

// 授权矩阵：这些路由只接受 Web 会话。
func TestOnlyWebSessionMayManageCredentials(t *testing.T) {
	e := setup(t)
	target := e.issue("/v1/settings/tokens", `{"kind":"observer"}`)
	ext := e.issue("/v1/settings/ext-keys", `{"scope":"channel","label":"tg"}`)
	obs, wrk, dlg := target.Secret, e.issue("/v1/settings/tokens", `{"kind":"worker"}`).Secret, e.issue("/v1/settings/tokens", `{"kind":"delegate"}`).Secret

	paths := []struct{ method, path, body string }{
		{"GET", "/v1/settings/tokens", ""},
		{"POST", "/v1/settings/tokens", `{"kind":"worker"}`},
		{"DELETE", "/v1/settings/tokens/" + target.Credential.ID, ""},
		{"GET", "/v1/settings/ext-keys", ""},
		{"POST", "/v1/settings/ext-keys", `{"scope":"x"}`},
		{"DELETE", "/v1/settings/ext-keys/" + ext.Credential.ID, ""},
	}
	before := len(e.list("/v1/settings/tokens")) + len(e.list("/v1/settings/ext-keys"))
	for _, p := range paths {
		for name, c := range map[string]struct {
			mod  func(*http.Request)
			want domain.Code
			http int
		}{
			"none":     {nil, domain.CodeUnauthenticated, 401},
			"bad":      {bearer("apa_wrk_nope"), domain.CodeUnauthenticated, 401},
			"observer": {bearer(obs), domain.CodePermissionDenied, 403},
			"worker":   {bearer(wrk), domain.CodePermissionDenied, 403},
			"delegate": {bearer(dlg), domain.CodeWebOnly, 403}, // 退出码 20
			"ext":      {bearer(ext.Secret), domain.CodePermissionDenied, 403},
		} {
			r := e.do(p.method, p.path, p.body, c.mod)
			if r.Code != c.http || errCode(t, r) != c.want {
				t.Errorf("%s %s as %s: %d %s, want %d %s", p.method, p.path, name, r.Code, r.Body, c.http, c.want)
			}
		}
	}
	if after := len(e.list("/v1/settings/tokens")) + len(e.list("/v1/settings/ext-keys")); after != before {
		t.Fatalf("a refused caller changed credentials: %d -> %d", before, after)
	}
	for _, c := range append(e.list("/v1/settings/tokens"), e.list("/v1/settings/ext-keys")...) {
		if !c.RevokedAt.IsZero() {
			t.Fatalf("a refused caller revoked %s", c.ID)
		}
	}
	// delegate 的越权尝试在账本留痕（退出码 20）。
	var rejected int
	e.st.View(context.Background(), func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, "settings:credentials")
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range es {
			if x.Event.Type == domain.EvCommandRejected {
				rejected++
			}
		}
		return nil
	})
	if rejected != 6 {
		t.Fatal("each of the 6 delegate attempts must leave a command.rejected event")
	}

	// Cookie 写请求缺 CSRF 令牌被拒。
	r := e.do("POST", "/v1/settings/tokens", `{"kind":"worker"}`, func(r *http.Request) { r.AddCookie(e.cookie) })
	if r.Code != 403 {
		t.Fatalf("missing CSRF: %d %s", r.Code, r.Body)
	}
}

func TestExtKeysAndTokensStaySeparate(t *testing.T) {
	e := setup(t)
	tok := e.issue("/v1/settings/tokens", `{"kind":"observer"}`)
	ext := e.issue("/v1/settings/ext-keys", `{"scope":"channel.telegram","label":"tg"}`)
	if !strings.HasPrefix(ext.Secret, "apa_ext_") || ext.Credential.Kind != auth.Ext || ext.Credential.Scope != "channel.telegram" {
		t.Fatalf("ext key = %+v", ext)
	}
	if l := e.list("/v1/settings/ext-keys"); len(l) != 1 || l[0].ID != ext.Credential.ID {
		t.Fatalf("ext-keys list = %+v", l)
	}
	if l := e.list("/v1/settings/tokens"); len(l) != 1 || l[0].ID != tok.Credential.ID {
		t.Fatalf("tokens list = %+v", l)
	}
	cases := []struct {
		name, method, path, body string
		code                     int
	}{
		{"ext key needs scope", "POST", "/v1/settings/ext-keys", `{}`, 400},
		{"token cannot be ext", "POST", "/v1/settings/tokens", `{"kind":"ext","scope":"x"}`, 400},
		{"webhook is not a token", "POST", "/v1/settings/tokens", `{"kind":"webhook","scope":"s"}`, 400},
		{"token kind required", "POST", "/v1/settings/tokens", `{}`, 400},
		{"token scope refused", "POST", "/v1/settings/tokens", `{"kind":"worker","scope":"x"}`, 400},
		{"executor only for worker", "POST", "/v1/settings/tokens", `{"kind":"observer","executor":"a.b"}`, 400},
		{"malformed body", "POST", "/v1/settings/tokens", `{`, 400},
		{"tokens route cannot revoke ext key", "DELETE", "/v1/settings/tokens/" + ext.Credential.ID, "", 404},
		{"ext-keys route cannot revoke token", "DELETE", "/v1/settings/ext-keys/" + tok.Credential.ID, "", 404},
		{"unknown id", "DELETE", "/v1/settings/tokens/cred_nope", "", 404},
	}
	for _, c := range cases {
		if r := e.web(c.method, c.path, c.body); r.Code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, r.Code, r.Body, c.code)
		}
	}
	// ext-keys 路由无视请求体里的 kind，只会签发 ext。
	if x := e.issue("/v1/settings/ext-keys", `{"kind":"delegate","scope":"s"}`); x.Credential.Kind != auth.Ext {
		t.Fatalf("ext-keys issued %s", x.Credential.Kind)
	}
	if r := e.web("DELETE", "/v1/settings/ext-keys/"+ext.Credential.ID, ""); r.Code != 200 {
		t.Fatalf("revoke ext key: %d %s", r.Code, r.Body)
	}
	if r := e.do("GET", "/v1/echo", "", bearer(ext.Secret)); r.Code != 401 {
		t.Fatalf("revoked ext key must be rejected, got %d", r.Code)
	}
}

func TestMountRejectsDuplicateRegistration(t *testing.T) {
	e := setup(t)
	x := command.New(e.st, nil)
	if _, err := credentials.Mount(x, e.auth); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Mount(x, e.auth); err == nil {
		t.Fatal("mounting twice on one executor must fail")
	}
}
