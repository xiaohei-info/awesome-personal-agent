//go:build apa_testclock

package core

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
)

// 写偏移后，未注入 Now 的 Server 取到的 Now 前进：会话立即过期，登录发出的 Cookie 到期时间取自测试时钟。
func TestTestClockOffsetExpiresWebSessionImmediately(t *testing.T) {
	home := t.TempDir()
	clock.SetHome(home)
	e := setup(t)
	srv, err := New(Deps{Store: e.st, Auth: auth.New(nil), Groups: []RouteGroup{func(r *Router) {
		r.Handle(Route{Method: "GET", Path: "/echo", Handler: func(w http.ResponseWriter, r *http.Request, p *Principal) error {
			return WriteJSON(w, 200, map[string]bool{"web": p.Web})
		}})
	}}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	c, _ := e.login()
	if r := e.do("GET", "/v1/echo", "", session(c, "")); r.Code != 200 {
		t.Fatalf("fresh session rejected: %d", r.Code)
	}
	if err := os.WriteFile(filepath.Join(home, clock.OffsetFile), []byte(DefaultSessionTTL.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/v1/echo", "", session(c, "")); r.Code != 401 {
		t.Fatalf("session must expire right after the offset write: %d", r.Code)
	}
}
