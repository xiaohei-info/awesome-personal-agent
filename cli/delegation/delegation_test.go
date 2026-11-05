package delegation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apidel "github.com/xiaohei-info/awesome-personal-agent/api/delegation"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kdel "github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const (
	ownerPW   = "correct horse battery"
	workHost  = "work-host.cli"
	otherHost = "other-host.cli"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

// world 是真实内核（账本、命令管线、委派/执行者/事项路由）经 HTTP 对外，CLI 以各级凭据调用它。
type world struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	x      *command.Executor
	ts     *httptest.Server
	cookie *http.Cookie
	csrf   string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		kitems.Module, executors.Module, kdel.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	for _, err := range []error{
		projects.Register(x),
		kitems.Register(x, kitems.NewKinds()),
		executors.Register(x, executors.Options{Owner: "owner", Project: projects.Get}),
		kdel.Register(x, kdel.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Error(err) }}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	a := auth.New(nil)
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), apidel.Routes(x)}})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, st: st, auth: a, x: x, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(w.ts.Close)
	for _, id := range []string{workHost, otherHost} {
		w.cmd("executor.put", map[string]any{"id": id, "kind": "external-cli"})
	}
	resp := w.raw("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(resp.body, &out)
	for _, c := range resp.cookies {
		if c.Name == core.SessionCookie {
			w.cookie = c
		}
	}
	w.csrf = out.CSRF
	return w
}

type rawResp struct {
	code    int
	body    []byte
	cookies []*http.Cookie
}

func (w *world) raw(method, path, body string, mod func(*http.Request)) rawResp {
	w.t.Helper()
	req, _ := http.NewRequest(method, w.ts.URL+path, strings.NewReader(body))
	if mod != nil {
		mod(req)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer r.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(r.Body)
	return rawResp{r.StatusCode, b.Bytes(), r.Cookies()}
}

func (w *world) cmd(name string, in any) json.RawMessage {
	w.t.Helper()
	b, _ := json.Marshal(in)
	res, err := w.x.Do(bg, command.Request{Identity: owner, Command: name, Input: b})
	if err != nil {
		w.t.Fatal(name, err)
	}
	return res.Value
}

func (w *world) project(allowed []string) string {
	w.t.Helper()
	var o struct{ ID string }
	_ = json.Unmarshal(w.cmd("project.create", map[string]any{"name": "acc-work"}), &o)
	if allowed != nil {
		w.cmd("project.allowed_executors.set", map[string]any{"id": o.ID, "executors": allowed})
	}
	return o.ID
}

func (w *world) parent(project string) string {
	w.t.Helper()
	r := w.raw("POST", "/v1/items", `{"kind":"task","title":"fix bug","phase":"open","project":"`+project+`"}`, func(r *http.Request) {
		r.AddCookie(w.cookie)
		r.Header.Set(core.CSRFHeader, w.csrf)
	})
	var o struct{ ID string }
	if r.code != 201 || json.Unmarshal(r.body, &o) != nil {
		w.t.Fatalf("create item: %d %s", r.code, r.body)
	}
	return o.ID
}

func (w *world) get(id string) domain.Item {
	w.t.Helper()
	r := w.raw("GET", "/v1/items/"+id, "", func(r *http.Request) { r.AddCookie(w.cookie) })
	var g struct{ Item domain.Item }
	_ = json.Unmarshal(r.body, &g)
	return g.Item
}

func (w *world) token(kind auth.Kind, executor string) string {
	w.t.Helper()
	var secret string
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := w.auth.Issue(tx, auth.Spec{Kind: kind, Executor: executor})
		secret = s
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return secret
}

type result struct {
	code        int
	out, errOut string
}

func (w *world) apa(token, stdin string, args ...string) result {
	w.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": w.ts.URL, "APA_TOKEN": token, "APA_HOME": w.t.TempDir()}
	code := client.Main(args, client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: w.t.TempDir(),
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return result{code, out.String(), errb.String()}
}

func errOf(t *testing.T, r result) domain.Error {
	t.Helper()
	var e domain.Error
	if err := json.Unmarshal([]byte(r.errOut), &e); err != nil {
		t.Fatalf("stderr is not an error body: %q", r.errOut)
	}
	return e
}

func idOf(t *testing.T, r result) string {
	t.Helper()
	var o struct{ ID string }
	if r.code != 0 || json.Unmarshal([]byte(r.out), &o) != nil || o.ID == "" {
		t.Fatalf("no id: %+v", r)
	}
	return o.ID
}

// flow-delegate-recover / ACT-DLG-03、04、07、08、09：delegate 建子事项使父事项 waiting；
// 绑定的 worker 回执 progress 再 done，父事项回 open；非归属 worker 退出码 4，终态再回执退出码 6，不存在退出码 5。
func TestDelegateReportRecoverParent(t *testing.T) {
	w := newWorld(t)
	proj := w.project([]string{workHost})
	parent := w.parent(proj)
	dlg, wrk, foreign := w.token(auth.Delegate, ""), w.token(auth.Worker, workHost), w.token(auth.Worker, otherHost)

	r := w.apa(dlg, "", "delegate", "--parent", parent, "--assignee", workHost, "--title", "do it")
	child := idOf(t, r)
	if c := w.get(child); c.Assignee != workHost || c.Project != proj || c.Kind != domain.KindDelegation || c.Title != "do it" {
		t.Fatalf("child = %+v", c)
	}
	if p := w.get(parent); p.Phase != domain.PhaseWaiting || p.WaitingOn == nil || p.WaitingOn.Ref != workHost {
		t.Fatalf("parent after delegate = %+v", p)
	}

	if r := w.apa(foreign, "", "report", child, "--outcome", "done", "--summary", "x", "--pr", "https://x/pr/1"); r.code != 4 || errOf(t, r).Code != domain.CodePermissionDenied {
		t.Fatalf("foreign worker: %+v", r)
	}
	if w.get(child).Phase == domain.PhaseDone {
		t.Fatal("foreign receipt changed the child")
	}
	if r := w.apa(wrk, "", "report", child, "--outcome", "progress", "--summary", "half way"); r.code != 0 {
		t.Fatalf("progress: %+v", r)
	}
	if c := w.get(child); c.StatusLabel != "half way" || c.Phase == domain.PhaseDone {
		t.Fatalf("after progress = %+v", c)
	}
	if r := w.apa(wrk, "", "report", child, "--outcome", "done", "--summary", "shipped", "--pr", "https://x/pr/88", "--commit", "abc"); r.code != 0 {
		t.Fatalf("done: %+v", r)
	}
	if c := w.get(child); c.Phase != domain.PhaseDone {
		t.Fatalf("child = %+v", c)
	}
	if p := w.get(parent); p.Phase != domain.PhaseOpen || p.WaitingOn != nil {
		t.Fatalf("parent after report = %+v", p)
	}
	_ = w.st.View(bg, func(tx *store.Tx) error {
		if it, ok, err := kdel.Lookup(tx, "https://x/pr/88"); err != nil || !ok || it != child {
			t.Fatalf("PR evidence link = %q %v %v", it, ok, err)
		}
		return nil
	})
	if r := w.apa(wrk, "", "report", child, "--outcome", "done", "--summary", "again", "--pr", "https://x/pr/88"); r.code != 6 {
		t.Fatalf("report on a settled child: %+v", r)
	}
	if r := w.apa(dlg, "", "report", "nope", "--outcome", "done", "--summary", "s", "--pr", "https://x/pr/9"); r.code != 5 {
		t.Fatalf("unknown item: %+v", r)
	}
}

// 阻塞回执：blocked 把子事项推向等待，且需要说明；缺 --outcome 是用法错误（2）。
func TestReportBlockedAndUsage(t *testing.T) {
	w := newWorld(t)
	parent := w.parent("")
	child := idOf(t, w.apa(w.token(auth.Delegate, ""), "", "delegate", "--parent", parent, "--assignee", workHost))
	wrk := w.token(auth.Worker, workHost)

	if r := w.apa(wrk, "", "report", child, "--summary", "s"); r.code != 2 {
		t.Fatalf("missing outcome: %+v", r)
	}
	if r := w.apa(wrk, "", "report", "--outcome", "done"); r.code != 2 {
		t.Fatalf("missing id: %+v", r)
	}
	if r := w.apa(wrk, "", "report", child, "--outcome", "blocked", "--summary", "need creds"); r.code != 0 {
		t.Fatalf("blocked: %+v", r)
	}
	if c := w.get(child); !strings.Contains(c.StatusLabel, "need creds") || c.Phase == domain.PhaseDone {
		t.Fatalf("after blocked = %+v", c)
	}
	// 非法 outcome 由内核拒绝（2）。
	if r := w.apa(wrk, "", "report", child, "--outcome", "weird", "--summary", "s"); r.code != 2 {
		t.Fatalf("bad outcome: %+v", r)
	}
}

// flow-allowed-executors / ACT-ALW-01..04：名单外委派退出码 21、带允许的 id 且不建子事项；
// 不在清单 5；executors --project 标明是否允许；observer 可读清单；未带凭据 3。
func TestAllowedExecutors(t *testing.T) {
	w := newWorld(t)
	proj := w.project([]string{workHost})
	parent := w.parent(proj)
	dlg, obs := w.token(auth.Delegate, ""), w.token(auth.Observer, "")

	r := w.apa(dlg, "", "delegate", "--parent", parent, "--assignee", otherHost)
	if e := errOf(t, r); r.code != 21 || e.Code != domain.CodeExecutorNotAllowed || !slices.Equal(e.Allowed, []string{workHost}) {
		t.Fatalf("rejection = %d %+v", r.code, e)
	}
	if p := w.get(parent); p.Phase != domain.PhaseOpen {
		t.Fatalf("parent changed: %+v", p)
	}
	if r := w.apa(dlg, "", "delegate", "--parent", parent, "--assignee", "ghost.cli"); r.code != 5 {
		t.Fatalf("unknown executor: %+v", r)
	}
	if r := w.apa(dlg, "", "delegate", "--parent", parent); r.code != 2 {
		t.Fatalf("missing assignee: %+v", r)
	}
	if r := w.apa(obs, "", "delegate", "--parent", parent, "--assignee", workHost); r.code != 4 {
		t.Fatalf("observer delegate: %+v", r)
	}

	r = w.apa(obs, "", "executors", "--project", proj)
	var list struct {
		Executors []struct {
			ID      string
			Allowed *bool
		}
	}
	if r.code != 0 || json.Unmarshal([]byte(r.out), &list) != nil {
		t.Fatalf("executors: %+v", r)
	}
	got := map[string]bool{}
	for _, x := range list.Executors {
		if x.Allowed == nil {
			t.Fatalf("allowed missing for %s", x.ID)
		}
		got[x.ID] = *x.Allowed
	}
	if len(got) != 2 || !got[workHost] || got[otherHost] {
		t.Fatalf("allowed = %v", got)
	}
	if r := w.apa(obs, "", "executors"); r.code != 0 || strings.Contains(r.out, "allowed") || !strings.Contains(r.out, workHost) {
		t.Fatalf("unscoped list: %+v", r)
	}
	if r := w.apa("", "", "executors"); r.code != 3 {
		t.Fatalf("anonymous: %+v", r)
	}
	if r := w.apa(obs, "", "executors", "extra"); r.code != 2 {
		t.Fatalf("stray arg: %+v", r)
	}
}

// ACT-ALW-07、10 / ACT-ITEM-06：apa declare 从文件或标准输入提交声明，只进待确认，本人确认后才进清单；
// observer 不能提交（4）；坏 JSON、缺文件为用法错误（2）；内核拒绝的声明原样透出。
func TestDeclare(t *testing.T) {
	w := newWorld(t)
	wrk, obs := w.token(auth.Worker, workHost), w.token(auth.Observer, "")
	decl := `{"harness":"newhost","executors":[{"id":"newhost.cli","kind":"external-cli"}]}`
	file := filepath.Join(t.TempDir(), "decl.json")
	if err := os.WriteFile(file, []byte(decl), 0o600); err != nil {
		t.Fatal(err)
	}

	if r := w.apa(obs, "", "declare", file); r.code != 4 {
		t.Fatalf("observer declare: %+v", r)
	}
	if r := w.apa(wrk, "", "declare", file); r.code != 0 || !strings.Contains(r.out, "version") {
		t.Fatalf("declare file: %+v", r)
	}
	if strings.Contains(w.apa(wrk, "", "executors").out, "newhost.cli") {
		t.Fatal("an unconfirmed declaration is live")
	}
	w.cmd("executor.confirm", map[string]any{"harness": "newhost", "rev": 1})
	if !strings.Contains(w.apa(wrk, "", "executors").out, "newhost.cli") {
		t.Fatal("confirmed declaration missing from the list")
	}
	if r := w.apa(wrk, strings.ReplaceAll(decl, "newhost", "viastdin"), "declare", "-"); r.code != 0 {
		t.Fatalf("declare stdin: %+v", r)
	}
	if r := w.apa(wrk, `{"harness":"x"`, "declare", "-"); r.code != 2 {
		t.Fatalf("bad json: %+v", r)
	}
	if r := w.apa(wrk, "", "declare", filepath.Join(t.TempDir(), "missing.json")); r.code != 2 {
		t.Fatalf("missing file: %+v", r)
	}
	if r := w.apa(wrk, "", "declare"); r.code != 2 {
		t.Fatalf("no file: %+v", r)
	}
	if r := w.apa(wrk, `{"harness":"newhost"}`, "declare", "-"); r.code != 2 {
		t.Fatalf("kernel-rejected declaration: %+v", r)
	}
}

// 幂等与乐观版本：同键重放首次结果不重复建子事项；--version 不符退出码 6。
func TestIdempotencyAndVersion(t *testing.T) {
	w := newWorld(t)
	parent := w.parent("")
	dlg, wrk := w.token(auth.Delegate, ""), w.token(auth.Worker, workHost)
	args := []string{"delegate", "--parent", parent, "--assignee", workHost, "--idempotency-key", "k1"}
	first, again := w.apa(dlg, "", args...), w.apa(dlg, "", args...)
	if idOf(t, again) != idOf(t, first) || !strings.Contains(again.out, `"replayed":true`) {
		t.Fatalf("replay: %+v / %+v", first, again)
	}
	child := idOf(t, first)
	if r := w.apa(wrk, "", "report", child, "--outcome", "progress", "--summary", "s", "--version", "999"); r.code != 6 {
		t.Fatalf("stale version: %+v", r)
	}
	if r := w.apa(wrk, "", "report", child, "--outcome", "progress", "--summary", "s", "--version", "abc"); r.code != 2 {
		t.Fatalf("malformed version: %+v", r)
	}
}
