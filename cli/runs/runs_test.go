package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apiruns "github.com/xiaohei-info/awesome-personal-agent/api/runs"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/wake"
	_ "modernc.org/sqlite"
)

var bg = context.Background()

type handlerTransport struct{ h http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.RemoteAddr = "127.0.0.1:5000"
	h.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// rig 装配真实账本、Run 表与真实 /v1/runs 路由；CLI 经 http.Client 直接打进该 handler。
type rig struct {
	t    *testing.T
	st   *store.Store
	auth *auth.Auth
	srv  *core.Server
}

func newRig(t *testing.T) *rig {
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
	wk, err := wake.New(wake.Options{Delivery: delivery.New(delivery.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiruns.Routes(st, wk)}})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, st: st, auth: a, srv: srv}
}

func (r *rig) token(kind auth.Kind) string {
	var secret string
	scope := ""
	if kind == auth.Ext {
		scope = "x"
	}
	if err := r.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := r.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return secret
}

func (r *rig) enqueue(run domain.Run) string {
	r.t.Helper()
	var out domain.Run
	err := r.st.Update(bg, func(tx *store.Tx) (err error) {
		out, err = proc.Enqueue(tx, proc.Request{Run: run,
			Spec: proc.Spec{Command: []string{"/bin/true", "SECRET-ARGV"}, Stdin: json.RawMessage(`{"k":"SECRET-STDIN"}`)}}, time.Now())
		return
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return out.ID
}

func (r *rig) sql(q string, args ...any) {
	r.t.Helper()
	if err := r.st.Update(bg, func(tx *store.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
		r.t.Fatal(err)
	}
}

type result struct {
	code        int
	out, errOut string
}

func (r *rig) apa(token string, args ...string) result {
	r.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": token, "APA_HOME": r.t.TempDir()}
	env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: r.t.TempDir(), Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: handlerTransport{r.srv.Handler()}}}
	return result{client.Main(args, env), out.String(), errb.String()}
}

func listIDs(t *testing.T, res result) []string {
	t.Helper()
	var out struct{ Runs []domain.Run }
	if res.code != 0 || json.Unmarshal([]byte(res.out), &out) != nil {
		t.Fatalf("list: %+v", res)
	}
	ids := []string{}
	for _, r := range out.Runs {
		ids = append(ids, r.ID)
	}
	return ids
}

// seed：一个 running、两个 queued 的长时任务（ACT-OUT-01 的超限排队现场）和一条已成功的来源拉取。
func seed(r *rig) (running, q1, q2, src string) {
	running = r.enqueue(domain.Run{Category: domain.RunTask, Target: "item_1", Harness: "work-host.stub", TimeoutSec: 120})
	q1 = r.enqueue(domain.Run{Category: domain.RunTask, Target: "item_2", TimeoutSec: 120})
	q2 = r.enqueue(domain.Run{Category: domain.RunBackground, Runner: "main_session", Launch: "wake", Target: "item_3", TimeoutSec: 120})
	src = r.enqueue(domain.Run{Category: domain.RunSource, Target: "src_1", TimeoutSec: 60})
	r.sql(`UPDATE outcall_run SET state='running', started_at=? WHERE id=?`, time.Now().UnixNano(), running)
	r.sql(`UPDATE outcall_run SET state='running', started_at=? WHERE id=?`, time.Now().UnixNano(), src)
	zero := 0
	if err := r.st.Update(bg, func(tx *store.Tx) error {
		return proc.Settle(tx, src, domain.RunSucceeded, &zero, "", time.Now())
	}); err != nil {
		r.t.Fatal(err)
	}
	return
}

// ACT-OUT-01 / ACT-BGM-02：observer 用 `apa run list` 看到按入队顺序的全部 Run，一个 running、其余 queued，且不泄露命令规格。
func TestRunListShowsQueueOrderAndStates(t *testing.T) {
	r := newRig(t)
	running, q1, q2, src := seed(r)
	obs := r.token(auth.Observer)

	res := r.apa(obs, "run", "list")
	if got := listIDs(t, res); strings.Join(got, ",") != strings.Join([]string{running, q1, q2, src}, ",") {
		t.Fatalf("ids = %v", got)
	}
	if strings.Contains(res.out, "SECRET") || strings.Contains(strings.TrimSpace(res.out), "\n") {
		t.Fatalf("output must be compact JSON without command spec: %s", res.out)
	}
	var out struct{ Runs []domain.Run }
	_ = json.Unmarshal([]byte(res.out), &out)
	states := map[domain.RunState]int{}
	for _, run := range out.Runs {
		states[run.State]++
	}
	if states[domain.RunRunning] != 1 || states[domain.RunQueued] != 2 || states[domain.RunSucceeded] != 1 {
		t.Fatalf("states = %v", states)
	}

	pretty := r.apa(obs, "run", "list", "--pretty")
	if pretty.code != 0 || strings.Contains(pretty.out, "SECRET") {
		t.Fatalf("pretty: %+v", pretty)
	}
	for _, want := range []string{running, "running", "queued", "succeeded", "item_2", "main_session"} {
		if !strings.Contains(pretty.out, want) {
			t.Fatalf("pretty missing %q: %s", want, pretty.out)
		}
	}
	if empty := r.apa(obs, "run", "list", "--state", "timeout", "--pretty"); empty.code != 0 || !strings.Contains(empty.out, "no runs") {
		t.Fatalf("empty pretty: %+v", empty)
	}
}

// 筛选 flag 原样交给内核：state、category、target、limit。
func TestRunListFilters(t *testing.T) {
	r := newRig(t)
	running, q1, q2, src := seed(r)
	obs := r.token(auth.Observer)
	eq := func(want []string, args ...string) {
		t.Helper()
		if got := listIDs(t, r.apa(obs, append([]string{"run", "list"}, args...)...)); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%v = %v, want %v", args, got, want)
		}
	}
	eq([]string{q1, q2}, "--state", "queued")
	eq([]string{running}, "--state", "running", "--category", "task")
	eq([]string{src}, "--category", "source")
	eq([]string{q1}, "--target", "item_2")
	eq([]string{q2, src}, "--limit", "2")
}

// `apa run show <id>`：状态、授权依据、退出码、结束时间；--pretty 同样不泄露命令规格。
func TestRunShow(t *testing.T) {
	r := newRig(t)
	_, _, _, src := seed(r)
	id := r.enqueue(domain.Run{Category: domain.RunTask, Target: "item_9", Harness: "work-host.stub", SessionID: "s1", Access: "worker",
		Basis: &domain.Basis{PolicyVersion: 3, RuleID: "rule.auto"}, ChainDepth: 2, TimeoutSec: 90})
	obs := r.token(auth.Observer)

	res := r.apa(obs, "run", "show", id)
	var run domain.Run
	if res.code != 0 || json.Unmarshal([]byte(res.out), &run) != nil || run.ID != id || run.State != domain.RunQueued ||
		run.Basis == nil || run.Basis.RuleID != "rule.auto" || run.ChainDepth != 2 {
		t.Fatalf("show: %+v", res)
	}
	done := r.apa(obs, "run", "show", src, "--pretty")
	for _, want := range []string{src, "succeeded", "exit 0", "src_1"} {
		if done.code != 0 || !strings.Contains(done.out, want) {
			t.Fatalf("pretty missing %q: %+v", want, done)
		}
	}
	pr := r.apa(obs, "run", "show", id, "--pretty")
	for _, want := range []string{"policy v3", "rule.auto", "depth: 2", "work-host.stub"} {
		if !strings.Contains(pr.out, want) {
			t.Fatalf("pretty missing %q: %s", want, pr.out)
		}
	}
	if strings.Contains(res.out+pr.out+done.out, "SECRET") {
		t.Fatal("command spec leaked")
	}
}

// 失败语义：错误体与退出码原样透出；本地参数错误不发请求；ext 凭据不能读账本。
func TestRunErrors(t *testing.T) {
	r := newRig(t)
	seed(r)
	obs := r.token(auth.Observer)
	for _, tc := range []struct {
		name string
		tok  string
		args []string
		code int
		in   string
	}{
		{"missing run", obs, []string{"run", "show", "run_missing"}, domain.CodeNotFound.ExitCode(), "not_found"},
		{"bad state", obs, []string{"run", "list", "--state", "bogus"}, domain.CodeInvalidRequest.ExitCode(), "invalid_request"},
		{"no subcommand", obs, []string{"run"}, domain.CodeInvalidRequest.ExitCode(), "invalid_request"},
		{"unknown subcommand", obs, []string{"run", "kill"}, domain.CodeInvalidRequest.ExitCode(), "invalid_request"},
		{"show without id", obs, []string{"run", "show"}, domain.CodeInvalidRequest.ExitCode(), "invalid_request"},
		{"list stray arg", obs, []string{"run", "list", "extra"}, domain.CodeInvalidRequest.ExitCode(), "invalid_request"},
		{"no credential", "", []string{"run", "list"}, domain.CodeUnauthenticated.ExitCode(), "unauthenticated"},
	} {
		res := r.apa(tc.tok, tc.args...)
		if res.code == 0 || res.code != tc.code || res.out != "" || !strings.Contains(res.errOut, tc.in) {
			t.Fatalf("%s: %+v (want exit %d containing %q)", tc.name, res, tc.code, tc.in)
		}
	}
	if res := r.apa(r.token(auth.Ext), "run", "list"); res.code == 0 {
		t.Fatalf("ext credential must not read runs: %+v", res)
	}
}
