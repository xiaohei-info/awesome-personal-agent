package core

import (
	"net/http"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 3b 要 Web 本人刚输过口令（§14.3）：登录算一次；过了窗口要再输一次，再输后放行。错口令不放行。
func TestReauthOpensPowerWindow(t *testing.T) {
	e := setup(t)
	c, csrf := e.login()
	if r := e.do("DELETE", "/v1/auth/sessions", "", session(c, csrf)); r.Code != 200 {
		t.Fatalf("right after login a 3b action should pass: %d %s", r.Code, r.Body)
	}
	c, csrf = e.login()
	e.now = e.now.Add(ReauthWindow + time.Minute)
	r := e.do("DELETE", "/v1/auth/sessions", "", session(c, csrf))
	if b := errBody(t, r); r.Code != http.StatusForbidden || b.Code != domain.CodeReauthRequired || b.Reason != domain.ReasonReauthRequired {
		t.Fatalf("outside the window: want 403 reauth_required, got %d %s", r.Code, r.Body)
	}
	if r := e.do("POST", "/v1/auth/reauth", `{"password":"wrong one entirely"}`, session(c, csrf)); r.Code != http.StatusUnauthorized {
		t.Fatalf("wrong PW must not reauth: %d %s", r.Code, r.Body)
	}
	if r := e.do("POST", "/v1/auth/reauth", `{"password":"`+ownerPW+`"}`, session(c, csrf)); r.Code != http.StatusNoContent {
		t.Fatalf("reauth: %d %s", r.Code, r.Body)
	}
	if r := e.do("DELETE", "/v1/auth/sessions", "", session(c, csrf)); r.Code != 200 {
		t.Fatalf("after reauth a 3b action should pass: %d %s", r.Code, r.Body)
	}
}
