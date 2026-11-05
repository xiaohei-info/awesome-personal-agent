package agents_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiagentsmd "github.com/xiaohei-info/awesome-personal-agent/api/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/agents"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	_ "modernc.org/sqlite"
)

const repo = "https://example.com/acme/app.git"

var bg = context.Background()

// kernel 是真实内核（账本、命令管线、AGENTS.md 渲染与三态判定、路由）经 httptest 暴露；CLI 经真实 HTTP 调它。
type kernel struct {
	t     *testing.T
	st    *store.Store
	au    *auth.Auth
	x     *command.Executor
	svc   *agentsmd.Service
	url   string
	token string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, materials.Module, hosted.Module, agentsmd.Module, core.Module} {
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
	for _, f := range []func() error{
		func() error { return projects.Register(x) },
		func() error { return items.Register(x, items.NewKinds()) },
		func() error { return materials.Register(x) },
		func() error { return hosted.Register(x) },
		func() error { return decisions.Register(x) },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	ld := kp.New(kp.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	if _, err := ld.Load(); err != nil {
		t.Fatal(err)
	}
	pack, ok := ld.Get("agents-md")
	if !ok {
		t.Fatal("agents-md pack missing")
	}
	svc, err := agentsmd.New(x, agentsmd.Options{Template: pack.Templates["agents"], OnError: func(err error) { t.Errorf("auto render: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiagentsmd.Routes(x, svc)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	k := &kernel{t: t, st: st, au: a, x: x, svc: svc, url: ts.URL}
	if err := st.Update(bg, func(tx *store.Tx) error {
		_, s, err := a.Issue(tx, auth.Spec{Kind: auth.Observer})
		k.token = s
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	var prj struct{ ID string }
	k.do(owner, "project.create", map[string]any{"name": "acc-work"}, &prj)
	k.do(owner, "project.repo.add", map[string]any{"id": prj.ID, "remote": repo}, nil)
	return k
}

func (k *kernel) do(id command.Identity, cmd string, in, out any) {
	k.t.Helper()
	b, _ := json.Marshal(in)
	r, err := k.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", WebURL: "http://web/x"})
	if err != nil {
		k.t.Fatalf("%s: %v", cmd, err)
	}
	if out != nil {
		_ = json.Unmarshal(r.Value, out)
	}
}

type result struct {
	code        int
	out, errOut string
}

func (k *kernel) apa(args ...string) result {
	k.t.Helper()
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": k.token, "APA_HOME": k.t.TempDir()}
	var out, errb bytes.Buffer
	env := client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: k.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient}
	return result{client.Main(args, env), out.String(), errb.String()}
}

// rendered 取内核当前渲染（只用于造文件与断言，不是被测路径）。
func (k *kernel) rendered() string {
	k.t.Helper()
	v, err := k.svc.Get(bg, repo)
	if err != nil {
		// 首次渲染由 check 触发；先用缺失文件触发一次。
		k.apa("agents", "check", "--repo", repo, "--file", filepath.Join(k.t.TempDir(), "AGENTS.md"))
		if v, err = k.svc.Get(bg, repo); err != nil {
			k.t.Fatal(err)
		}
	}
	return v.Content
}

func write(t *testing.T, content string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "AGENTS.md")
	if content != "" {
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func verdict(t *testing.T, r result) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.out), &m); err != nil {
		t.Fatalf("stdout is not JSON: %q (stderr %q)", r.out, r.errOut)
	}
	return m
}

func read(t *testing.T, f string) string {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// flow-agents-md / ACT-AGM-03：文件等于最新渲染 → 退出码 0；文件缺失 → 过期，退出码 10；二者不改动工作区。
func TestCheckCurrentAndStale(t *testing.T) {
	k := newKernel(t)
	f := write(t, k.rendered())
	r := k.apa("agents", "check", "--repo", repo, "--file", f)
	if r.code != 0 || verdict(t, r)["state"] != "current" {
		t.Fatalf("current: %+v", r)
	}
	missing := write(t, "")
	r = k.apa("agents", "check", "--repo", repo, "--file", missing)
	if r.code != domain.ExitAgentsStale || verdict(t, r)["state"] != "stale" || r.errOut != "" {
		t.Fatalf("missing file must be stale/10 with the verdict on stdout only: %+v", r)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("check must not create the file")
	}
}

// ACT-AGM-08：已有手写文件首次检查落入漂移，退出码 11，生成 needs_input 事项；文件原样保留。
func TestCheckHandWrittenFileIsDrift(t *testing.T) {
	k := newKernel(t)
	f := write(t, "# my own notes\n")
	r := k.apa("agents", "check", "--repo", repo, "--file", f)
	vd := verdict(t, r)
	if r.code != domain.ExitAgentsDrift || vd["state"] != "drift" || vd["item"] == "" {
		t.Fatalf("drift: %+v", r)
	}
	if read(t, f) != "# my own notes\n" {
		t.Fatal("check must not touch the workspace file")
	}
}

// ACT-AGM-04：过期文件由 sync 直接覆盖为最新渲染，之后 check 为一致。
func TestSyncWritesStaleAndMissing(t *testing.T) {
	k := newKernel(t)
	want := k.rendered()
	missing := write(t, "")
	r := k.apa("agents", "sync", "--repo", repo, "--file", missing)
	if r.code != 0 || verdict(t, r)["written"] != true || read(t, missing) != want {
		t.Fatalf("sync missing: %+v", r)
	}
	if r := k.apa("agents", "check", "--repo", repo, "--file", missing); r.code != 0 {
		t.Fatalf("after sync check must be current: %+v", r)
	}
	r = k.apa("agents", "sync", "--repo", repo, "--file", missing)
	if r.code != 0 || verdict(t, r)["written"] != false {
		t.Fatalf("sync of a current file writes nothing: %+v", r)
	}
}

// ACT-AGM-05：漂移时 sync 不覆盖手改内容，退出码 6；本人选择"覆盖"后才写入。
func TestSyncDriftBlockedUntilOverride(t *testing.T) {
	k := newKernel(t)
	want := k.rendered()
	f := write(t, "# hand edited\n")
	r := k.apa("agents", "sync", "--repo", repo, "--file", f)
	if r.code != domain.ExitConflict || read(t, f) != "# hand edited\n" {
		t.Fatalf("drift sync must exit 6 and keep the file: %+v", r)
	}
	if verdict(t, r)["written"] != false || !strings.Contains(r.errOut, `"conflict"`) {
		t.Fatalf("drift sync output: %+v", r)
	}
	item, _ := verdict(t, k.apa("agents", "check", "--repo", repo, "--file", f))["item"].(string)
	owner := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	if _, err := k.svc.Resolve(bg, owner, agentsmd.ResolveIn{Item: item, Choice: agentsmd.ChoiceOverride}); err != nil {
		t.Fatal(err)
	}
	r = k.apa("agents", "sync", "--repo", repo, "--file", f)
	if r.code != 0 || read(t, f) != want {
		t.Fatalf("after override sync must write: %+v", r)
	}
}

// 失败语义：未知仓库 → 5；未知子命令、缺 repo → 2；内核不可达 → 7。
func TestFailures(t *testing.T) {
	k := newKernel(t)
	f := write(t, "")
	if r := k.apa("agents", "check", "--repo", "https://example.com/none.git", "--file", f); r.code != domain.ExitNotFound {
		t.Fatalf("unknown repo: %+v", r)
	}
	if r := k.apa("agents", "bogus"); r.code != domain.ExitUsage {
		t.Fatalf("bogus: %+v", r)
	}
	dir := t.TempDir() // 非 git 目录，且未给 --repo
	if r := k.apa("agents", "check", "--file", filepath.Join(dir, "AGENTS.md")); r.code != domain.ExitUsage {
		t.Fatalf("no repo: %+v", r)
	}
	k.url = "http://127.0.0.1:1"
	if r := k.apa("agents", "check", "--repo", repo, "--file", f); r.code != domain.ExitUnreachable {
		t.Fatalf("unreachable: %+v", r)
	}
}

// --repo 缺省取工作区 origin；参数错误与文件读写错误都是明确的失败，而不是静默成功。
func TestRepoFromOriginAndLocalErrors(t *testing.T) {
	k := newKernel(t)
	dir := t.TempDir()
	for _, a := range [][]string{{"init"}, {"remote", "add", "origin", repo}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, a...)...).CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, out)
		}
	}
	f := filepath.Join(dir, "AGENTS.md")
	if r := k.apa("agents", "sync", "--file", f); r.code != 0 || read(t, f) != k.rendered() {
		t.Fatalf("sync with origin-derived repo: %+v", r)
	}
	for name, args := range map[string][]string{
		"no subcommand": {"agents"},
		"bad flag":      {"agents", "check", "--nope"},
		"extra arg":     {"agents", "check", "x"},
		"file is a dir": {"agents", "check", "--repo", repo, "--file", dir},
	} {
		if r := k.apa(args...); r.code != domain.ExitUsage {
			t.Fatalf("%s: want exit 2, got %+v", name, r)
		}
	}
	if r := k.apa("agents", "sync", "--repo", repo, "--file", filepath.Join(dir, "no", "such", "AGENTS.md")); r.code != domain.ExitUsage {
		t.Fatalf("unwritable target dir: %+v", r)
	}
}
