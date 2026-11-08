package projects_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apiprojects "github.com/xiaohei-info/awesome-personal-agent/api/projects"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

// kernel 是真实内核（真实存储、命令管线、授权与路由）经 httptest 暴露；CLI 经真实 HTTP 调它。
type kernel struct {
	t   *testing.T
	st  *store.Store
	au  *auth.Auth
	url string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, kp.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, nil)
	if err := kp.Register(x); err != nil {
		t.Fatal(err)
	}
	au := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{apiprojects.Routes(st, x)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &kernel{t, st, au, ts.URL}
}

func (k *kernel) token(kind auth.Kind) string {
	k.t.Helper()
	var secret string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		_, s, err := k.au.Issue(tx, auth.Spec{Kind: kind})
		secret = s
		return err
	}); err != nil {
		k.t.Fatal(err)
	}
	return secret
}

type result struct {
	code        int
	out, errOut string
}

func (k *kernel) apa(token string, args ...string) result {
	k.t.Helper()
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": token, "APA_SESSION": "s1", "APA_HOME": k.t.TempDir()}
	var out, errb bytes.Buffer
	env := client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: k.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient}
	code := client.Main(append([]string{"--evidence", "owner: go ahead"}, args...), env)
	return result{code, out.String(), errb.String()}
}

func (r result) project(t *testing.T) domain.Project {
	t.Helper()
	var p domain.Project
	if err := json.Unmarshal([]byte(r.out), &p); err != nil || p.ID == "" {
		t.Fatalf("not a project (exit %d): out=%q err=%q", r.code, r.out, r.errOut)
	}
	return p
}

func (k *kernel) mustCreate(token string, args ...string) domain.Project {
	k.t.Helper()
	r := k.apa(token, append([]string{"project", "create"}, args...)...)
	if r.code != 0 {
		k.t.Fatalf("create: %+v", r)
	}
	return r.project(k.t)
}

func (k *kernel) rejections(object string) (n int) {
	k.t.Helper()
	if err := k.st.View(context.Background(), func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, en := range es {
			if en.Event.Type == domain.EvCommandRejected && en.Event.Object == object {
				n++
			}
		}
		return err
	}); err != nil {
		k.t.Fatal(err)
	}
	return
}

func TestCreateUpdateListShow(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	p := k.mustCreate(dlg, "--name", "Home", "--intro", "chores", "--tag", "a", "--tag", "b", "--capture")
	if p.Name != "Home" || p.Intro != "chores" || len(p.Tags) != 2 || !p.Capture || p.Status != domain.ProjectActive {
		t.Fatalf("created: %+v", p)
	}
	u := k.apa(dlg, "project", "update", p.ID, "--status", "paused", "--capture=false")
	if u.code != 0 {
		t.Fatalf("update: %+v", u)
	}
	if got := u.project(t); got.Status != domain.ProjectPaused || got.Capture || got.Name != "Home" {
		t.Fatalf("update must be partial: %+v", got)
	}
	// observer 可读。
	s := k.apa(obs, "project", "show", p.ID)
	if s.code != 0 || s.project(t).Status != domain.ProjectPaused {
		t.Fatalf("show: %+v", s)
	}
	l := k.apa(obs, "project", "list")
	var lr struct{ Projects []domain.Project }
	if l.code != 0 || json.Unmarshal([]byte(l.out), &lr) != nil || len(lr.Projects) != 1 {
		t.Fatalf("list: %+v", l)
	}
	if r := k.apa(obs, "project", "show", "nope"); r.code != 5 {
		t.Fatalf("missing project must exit 5: %+v", r)
	}
	// observer 不能写。
	if r := k.apa(obs, "project", "create", "--name", "X"); r.code != 4 {
		t.Fatalf("observer create must exit 4: %+v", r)
	}
}

// ACT-AUTH-07：本机检出 worker 可改；远程地址只有 delegate 可改，worker 得 4。
func TestReposAuthorizationByCredentialLevel(t *testing.T) {
	k := newKernel(t)
	dlg, wrk, obs := k.token(auth.Delegate), k.token(auth.Worker), k.token(auth.Observer)
	p := k.mustCreate(dlg, "--name", "Code")
	const remote = "https://github.com/o/r.git"

	for _, tk := range []string{wrk, obs} {
		if r := k.apa(tk, "project", "repos", "add", p.ID, "--remote", remote); r.code != 4 {
			t.Fatalf("non-delegate add remote must exit 4: %+v", r)
		}
	}
	if r := k.apa(dlg, "project", "repos", "add", p.ID, "--remote", remote); r.code != 0 {
		t.Fatalf("delegate add remote: %+v", r)
	}
	// worker 写本机检出。
	r := k.apa(wrk, "project", "repos", "add", p.ID, "--remote", remote, "--host", "box1", "--path", "/src/r")
	if r.code != 0 {
		t.Fatalf("worker set checkout: %+v", r)
	}
	got := r.project(t)
	if len(got.Repos) != 1 || len(got.Repos[0].Checkouts) != 1 || got.Repos[0].Checkouts[0] != (domain.Checkout{Host: "box1", Path: "/src/r"}) {
		t.Fatalf("checkout not recorded: %+v", got.Repos)
	}
	if r := k.apa(obs, "project", "repos", "add", p.ID, "--remote", remote, "--host", "box1", "--path", "/x"); r.code != 4 {
		t.Fatalf("observer checkout must exit 4: %+v", r)
	}
	// worker 删不了远程地址，但能删自己的检出。
	if r := k.apa(wrk, "project", "repos", "remove", p.ID, "--remote", remote); r.code != 4 {
		t.Fatalf("worker remove remote must exit 4: %+v", r)
	}
	if r := k.apa(wrk, "project", "repos", "remove", p.ID, "--remote", remote, "--host", "box1", "--path", "/src/r"); r.code != 0 || len(r.project(t).Repos[0].Checkouts) != 0 {
		t.Fatalf("worker remove checkout: %+v", r)
	}
	if r := k.apa(dlg, "project", "repos", "remove", p.ID, "--remote", remote); r.code != 0 || len(r.project(t).Repos) != 0 {
		t.Fatalf("delegate remove remote: %+v", r)
	}
}

// ACT-ALW-08：CLI 的 update 与 HTTP PATCH 带 allowed_executors 都返回 20，名单不变且记拒绝事件。
func TestAllowedExecutorsRejectedOnBothPaths(t *testing.T) {
	k := newKernel(t)
	dlg := k.token(auth.Delegate)
	p := k.mustCreate(dlg, "--name", "P")

	r := k.apa(dlg, "project", "update", p.ID, "--allowed-executors", "ex1,ex2")
	if r.code != 20 || !strings.Contains(r.errOut, `"web_only"`) {
		t.Fatalf("CLI path must exit 20 web_only: %+v", r)
	}
	if k.rejections(p.ID) != 1 {
		t.Fatal("CLI rejection must leave a rejection event")
	}

	resp := rawPatch(t, k.url+"/v1/projects/"+p.ID, dlg, `{"allowed_executors":["ex1"]}`)
	if resp != 403 {
		t.Fatalf("PATCH must be 403 (exit 20 class), got %d", resp)
	}
	if k.rejections(p.ID) != 2 {
		t.Fatal("PATCH rejection must leave a rejection event")
	}

	got := k.apa(dlg, "project", "show", p.ID).project(t)
	if got.AllowedExecutors != nil {
		t.Fatalf("allowed_executors changed: %v", got.AllowedExecutors)
	}
	// create 带该字段同样不是成功路径。
	if c := k.apa(dlg, "project", "create", "--name", "Q", "--allowed-executors", "x"); c.code != 20 {
		t.Fatalf("create with allowed_executors must exit 20: %+v", c)
	}
	// 非 delegate 得 4。
	if w := k.apa(k.token(auth.Worker), "project", "update", p.ID, "--allowed-executors", "x"); w.code != 4 {
		t.Fatalf("worker must exit 4: %+v", w)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	k := newKernel(t)
	dlg := k.token(auth.Delegate)
	for _, args := range [][]string{
		{"project"}, {"project", "bogus"}, {"project", "list", "x"}, {"project", "show"},
		{"project", "create"}, {"project", "update"}, {"project", "update", "id"},
		{"project", "update", "id", "--bogus", "1"}, {"project", "create", "--name", "a", "--capture=maybe"},
		{"project", "repos"}, {"project", "repos", "add"}, {"project", "repos", "add", "id"},
		{"project", "repos", "add", "id", "--bogus"},
	} {
		if r := k.apa(dlg, args...); r.code != 2 {
			t.Errorf("%v: want exit 2, got %+v", args, r)
		}
	}
}

func TestCheckoutDefaultsHostToThisMachine(t *testing.T) {
	k := newKernel(t)
	dlg := k.token(auth.Delegate)
	p := k.mustCreate(dlg, "--name", "C")
	k.apa(dlg, "project", "repos", "add", p.ID, "--remote", "r1")
	r := k.apa(dlg, "project", "repos", "add", p.ID, "--remote", "r1", "--path", "/p")
	if r.code != 0 || r.project(t).Repos[0].Checkouts[0].Host == "" {
		t.Fatalf("host must default to the machine host name: %+v", r)
	}
}

func rawPatch(t *testing.T, url, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest("PATCH", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
