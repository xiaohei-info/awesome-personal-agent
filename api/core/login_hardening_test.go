package core

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func from(addr string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = addr + ":50000"; r.TLS = &tlsState } // 非本机来源须经 HTTPS 才能登录
}

func (e *env) loginFrom(addr, pw string) resp {
	return e.do("POST", "/v1/auth/login", `{"password":"`+pw+`"}`, from(addr))
}

func (e *env) events(typ string) []domain.Event {
	e.t.Helper()
	var out []domain.Event
	if err := e.st.View(context.Background(), func(tx *store.Tx) error {
		all, err := eventlog.Read(tx, 0, 10000)
		for _, en := range all {
			if typ == "" || en.Event.Type == typ {
				out = append(out, en.Event)
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) webDo(method, path, body string, c *http.Cookie, csrf string) resp {
	return e.do(method, path, body, func(r *http.Request) {
		r.AddCookie(c)
		r.Header.Set(CSRFHeader, csrf)
	})
}

func TestFiveFailuresLockThatAddressForFifteenMinutes(t *testing.T) {
	e := setup(t)
	for i := 0; i < 5; i++ {
		if r := e.loginFrom("198.51.100.1", "wrong-wrong-wrong"); r.Code != 401 {
			t.Fatalf("failure %d: %d", i+1, r.Code)
		}
	}
	r := e.loginFrom("198.51.100.1", ownerPW) // 锁定期内正确口令也不行
	if b := errBody(t, r); r.Code != 429 || b.Code != domain.CodeLoginLocked || r.Header().Get("Retry-After") == "" || r.cookie(SessionCookie) != nil {
		t.Fatalf("locked login: %d %+v retry-after=%q", r.Code, b, r.Header().Get("Retry-After"))
	}
	if r := e.loginFrom("198.51.100.2", ownerPW); r.Code != 200 { // 别的地址不受影响
		t.Fatalf("other address locked too: %d", r.Code)
	}
	if n := len(e.events(evLoginLocked)); n != 1 {
		t.Fatalf("lock events = %d, want 1 (rejected attempts during the lock add none)", n)
	}
	e.now = e.now.Add(15 * time.Minute)
	if r := e.loginFrom("198.51.100.1", ownerPW); r.Code != 200 {
		t.Fatalf("lock must lift after 15 minutes: %d", r.Code)
	}
}

func TestSuccessfulLoginResetsConsecutiveFailures(t *testing.T) {
	e := setup(t)
	for round := 0; round < 3; round++ {
		for i := 0; i < 4; i++ {
			e.loginFrom("198.51.100.1", "wrong-wrong-wrong")
		}
		if r := e.loginFrom("198.51.100.1", ownerPW); r.Code != 200 {
			t.Fatalf("round %d: failures were not consecutive, login refused: %d", round, r.Code)
		}
	}
}

func TestLockIsRuntimeStateAndClearsOnRestart(t *testing.T) {
	e := setup(t)
	for i := 0; i < 5; i++ {
		e.loginFrom("198.51.100.1", "wrong-wrong-wrong")
	}
	if r := e.loginFrom("198.51.100.1", ownerPW); r.Code != 429 {
		t.Fatalf("not locked: %d", r.Code)
	}
	restarted, err := New(Deps{Store: e.st, Auth: e.auth, Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = restarted
	if r := e.loginFrom("198.51.100.1", ownerPW); r.Code != 200 {
		t.Fatalf("lock survived a restart: %d", r.Code)
	}
}

func TestFloodAcrossAddressesDelaysEveryLoginAndAlertsOnce(t *testing.T) {
	e := setup(t)
	const delay = 80 * time.Millisecond
	e.srv.d.LoginDelay = delay
	// 21 次失败分散在 6 个地址，每个地址都不到 5 次连续失败，因此没有任何地址被锁。
	n := 0
	for _, a := range []string{"1", "2", "3", "4", "5", "6"} {
		for i := 0; i < 4 && n < 21; i++ {
			e.loginFrom("198.51.100."+a, "wrong-wrong-wrong")
			n++
		}
	}
	if got := len(e.events("health.alert")); got != 1 {
		t.Fatalf("health.alert events = %d, want 1", got)
	}
	start := time.Now()
	r := e.loginFrom("198.51.100.7", ownerPW)
	if r.Code != 200 {
		t.Fatalf("correct password must still succeed eventually: %d", r.Code)
	}
	if took := time.Since(start); took < delay {
		t.Fatalf("login answered in %v, want a delay of at least %v", took, delay)
	}
	e.loginFrom("198.51.100.8", "wrong-wrong-wrong")
	if got := len(e.events("health.alert")); got != 1 {
		t.Fatalf("alert repeated within the same episode: %d", got)
	}
	e.now = e.now.Add(15*time.Minute + time.Second) // 窗口过去，恢复即时应答
	start = time.Now()
	if r := e.loginFrom("198.51.100.9", ownerPW); r.Code != 200 || time.Since(start) >= delay {
		t.Fatalf("delay must end with the window: %d in %v", r.Code, time.Since(start))
	}
}

func TestForwardedForOnlyTrustedFromLoopbackProxy(t *testing.T) {
	e := setup(t)
	xff := func(remote, client string) func(*http.Request) {
		return func(r *http.Request) {
			r.RemoteAddr = remote + ":1"
			r.Header.Set("X-Forwarded-For", client)
			r.Header.Set("X-Forwarded-Proto", "https")
		}
	}
	bad := `{"password":"wrong-wrong-wrong"}`
	for i := 0; i < 5; i++ { // 本机代理后的两个真实客户端各自计数
		e.do("POST", "/v1/auth/login", bad, xff("127.0.0.1", "203.0.113.5"))
	}
	if r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, xff("127.0.0.1", "203.0.113.5")); r.Code != 429 {
		t.Fatalf("client behind proxy not locked: %d", r.Code)
	}
	if r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, xff("127.0.0.1", "203.0.113.6")); r.Code != 200 {
		t.Fatalf("another client behind the same proxy locked: %d", r.Code)
	}
	// 非本机来源自称的 X-Forwarded-For 无效：锁的是它真实的地址。
	for i := 0; i < 5; i++ {
		e.do("POST", "/v1/auth/login", bad, func(r *http.Request) {
			r.RemoteAddr = "203.0.113.50:1"
			r.TLS = &tlsState
			r.Header.Set("X-Forwarded-For", "198.51.100.77")
		})
	}
	if r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, func(r *http.Request) {
		r.RemoteAddr = "203.0.113.50:1"
		r.TLS = &tlsState
		r.Header.Set("X-Forwarded-For", "198.51.100.78")
	}); r.Code != 429 {
		t.Fatalf("spoofed X-Forwarded-For dodged the lock: %d", r.Code)
	}
}

func TestSessionIdleSevenDays(t *testing.T) {
	e := setup(t)
	day := 24 * time.Hour
	s1, _ := e.login()
	s2, _ := e.login()
	ok := func(c *http.Cookie) bool { return e.do("GET", "/v1/echo", "", session(c, "")).Code == 200 }
	e.now = e.now.Add(6 * day)
	if !ok(s2) {
		t.Fatal("session must be valid on day 6")
	}
	e.now = e.now.Add(day + time.Minute) // 第 7 天加 1 分钟：S1 空闲满 7 天，S2 才空闲 1 天
	if ok(s1) {
		t.Fatal("session idle for 7 days still valid")
	}
	e.now = e.now.Add(5*day - time.Minute) // 第 12 天
	if !ok(s2) {
		t.Fatal("session used within the last 7 days was dropped")
	}
}

func TestSessionAbsoluteThirtyDays(t *testing.T) {
	e := setup(t)
	day := 24 * time.Hour
	start := e.now
	s3, _ := e.login()
	ok := func() bool { return e.do("GET", "/v1/echo", "", session(s3, "")).Code == 200 }
	for i := 1; i <= 4; i++ { // 每 6 天用一次，到第 24 天
		e.now = start.Add(time.Duration(i) * 6 * day)
		if !ok() {
			t.Fatalf("dropped on day %d", i*6)
		}
	}
	e.now = start.Add(30*day - time.Minute)
	if !ok() {
		t.Fatal("dropped before 30 days")
	}
	e.now = start.Add(30*day + time.Minute)
	if ok() {
		t.Fatal("continuously used session survived the 30-day absolute limit")
	}
}

func TestSessionsAreListedAndRevokedOneByOneOrAll(t *testing.T) {
	e := setup(t)
	login := func(agent string) (*http.Cookie, string) {
		r := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, func(r *http.Request) { r.Header.Set("User-Agent", agent) })
		var out struct {
			CSRF string `json:"csrf_token"`
		}
		r.json(&out)
		return r.cookie(SessionCookie), out.CSRF
	}
	c1, t1 := login("UA-one")
	c2, t2 := login("UA-two")
	c3, _ := login("UA-three")

	r := e.webDo("GET", "/v1/auth/sessions", "", c1, t1)
	var list struct {
		Sessions []sessionInfo `json:"sessions"`
	}
	r.json(&list)
	if r.Code != 200 || len(list.Sessions) != 3 {
		t.Fatalf("list: %d %s", r.Code, r.Body)
	}
	cur := 0
	for _, s := range list.Sessions {
		if s.Addr != "127.0.0.1" || !strings.HasPrefix(s.Agent, "UA-") || s.LastUsedAt.IsZero() || s.ID == "" {
			t.Fatalf("session fields: %+v", s)
		}
		if s.Current {
			cur++
		}
	}
	if cur != 1 {
		t.Fatalf("exactly one session is current: %d", cur)
	}
	var second string
	for _, s := range list.Sessions {
		if s.Agent == "UA-two" {
			second = s.ID
		}
	}
	if r := e.webDo("DELETE", "/v1/auth/sessions/"+second, "", c1, t1); r.Code != 200 {
		t.Fatalf("revoke one: %d %s", r.Code, r.Body)
	}
	if e.do("GET", "/v1/echo", "", session(c2, t2)).Code != 401 {
		t.Fatal("revoked session still valid")
	}
	if e.do("GET", "/v1/echo", "", session(c1, "")).Code != 200 || e.do("GET", "/v1/echo", "", session(c3, "")).Code != 200 {
		t.Fatal("revoking one session must not touch the others")
	}
	if r := e.webDo("DELETE", "/v1/auth/sessions/"+second, "", c1, t1); r.Code != 404 {
		t.Fatalf("revoking an unknown session: %d", r.Code)
	}
	if r := e.webDo("DELETE", "/v1/auth/sessions", "", c1, t1); r.Code != 200 {
		t.Fatalf("revoke all: %d", r.Code)
	}
	if e.do("GET", "/v1/echo", "", session(c1, "")).Code != 401 || e.do("GET", "/v1/echo", "", session(c3, "")).Code != 401 {
		t.Fatal("revoke-all must end every session")
	}
	if n := len(e.events(evSessionRevoked)); n != 2 {
		t.Fatalf("revoke events = %d, want one per revocation", n)
	}
}

func TestSessionAndPasswordManagementIsWebOnly(t *testing.T) {
	e := setup(t)
	c, _ := e.login()
	del := e.issue(auth.Delegate)
	bearer := func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+del)
		r.Header.Set(EvidenceHeader, "按本人原话")
		r.Header.Set("X-APA-Session-Kind", "direct")
	}
	for _, rq := range [][3]string{{"GET", "/v1/auth/sessions", ""}, {"DELETE", "/v1/auth/sessions", ""},
		{"POST", "/v1/auth/password", `{"current":"` + ownerPW + `","new":"a-brand-new-passphrase"}`}} {
		if r := e.do(rq[0], rq[1], rq[2], bearer); r.Code != 403 {
			t.Fatalf("%s %s with a delegate token: %d", rq[0], rq[1], r.Code)
		}
	}
	if e.do("GET", "/v1/echo", "", session(c, "")).Code != 200 {
		t.Fatal("a refused request must not end sessions")
	}
	if r := e.loginFrom("127.0.0.1", ownerPW); r.Code != 200 {
		t.Fatal("password must be unchanged")
	}
}

func TestChangePasswordChecksCurrentAndLengthThenRevokesOtherSessions(t *testing.T) {
	e := setup(t)
	a, ta := e.login()
	b, tb := e.login()
	change := func(cur, next string) resp {
		return e.webDo("POST", "/v1/auth/password", `{"current":"`+cur+`","new":"`+next+`"}`, a, ta)
	}
	if r := change("not-the-current-one", "a-brand-new-passphrase"); r.Code != 403 {
		t.Fatalf("wrong current: %d", r.Code)
	}
	if r := change(ownerPW, "elevenchars"); r.Code != 400 {
		t.Fatalf("11-char new: %d", r.Code)
	}
	if r := e.loginFrom("127.0.0.1", ownerPW); r.Code != 200 {
		t.Fatal("refused change must leave the password as is")
	}
	if r := change(ownerPW, "a-brand-new-passphrase"); r.Code != 200 {
		t.Fatalf("change: %d %s", r.Code, r.Body)
	}
	if e.do("GET", "/v1/echo", "", session(b, tb)).Code != 401 {
		t.Fatal("other session survived the change")
	}
	if e.do("GET", "/v1/echo", "", session(a, "")).Code != 200 {
		t.Fatal("the session that changed the password must stay")
	}
	if r := e.loginFrom("127.0.0.1", ownerPW); r.Code != 401 {
		t.Fatalf("old password: %d", r.Code)
	}
	if r := e.loginFrom("127.0.0.1", "a-brand-new-passphrase"); r.Code != 200 {
		t.Fatalf("new password: %d", r.Code)
	}
	if n := len(e.events(evPasswordChanged)); n != 1 {
		t.Fatalf("change events = %d", n)
	}
}

func TestWrongCurrentPasswordCountsTowardTheLock(t *testing.T) {
	e := setup(t)
	c, tok := e.login()
	for i := 0; i < 5; i++ {
		e.webDo("POST", "/v1/auth/password", `{"current":"guess-guess-guess","new":"a-brand-new-passphrase"}`, c, tok)
	}
	r := e.webDo("POST", "/v1/auth/password", `{"current":"`+ownerPW+`","new":"a-brand-new-passphrase"}`, c, tok)
	if r.Code != 429 {
		t.Fatalf("guessing the current password through a hijacked session must be rate limited: %d", r.Code)
	}
}

func TestPasswordLengthCountsCharactersAndIsNotRetroactive(t *testing.T) {
	e := setup(t)
	set := func(pw string) error {
		return e.st.Update(context.Background(), func(tx *store.Tx) error { return SetOwnerPassword(tx, pw) })
	}
	if err := set(strings.Repeat("口", 11)); err == nil { // 33 字节但只有 11 个字符
		t.Fatal("11 characters accepted")
	}
	if err := set(strings.Repeat("口", 12)); err != nil {
		t.Fatalf("12 characters rejected: %v", err)
	}
	// 升级前设置的 8 个字符的口令：直接按 v0.1 规则落库，仍能登录。
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error { return storeOwner(tx, "short8pw", "") }); err != nil {
		t.Fatal(err)
	}
	if r := e.loginFrom("127.0.0.1", "short8pw"); r.Code != 200 {
		t.Fatalf("existing short password refused: %d", r.Code)
	}
}

func TestSessionsFromBeforeTheUpgradeAreDead(t *testing.T) {
	e := setup(t)
	// 升级迁移（ledger-v3-session-validity）把旧会话的 expires_at 置 0、last_used_at 默认 0。
	err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO apicore_session (id_hash, csrf, created_at, expires_at) VALUES (?, 'x', ?, 0)", hashID("old-cookie"), e.now.UnixNano())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/echo", "", session(&http.Cookie{Name: SessionCookie, Value: "old-cookie"}, "")); r.Code != 401 {
		t.Fatalf("pre-upgrade session accepted: %d", r.Code)
	}
}

func TestAuditEventsCoverLoginLockChangeRevokeWithoutSecrets(t *testing.T) {
	e := setup(t)
	c, tok := e.login()
	for i := 0; i < 5; i++ {
		e.loginFrom("198.51.100.1", "wrong-secret-guess")
	}
	e.webDo("POST", "/v1/auth/password", `{"current":"`+ownerPW+`","new":"a-brand-new-passphrase"}`, c, tok)
	e.webDo("DELETE", "/v1/auth/sessions", "", c, tok)
	for _, typ := range []string{evLogin, evLoginLocked, evPasswordChanged, evSessionRevoked} {
		if len(e.events(typ)) == 0 {
			t.Errorf("no %s event", typ)
		}
	}
	for _, ev := range e.events("") {
		blob := ev.Type + ev.Reason + string(ev.Payload) + ev.Object
		for _, pw := range []string{ownerPW, "wrong-secret-guess", "a-brand-new-passphrase"} {
			if strings.Contains(blob, pw) {
				t.Fatalf("event %s leaks a password: %s", ev.Type, blob)
			}
		}
	}
}

// 并发的错误口令尝试不能绕过锁：检查与计数是原子的，从同一地址只有 5 次会真正去校验口令。
func TestConcurrentWrongAttemptsCannotOutrunTheLock(t *testing.T) {
	const n = 30
	for name, attempt := range map[string]func(e *env, c *http.Cookie, tok string) resp{
		"login": func(e *env, _ *http.Cookie, _ string) resp { return e.loginFrom("198.51.100.1", "wrong-wrong-wrong") },
		"change-password": func(e *env, c *http.Cookie, tok string) resp {
			return e.webDo("POST", "/v1/auth/password", `{"current":"wrong-wrong-wrong","new":"a-brand-new-passphrase"}`, c, tok)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			c, tok := e.login()
			codes := make(chan int, n)
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				go func() { <-start; codes <- attempt(e, c, tok).Code }()
			}
			close(start)
			got := map[int]int{}
			for i := 0; i < n; i++ {
				got[<-codes]++
			}
			if got[429] != n-lockAfterFails || got[429]+got[401]+got[403] != n {
				t.Fatalf("want %d evaluated and %d locked out, got %v", lockAfterFails, n-lockAfterFails, got)
			}
		})
	}
}
