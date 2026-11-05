package core

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

var tlsState = tls.ConnectionState{}

type env struct {
	t    *testing.T
	st   *store.Store
	auth *auth.Auth
	now  time.Time
	srv  *Server
}

// setup 建真实账本、设置本人口令，并注册两条测试路由：/v1/echo（GET/POST，返回调用者）与 /v1/public（无需凭据）。
func setup(t *testing.T, groups ...RouteGroup) *env {
	t.Helper()
	testIterations = 1000
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Unix(1_700_000_000, 0)}
	clock := func() time.Time { return e.now }
	e.auth = auth.New(clock)
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	echo := func(w http.ResponseWriter, r *http.Request, p *Principal) error {
		return WriteJSON(w, 200, map[string]any{"principal": p.Caller.Actor.Principal, "via": p.Caller.Actor.Via, "level": p.Caller.Level, "web": p.Web})
	}
	base := func(r *Router) {
		r.Handle(Route{Method: "GET", Path: "/echo", Handler: echo})
		r.Handle(Route{Method: "POST", Path: "/echo", Handler: echo})
		r.Handle(Route{Method: "GET", Path: "/public", Auth: AuthNone, Handler: func(w http.ResponseWriter, r *http.Request, p *Principal) error {
			return WriteJSON(w, 200, map[string]bool{"anon": p == nil})
		}})
		r.Handle(Route{Method: "GET", Path: "/boom", Handler: func(http.ResponseWriter, *http.Request, *Principal) error {
			return errors.New("secret-db-path /home/u/ledger.db exploded")
		}})
		r.Handle(Route{Method: "GET", Path: "/denied", Handler: func(http.ResponseWriter, *http.Request, *Principal) error {
			e := domain.NewError(domain.CodeExecutorNotAllowed, "executor is outside the project allowlist", "pick one of allowed")
			e.Allowed = []string{"work-host.codex"}
			return e
		}})
		r.Handle(Route{Method: "GET", Path: "/panic", Handler: func(http.ResponseWriter, *http.Request, *Principal) error { panic("kaboom") }})
		r.Handle(Route{Method: "POST", Path: "/small", MaxBody: 16, Handler: func(w http.ResponseWriter, r *http.Request, p *Principal) error {
			var v map[string]any
			if err := DecodeJSON(r, &v); err != nil {
				return err
			}
			return WriteJSON(w, 200, v)
		}})
		r.HandleRoot(Route{Method: "POST", Path: "/ingest/{source_id}", Auth: AuthNone, Handler: func(w http.ResponseWriter, r *http.Request, p *Principal) error {
			return WriteJSON(w, 200, map[string]string{"source": r.PathValue("source_id")})
		}})
	}
	srv, err := New(Deps{Store: st, Auth: e.auth, Now: clock, Groups: append([]RouteGroup{base}, groups...)})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	return e
}

type resp struct {
	*httptest.ResponseRecorder
}

func (r resp) json(v any) {
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		r.Result() // keep recorder usable
		panic("not json: " + r.Body.String())
	}
}

func (r resp) cookie(name string) *http.Cookie {
	for _, c := range r.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (e *env) do(method, path, body string, mod func(*http.Request)) resp {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "127.0.0.1:50000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return resp{rec}
}

// login 以本人口令登录，返回会话 Cookie 与 CSRF 令牌。
func (e *env) login() (*http.Cookie, string) {
	e.t.Helper()
	r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if r.Code != 200 {
		e.t.Fatalf("login: %d %s", r.Code, r.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	r.json(&out)
	c := r.cookie(SessionCookie)
	if c == nil || out.CSRF == "" {
		e.t.Fatalf("login must set cookie and csrf token: %v %q", c, out.CSRF)
	}
	return c, out.CSRF
}

func (e *env) issue(kind auth.Kind) string {
	e.t.Helper()
	var secret string
	err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind})
		secret = s
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return secret
}

func bearer(secret string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func session(c *http.Cookie, csrf string) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(c)
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
	}
}

func errBody(t *testing.T, r resp) domain.Error {
	t.Helper()
	var raw map[string]json.RawMessage
	r.json(&raw)
	for _, k := range []string{"code", "why", "next", "web_url"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("error body lacks %q: %s", k, r.Body)
		}
	}
	if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("error content-type = %q", ct)
	}
	var e domain.Error
	r.json(&e)
	return e
}

// ---- 路由与 Bearer ----

func TestBearerAuthenticatesEveryTokenKind(t *testing.T) {
	e := setup(t)
	for kind, level := range map[auth.Kind]string{auth.Observer: "observer", auth.Worker: "worker", auth.Delegate: "delegate"} {
		r := e.do("GET", "/v1/echo", "", bearer(e.issue(kind)))
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", kind, r.Code, r.Body)
		}
		var out map[string]any
		r.json(&out)
		if out["level"] != level || out["via"] != "cli" || out["web"] != false {
			t.Fatalf("%s: caller = %v", kind, out)
		}
	}
}

func TestMissingBadAndRevokedBearerAreUnauthenticated(t *testing.T) {
	e := setup(t)
	secret := e.issue(auth.Worker)
	var id string
	cs := func() {
		_ = e.st.View(context.Background(), func(tx *store.Tx) error {
			l, err := e.auth.List(tx)
			id = l[0].ID
			return err
		})
	}
	cs()
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error { return e.auth.Revoke(tx, id) }); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*http.Request){
		"none":    nil,
		"garbage": bearer("apa_wrk_nope"),
		"revoked": bearer(secret),
		"scheme":  func(r *http.Request) { r.Header.Set("Authorization", "Basic abc") },
	} {
		r := e.do("GET", "/v1/echo", "", mod)
		if r.Code != 401 {
			t.Fatalf("%s: status %d", name, r.Code)
		}
		if b := errBody(t, r); b.Code != domain.CodeUnauthenticated || b.Code.ExitCode() != 3 {
			t.Fatalf("%s: %+v", name, b)
		}
	}
}

func TestInvalidBearerDoesNotFallBackToSession(t *testing.T) {
	e := setup(t)
	c, csrf := e.login()
	r := e.do("GET", "/v1/echo", "", func(r *http.Request) { session(c, csrf)(r); bearer("apa_wrk_nope")(r) })
	if r.Code != 401 {
		t.Fatalf("status %d", r.Code)
	}
}

func TestPublicAndUnversionedRoutes(t *testing.T) {
	e := setup(t)
	var out map[string]bool
	r := e.do("GET", "/v1/public", "", nil)
	r.json(&out)
	if r.Code != 200 || !out["anon"] {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	// /ingest 不带版本前缀（§17.1）。
	r = e.do("POST", "/ingest/src_1", "{}", nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"src_1"`) {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	if r = e.do("POST", "/v1/ingest/src_1", "{}", nil); r.Code != 404 {
		t.Fatalf("ingest must not be versioned: %d", r.Code)
	}
}

func TestUnifiedErrorBodies(t *testing.T) {
	e := setup(t)
	tok := bearer(e.issue(auth.Observer))

	r := e.do("GET", "/v1/nope", "", tok)
	if b := errBody(t, r); r.Code != 404 || b.Code != domain.CodeNotFound {
		t.Fatalf("404: %d %+v", r.Code, b)
	}
	r = e.do("GET", "/v1/denied", "", tok)
	if b := errBody(t, r); r.Code != 403 || b.Code != domain.CodeExecutorNotAllowed || len(b.Allowed) != 1 || b.Code.ExitCode() != 21 {
		t.Fatalf("domain error passthrough: %d %+v", r.Code, b)
	}
	// 非领域错误：500，不泄露内部细节。
	r = e.do("GET", "/v1/boom", "", tok)
	b := errBody(t, r)
	if r.Code != 500 || b.Code != domain.CodeInternal || strings.Contains(r.Body.String(), "secret-db-path") {
		t.Fatalf("internal: %d %s", r.Code, r.Body)
	}
	r = e.do("GET", "/v1/panic", "", tok)
	if b := errBody(t, r); r.Code != 500 || b.Code != domain.CodeInternal || strings.Contains(r.Body.String(), "kaboom") {
		t.Fatalf("panic: %d %s", r.Code, r.Body)
	}
	// 未认证的 404 路径也要先答 401 吗？不：未登记路径不暴露存在性，直接 404。
}

func TestBodyLimitAndMalformedJSON(t *testing.T) {
	e := setup(t)
	tok := bearer(e.issue(auth.Worker))
	if r := e.do("POST", "/v1/small", `{"a":1}`, tok); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	for name, body := range map[string]string{
		"too large": `{"a":"` + strings.Repeat("x", 64) + `"}`,
		"malformed": `{"a":`,
		"trailing":  `{"a":1} {"b":2}`,
	} {
		r := e.do("POST", "/v1/small", body, tok)
		if b := errBody(t, r); r.Code != 400 || b.Code != domain.CodeInvalidRequest {
			t.Fatalf("%s: %d %+v", name, r.Code, b)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := setup(t)
	r := e.do("GET", "/v1/public", "", nil)
	if r.Header().Get("Cache-Control") != "no-store" || r.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", r.Header())
	}
}

// ---- 路由分组注册：新分组只加自己的文件 ----

func TestGroupsRegisterWithoutTouchingCoreTable(t *testing.T) {
	items := func(r *Router) {
		r.Handle(Route{Method: "GET", Path: "/items/{id}", Handler: func(w http.ResponseWriter, r *http.Request, p *Principal) error {
			return WriteJSON(w, 200, map[string]string{"id": r.PathValue("id")})
		}})
	}
	e := setup(t, items)
	r := e.do("GET", "/v1/items/item_8f2", "", bearer(e.issue(auth.Observer)))
	if r.Code != 200 || !strings.Contains(r.Body.String(), "item_8f2") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestGlobalRegistryGroupsAreMounted(t *testing.T) {
	defer func(old []namedGroup) { registry = old }(registry)
	registry = nil
	Register("zz-test", func(r *Router) {
		r.Handle(Route{Method: "GET", Path: "/zz", Auth: AuthNone, Handler: func(w http.ResponseWriter, r *http.Request, p *Principal) error {
			return WriteJSON(w, 200, map[string]string{"ok": "1"})
		}})
	})
	e := setup(t)
	if r := e.do("GET", "/v1/zz", "", nil); r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("duplicate group name must panic at init time")
			}
		}()
		Register("zz-test", func(*Router) {})
	}()
}

func TestInvalidRoutesRejectedAtStartup(t *testing.T) {
	e := setup(t)
	h := func(http.ResponseWriter, *http.Request, *Principal) error { return nil }
	for name, g := range map[string]RouteGroup{
		"duplicate": func(r *Router) {
			r.Handle(Route{Method: "GET", Path: "/dup", Handler: h})
			r.Handle(Route{Method: "GET", Path: "/dup", Handler: h})
		},
		"no handler": func(r *Router) { r.Handle(Route{Method: "GET", Path: "/x"}) },
		"bad path":   func(r *Router) { r.Handle(Route{Method: "GET", Path: "x", Handler: h}) },
		"no method":  func(r *Router) { r.Handle(Route{Path: "/x", Handler: h}) },
		"conflict with auth": func(r *Router) {
			r.Handle(Route{Method: "POST", Path: "/auth/login", Handler: h})
		},
	} {
		if _, err := New(Deps{Store: e.st, Auth: e.auth, Groups: []RouteGroup{g}}); err == nil {
			t.Fatalf("%s: want startup error", name)
		}
	}
	if _, err := New(Deps{Auth: e.auth}); err == nil {
		t.Fatal("missing store must fail")
	}
}

// ---- 登录、Cookie、CSRF ----

func TestLoginSetsSecureSessionCookieAndSessionActsAsOwner(t *testing.T) {
	e := setup(t)
	r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https") // 本机反向代理终结 TLS
	})
	if r.Code != 200 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
	c := r.cookie(SessionCookie)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Value == "" {
		t.Fatalf("cookie must be HttpOnly+Secure+SameSite=Strict: %+v", c)
	}
	if want := e.now.Add(DefaultSessionTTL); !c.Expires.Equal(want) {
		t.Fatalf("expires %v want %v", c.Expires, want)
	}
	if strings.Contains(r.Body.String(), c.Value) {
		t.Fatal("session id must not appear in the body")
	}
	// 会话只落哈希。
	_ = e.st.View(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT COUNT(*) FROM apicore_session WHERE id_hash = ?", c.Value)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var n int64
		rows.Next()
		_ = rows.Scan(&n)
		if n != 0 {
			t.Fatal("plaintext session id stored")
		}
		return nil
	})
	got := e.do("GET", "/v1/echo", "", session(c, ""))
	var out map[string]any
	got.json(&out)
	if got.Code != 200 || out["principal"] != "owner" || out["via"] != "web" || out["level"] != "web" || out["web"] != true {
		t.Fatalf("session caller: %d %v", got.Code, out)
	}
}

func TestLoginRejectsWrongPasswordWithoutLeak(t *testing.T) {
	e := setup(t)
	for name, body := range map[string]string{
		"wrong":   `{"password":"nope-nope-nope"}`,
		"empty":   `{"password":""}`,
		"missing": `{}`,
	} {
		r := e.do("POST", "/v1/auth/login", body, nil)
		if b := errBody(t, r); r.Code != 401 || b.Code != domain.CodeUnauthenticated {
			t.Fatalf("%s: %d %+v", name, r.Code, b)
		}
		if r.cookie(SessionCookie) != nil {
			t.Fatalf("%s: no cookie on failed login", name)
		}
	}
	if r := e.do("POST", "/v1/auth/login", `not json`, nil); r.Code != 400 {
		t.Fatalf("malformed: %d", r.Code)
	}
}

func TestLoginBeforeOwnerPasswordIsSetIsRejected(t *testing.T) {
	e := setup(t)
	_ = e.st.Update(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec("DELETE FROM apicore_owner"); return err })
	r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if r.Code != 401 {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestCSRFRequiredForCookieWritesOnly(t *testing.T) {
	e := setup(t)
	c, csrf := e.login()

	// 读不需要令牌。
	if r := e.do("GET", "/v1/echo", "", session(c, "")); r.Code != 200 {
		t.Fatalf("GET: %d", r.Code)
	}
	// 写：缺令牌与错令牌都拒绝，且是 403 permission_denied（退出码 4）。
	for name, tok := range map[string]string{"missing": "", "wrong": "x" + csrf} {
		r := e.do("POST", "/v1/echo", "{}", session(c, tok))
		if b := errBody(t, r); r.Code != 403 || b.Code != domain.CodePermissionDenied {
			t.Fatalf("%s: %d %+v", name, r.Code, b)
		}
	}
	if r := e.do("POST", "/v1/echo", "{}", session(c, csrf)); r.Code != 200 {
		t.Fatalf("valid csrf: %d %s", r.Code, r.Body)
	}
	// Bearer 不是环境凭据，不要求令牌。
	if r := e.do("POST", "/v1/echo", "{}", bearer(e.issue(auth.Worker))); r.Code != 200 {
		t.Fatalf("bearer POST: %d", r.Code)
	}
}

func TestCrossSiteOriginRejectedForCookieWrites(t *testing.T) {
	e := setup(t)
	c, csrf := e.login()
	for name, origin := range map[string]string{"foreign": "https://evil.example", "null": "null"} {
		r := e.do("POST", "/v1/echo", "{}", func(r *http.Request) {
			session(c, csrf)(r)
			r.Header.Set("Origin", origin)
		})
		if r.Code != 403 {
			t.Fatalf("%s: %d", name, r.Code)
		}
	}
	same := e.do("POST", "/v1/echo", "{}", func(r *http.Request) {
		session(c, csrf)(r)
		r.Header.Set("Origin", "http://"+r.Host)
	})
	if same.Code != 200 {
		t.Fatalf("same-origin: %d %s", same.Code, same.Body)
	}
	// 跨站登录（login CSRF）同样拒绝。
	r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if r.Code != 403 || r.cookie(SessionCookie) != nil {
		t.Fatalf("cross-site login: %d", r.Code)
	}
}

func TestLogoutEndsSessionServerSide(t *testing.T) {
	e := setup(t)
	c, csrf := e.login()
	// 登出本身也是写：需要 CSRF。
	if r := e.do("POST", "/v1/auth/logout", "", session(c, "")); r.Code != 403 {
		t.Fatalf("logout without csrf: %d", r.Code)
	}
	r := e.do("POST", "/v1/auth/logout", "", session(c, csrf))
	if r.Code != 200 {
		t.Fatalf("logout: %d %s", r.Code, r.Body)
	}
	if cl := r.cookie(SessionCookie); cl == nil || cl.MaxAge >= 0 || cl.Value != "" {
		t.Fatalf("cookie must be cleared: %+v", cl)
	}
	// 被偷走的旧 Cookie 此后不可用。
	if r := e.do("GET", "/v1/echo", "", session(c, "")); r.Code != 401 {
		t.Fatalf("stale session still valid: %d", r.Code)
	}
	if r := e.do("POST", "/v1/auth/logout", "", nil); r.Code != 401 {
		t.Fatalf("logout without session: %d", r.Code)
	}
}

func TestSessionExpiresAndLogoutRevokesOnlyThatSession(t *testing.T) {
	e := setup(t)
	c1, csrf1 := e.login()
	e.now = e.now.Add(time.Hour)
	c2, _ := e.login()
	e.do("POST", "/v1/auth/logout", "", session(c1, csrf1))
	if r := e.do("GET", "/v1/echo", "", session(c2, "")); r.Code != 200 {
		t.Fatalf("other session must survive: %d", r.Code)
	}
	e.now = e.now.Add(DefaultSessionTTL)
	if r := e.do("GET", "/v1/echo", "", session(c2, "")); r.Code != 401 {
		t.Fatalf("expired session accepted: %d", r.Code)
	}
}

func TestPlainHTTPFromNonLoopbackCannotLogIn(t *testing.T) {
	e := setup(t)
	body := `{"password":"` + ownerPW + `"}`
	remote := func(r *http.Request) { r.RemoteAddr = "203.0.113.9:4444" }
	r := e.do("POST", "/v1/auth/login", body, remote)
	if b := errBody(t, r); r.Code != 403 || b.Code != domain.CodePermissionDenied || r.cookie(SessionCookie) != nil {
		t.Fatalf("plain http from outside: %d %+v", r.Code, b)
	}
	// 非本机来源自称 X-Forwarded-Proto 不被信任（只有本机反向代理可信）。
	r = e.do("POST", "/v1/auth/login", body, func(r *http.Request) { remote(r); r.Header.Set("X-Forwarded-Proto", "https") })
	if r.Code != 403 {
		t.Fatalf("spoofed forwarded proto accepted: %d", r.Code)
	}
	// 直接 TLS 可以。
	r = e.do("POST", "/v1/auth/login", body, func(r *http.Request) { remote(r); r.TLS = &tlsState })
	if r.Code != 200 || r.cookie(SessionCookie) == nil || !r.cookie(SessionCookie).Secure {
		t.Fatalf("tls login: %d %s", r.Code, r.Body)
	}
}

func TestLoopbackPlainHTTPLoginCookieNotSecure(t *testing.T) {
	e := setup(t)
	r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	c := r.cookie(SessionCookie)
	if r.Code != 200 || c == nil || c.Secure || !c.HttpOnly {
		t.Fatalf("%d %+v", r.Code, c)
	}
}

// ---- 口令存储 ----

func TestOwnerPasswordIsHashedAndValidated(t *testing.T) {
	e := setup(t)
	_ = e.st.View(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT hash FROM apicore_owner")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var h []byte
		rows.Next()
		_ = rows.Scan(&h)
		if len(h) != 32 || strings.Contains(string(h), ownerPW) {
			t.Fatalf("hash column: %q", h)
		}
		return nil
	})
	err := e.st.Update(context.Background(), func(tx *store.Tx) error { return SetOwnerPassword(tx, "short") })
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
		t.Fatalf("short password: %v", err)
	}
	// 改口令后旧口令失效。
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error { return SetOwnerPassword(tx, "another long passphrase") }); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil); r.Code != 401 {
		t.Fatalf("old password still works: %d", r.Code)
	}
	if r := e.do("POST", "/v1/auth/login", `{"password":"another long passphrase"}`, nil); r.Code != 200 {
		t.Fatalf("new password: %d", r.Code)
	}
}

func TestChangingPasswordRevokesExistingSessions(t *testing.T) {
	e := setup(t)
	c, _ := e.login()
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error { return SetOwnerPassword(tx, "another long passphrase") }); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/echo", "", session(c, "")); r.Code != 401 {
		t.Fatalf("session survived password change: %d", r.Code)
	}
}

// ---- 监听：默认只在本机 ----

func TestDefaultListenIsLoopbackOnly(t *testing.T) {
	if got := listenAddr(""); got != config.Default().Listen {
		t.Fatalf("empty address must use the config default, got %q", got)
	}
	host, _, err := net.SplitHostPort(config.Default().Listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("config default listen %q is not loopback", config.Default().Listen)
	}
	l, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if !l.Addr().(*net.TCPAddr).IP.IsLoopback() {
		t.Fatalf("listener %v", l.Addr())
	}
}

func TestServeOverRealSocketAndShutdown(t *testing.T) {
	e := setup(t)
	l, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(ctx, l) }()

	url := "http://" + l.Addr().String()
	res, err := http.Get(url + "/v1/public")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
