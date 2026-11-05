package runs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

var bg = context.Background()

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	srv    *core.Server
	cookie *http.Cookie
}

// setup 装配真实账本与 Run 表、runs 路由，并以本人口令登录一个 Web 会话。
func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, proc.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := auth.New(nil)
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, auth: a, srv: srv}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	return e
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(r *http.Request) { r.AddCookie(e.cookie) }

func (e *env) token(kind auth.Kind) func(*http.Request) {
	e.t.Helper()
	scope := ""
	if kind == auth.Ext || kind == auth.Webhook {
		scope = "x"
	}
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

// enqueue 经内核入口写一条 queued 的 Run；命令规格里放一个秘密标记，用来证明响应不外泄它。
func (e *env) enqueue(r domain.Run) string {
	e.t.Helper()
	var out domain.Run
	err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		out, err = proc.Enqueue(tx, proc.Request{Run: r,
			Spec: proc.Spec{Command: []string{"/bin/true", "SECRET-ARGV"}, Stdin: json.RawMessage(`{"k":"SECRET-STDIN"}`)}}, time.Now())
		return
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out.ID
}

func (e *env) sql(q string, args ...any) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
		e.t.Fatal(err)
	}
}

func runOf(t *testing.T, rec *httptest.ResponseRecorder) domain.Run {
	t.Helper()
	var r domain.Run
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &r) != nil {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	return r
}

func listOf(t *testing.T, rec *httptest.ResponseRecorder) []domain.Run {
	t.Helper()
	var out struct{ Runs []domain.Run }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	return out.Runs
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return de.Code
}

// 超限排队的现场（ACT-OUT-01）：一个 running、两个 queued，observer 看到状态、授权依据和统计，
// 看不到命令规格；终态 Run 带退出码与结束时间。
func TestGetRunReturnsStateStatsAndBasis(t *testing.T) {
	e := setup(t)
	running := e.enqueue(domain.Run{Category: domain.RunTask, Target: "item_1", Harness: "work-host.stub", SessionID: "s1",
		Access: "worker", Basis: &domain.Basis{PolicyVersion: 3, RuleID: "rule.auto"}, ChainDepth: 2, TimeoutSec: 120})
	queued := e.enqueue(domain.Run{Category: domain.RunTask, Target: "item_2", TimeoutSec: 120})
	done := e.enqueue(domain.Run{Category: domain.RunSource, Target: "src_1", TimeoutSec: 60})
	e.sql(`UPDATE outcall_run SET state='running', started_at=? WHERE id=?`, time.Now().UnixNano(), running)
	e.sql(`UPDATE outcall_run SET state='running', started_at=? WHERE id=?`, time.Now().UnixNano(), done)
	exit := 0
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return proc.Settle(tx, done, domain.RunSucceeded, &exit, "", time.Now())
	}); err != nil {
		t.Fatal(err)
	}

	for name, mod := range map[string]func(*http.Request){"observer": e.token(auth.Observer), "owner": e.web} {
		rec := e.do("GET", "/v1/runs/"+running, "", mod)
		r := runOf(t, rec)
		if r.State != domain.RunRunning || r.Category != domain.RunTask || r.Target != "item_1" || r.StartedAt == nil ||
			r.Basis == nil || r.Basis.PolicyVersion != 3 || r.Basis.RuleID != "rule.auto" || r.ChainDepth != 2 || r.TimeoutSec != 120 {
			t.Fatalf("%s running = %s", name, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "SECRET") {
			t.Fatalf("%s response leaks spec: %s", name, rec.Body)
		}
	}
	if r := runOf(t, e.do("GET", "/v1/runs/"+queued, "", e.token(auth.Observer))); r.State != domain.RunQueued || r.StartedAt != nil {
		t.Fatalf("queued = %+v", r)
	}
	if r := runOf(t, e.do("GET", "/v1/runs/"+done, "", e.token(auth.Observer))); r.State != domain.RunSucceeded || r.ExitCode == nil || *r.ExitCode != 0 || r.FinishedAt == nil {
		t.Fatalf("done = %+v", r)
	}
}

// 列表按入队顺序返回，可按 state/category/target 筛选，limit 保留最新的若干条。
func TestListFiltersAndLimit(t *testing.T) {
	e := setup(t)
	a := e.enqueue(domain.Run{Category: domain.RunTask, Target: "item_1", TimeoutSec: 10})
	b := e.enqueue(domain.Run{Category: domain.RunTask, Target: "item_2", TimeoutSec: 10})
	c := e.enqueue(domain.Run{Category: domain.RunSource, Target: "src_1", TimeoutSec: 10})
	e.sql(`UPDATE outcall_run SET state='running' WHERE id=?`, a)
	obs := e.token(auth.Observer)

	ids := func(path string) []string {
		var out []string
		for _, r := range listOf(t, e.do("GET", path, "", obs)) {
			out = append(out, r.ID)
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	eq(ids("/v1/runs"), a, b, c)
	eq(ids("/v1/runs?state=queued"), b, c)
	eq(ids("/v1/runs?state=running"), a)
	eq(ids("/v1/runs?category=source"), c)
	eq(ids("/v1/runs?target=item_2"), b)
	eq(ids("/v1/runs?state=queued&category=task"), b)
	eq(ids("/v1/runs?limit=2"), b, c)
	if got := listOf(t, e.do("GET", "/v1/runs?state=timeout", "", obs)); got == nil || len(got) != 0 {
		t.Fatalf("empty result must be [] not null: %v", got)
	}
	if body := e.do("GET", "/v1/runs?state=timeout", "", obs).Body.String(); !strings.Contains(body, `"runs":[]`) {
		t.Fatalf("body = %s", body)
	}
}

// 错误语义：未登录 401、未知 id 404、ext/webhook 凭据 403、非法筛选与 limit 400。
func TestErrors(t *testing.T) {
	e := setup(t)
	id := e.enqueue(domain.Run{Category: domain.RunTask, Target: "item_1", TimeoutSec: 10})
	for _, path := range []string{"/v1/runs", "/v1/runs/" + id} {
		if rec := e.do("GET", path, "", nil); rec.Code != 401 || codeOf(t, rec) != domain.CodeUnauthenticated {
			t.Fatalf("anonymous %s = %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := e.do("GET", "/v1/runs/run_missing", "", e.token(auth.Observer)); rec.Code != 404 || codeOf(t, rec) != domain.CodeNotFound {
		t.Fatalf("unknown id = %d %s", rec.Code, rec.Body)
	}
	for _, k := range []auth.Kind{auth.Ext, auth.Webhook} {
		for _, path := range []string{"/v1/runs", "/v1/runs/" + id} {
			if rec := e.do("GET", path, "", e.token(k)); rec.Code != 403 || codeOf(t, rec) != domain.CodePermissionDenied {
				t.Fatalf("%s %s = %d %s", k, path, rec.Code, rec.Body)
			}
		}
	}
	for _, q := range []string{"state=bogus", "category=bogus", "limit=0", "limit=501", "limit=x"} {
		rec := e.do("GET", "/v1/runs?"+q, "", e.token(auth.Observer))
		if rec.Code != 400 || codeOf(t, rec) != domain.CodeInvalidRequest {
			t.Fatalf("%s = %d %s", q, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "bogus") { // why/next 只写机制，不回显输入
			t.Fatalf("error echoes input: %s", rec.Body)
		}
	}
}
