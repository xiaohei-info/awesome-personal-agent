package usage

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	kusage "github.com/xiaohei-info/awesome-personal-agent/kernel/usage"
	_ "modernc.org/sqlite"
)

// 读取口径：observer 可读，时间段只认 1、7、30，ext 密钥与未认证读不到。
func TestUsageRoute(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, core.Module, kusage.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	issue := func(spec auth.Spec) (secret string) {
		if err := st.Update(t.Context(), func(tx *store.Tx) (err error) { _, secret, err = a.Issue(tx, spec); return }); err != nil {
			t.Fatal(err)
		}
		return
	}
	observer, ext := issue(auth.Spec{Kind: auth.Observer}), issue(auth.Spec{Kind: auth.Ext, Scope: "x"})
	for _, c := range []struct {
		query, token string
		code         int
	}{{"", observer, 200}, {"?days=30", observer, 200}, {"?days=2", observer, 400}, {"", ext, 403}, {"", "", 401}} {
		req := httptest.NewRequest("GET", "/v1/usage"+c.query, nil)
		req.RemoteAddr = "127.0.0.1:5000"
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != c.code || (c.code == 200 && !strings.Contains(rec.Body.String(), `"rows":[]`)) {
			t.Errorf("%q: %d %s", c.query, rec.Code, rec.Body)
		}
	}
}
