package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

var owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

const workHost = "work-host.cli"

// readerBin 是真实的只读读取器可执行文件，TestMain 构建一次。
var readerBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hosted-reader")
	if err != nil {
		panic(err)
	}
	readerBin = filepath.Join(dir, "hosted-reader")
	if out, err := exec.Command("go", "build", "-o", readerBin, "../../sources/hostedreader/cmd/hosted-reader").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo 是一个带 docs/design.md 的远程（裸库）、一份检出和一份用来推新提交的工作副本。
type repo struct{ remote, checkout, work, commit string }

func newRepo(t *testing.T) repo {
	t.Helper()
	root := t.TempDir()
	r := repo{remote: filepath.Join(root, "remote.git"), checkout: filepath.Join(root, "checkout"), work: filepath.Join(root, "work")}
	git(t, root, "init", "-q", "-b", "main", r.work)
	os.MkdirAll(filepath.Join(r.work, "docs"), 0o755)
	os.WriteFile(filepath.Join(r.work, "docs/design.md"), []byte("# design v1\n"), 0o644)
	git(t, r.work, "add", ".")
	git(t, r.work, "commit", "-q", "-m", "init")
	git(t, root, "clone", "-q", "--bare", r.work, r.remote)
	git(t, r.work, "remote", "add", "origin", r.remote)
	git(t, root, "clone", "-q", r.remote, r.checkout)
	r.commit = git(t, r.work, "rev-parse", "HEAD")
	return r
}

// push 在工作副本提交一次并推到远程，返回新 commit。
func (r repo) push(t *testing.T, msg string, change func()) string {
	t.Helper()
	change()
	git(t, r.work, "add", "-A")
	git(t, r.work, "commit", "-q", "-m", msg)
	git(t, r.work, "push", "-q", "origin", "main")
	return git(t, r.work, "rev-parse", "HEAD")
}

type env struct {
	st   *store.Store
	x    *command.Executor
	au   *auth.Auth
	svc  *Service
	d    *proc.Dispatcher
	errs chan error
	sec  map[string]string
	prj  string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, executors.Module, materials.Module, proc.Module, delegation.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, au: auth.New(nil), errs: make(chan error, 16), sec: map[string]string{}}
	e.x = command.New(st, time.Now)
	for _, f := range []func() error{
		func() error { return projects.Register(e.x) },
		func() error { return items.Register(e.x, items.NewKinds()) },
		func() error { return executors.Register(e.x, executors.Options{Owner: "owner"}) },
		func() error { return materials.Register(e.x) },
		func() error { return Register(e.x) },
		func() error {
			return delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { e.errs <- err }})
		},
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	e.svc, err = New(e.x, Options{Reader: []string{readerBin}, Host: "apa-host", Owner: "owner",
		Kick: func() { e.d.Dispatch(bg) }, OnError: func(_ string, err error) { e.errs <- err }})
	if err != nil {
		t.Fatal(err)
	}
	e.d = proc.New(st, config.Default(), proc.Options{OnResult: e.svc.OnResult})
	t.Cleanup(e.d.Close)
	e.must(t, owner, "executor.put", map[string]any{"id": workHost, "kind": "external-cli"})
	e.must(t, owner, "executor.put", map[string]any{"id": "other-host.cli", "kind": "external-cli"})
	e.issue(t, "WRK", auth.Spec{Kind: auth.Worker, Executor: workHost})
	e.issue(t, "OBS", auth.Spec{Kind: auth.Observer})
	e.prj = e.id(t, e.must(t, owner, "project.create", map[string]any{"name": "acc-open"}))
	return e
}

func (e *env) issue(t *testing.T, name string, s auth.Spec) {
	t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, secret, err := e.au.Issue(tx, s)
		e.sec[name] = secret
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) as(name string) command.Identity {
	return command.Bearer{Auth: e.au, Secret: e.sec[name], Origin: auth.Origin{Harness: "h"}}
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) command.Result {
	t.Helper()
	r, err := e.do(id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func (e *env) id(t *testing.T, r command.Result) string {
	t.Helper()
	var out struct{ ID string }
	if err := json.Unmarshal(r.Value, &out); err != nil || out.ID == "" {
		t.Fatalf("no id in %s", r.Value)
	}
	return out.ID
}

func exitOf(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func (e *env) wantExit(t *testing.T, want int, id command.Identity, cmd string, in any) {
	t.Helper()
	if _, err := e.do(id, cmd, in); exitOf(err) != want {
		t.Fatalf("%s: want exit %d, got %v", cmd, want, err)
	}
}

// register 登记仓库并给项目设置本机检出；返回材料 id。
func (e *env) register(t *testing.T, r repo, path string) string {
	t.Helper()
	e.must(t, owner, "project.repo.add", map[string]any{"id": e.prj, "remote": r.remote})
	e.must(t, owner, "project.checkout.set", map[string]any{"id": e.prj, "remote": r.remote, "host": "apa-host", "path": r.checkout})
	res := e.must(t, e.as("WRK"), "material.add", map[string]any{"project": e.prj, "kind": "design", "title": "Design", "tags": []string{"arch"}, "repo": r.remote, "path": path})
	var out Result
	json.Unmarshal(res.Value, &out)
	return out.Material
}

func (e *env) entry(t *testing.T, id string) Entry {
	t.Helper()
	var en Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { en, err = Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return en
}

// await 轮询直到读取不再是"读取中"，模拟页面轮询。
func (e *env) await(t *testing.T, id string, force bool) View {
	t.Helper()
	read := e.svc.Content
	if force {
		read = e.svc.Refresh
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		v, err := read(bg, id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != StatusReading {
			return v
		}
		force, read = false, e.svc.Content
		if time.Now().After(deadline) {
			t.Fatal("reader never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *env) runs(t *testing.T) int {
	t.Helper()
	var n int
	e.st.View(bg, func(tx *store.Tx) error { rs, err := proc.List(tx); n = len(rs); return err })
	return n
}

func (e *env) events(t *testing.T, object string) []domain.Event {
	t.Helper()
	var out []domain.Event
	e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, object)
		for _, en := range es {
			out = append(out, en.Event)
		}
		return err
	})
	return out
}

func (e *env) needsInput(t *testing.T) []domain.Item {
	t.Helper()
	var its []domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		its, err = items.List(tx, items.Filter{Phase: domain.PhaseNeedsInput})
		return
	}); err != nil {
		t.Fatal(err)
	}
	return its
}

// ACT-HST-01/02：登记只记位置，无 Revision，commit 被拒绝（退出码 6）。
func TestAddRegistersLocationOnlyAndCommitIsRefused(t *testing.T) {
	e := setup(t)
	r := newRepo(t)
	id := e.register(t, r, "docs/./design.md")

	en := e.entry(t, id)
	if en.Repo != r.remote || en.Path != "docs/design.md" || en.Material.Location != r.remote+":docs/design.md" ||
		en.Material.Title != "Design" || en.Material.Kind != "design" || en.Commit != "" {
		t.Fatalf("entry: %+v", en)
	}
	e.st.View(bg, func(tx *store.Tx) error {
		if h, err := materials.History(tx, id); err != nil || len(h) != 0 {
			t.Fatalf("hosted material must have no revisions: %v %v", h, err)
		}
		if _, _, err := materials.Show(tx, id, ""); exitOf(err) != 5 {
			t.Fatalf("show has nothing to return for hosted: %v", err)
		}
		return nil
	})
	e.wantExit(t, 6, e.as("WRK"), "material.commit", map[string]any{"id": id, "content": "x", "reason": "r"})

	// 不入全文：读取前后账本里都没有正文副本。
	e.await(t, id, false)
	e.st.View(bg, func(tx *store.Tx) error {
		if h, _ := materials.History(tx, id); len(h) != 0 {
			t.Fatal("reading must not create a revision")
		}
		return nil
	})
	if evs := e.events(t, id); len(evs) == 0 || evs[0].Type != domain.EvMaterialRevised || strings.Contains(string(evs[0].Payload), "design v1") {
		t.Fatalf("events: %+v", evs)
	}
}

func TestAddValidationAndAuthz(t *testing.T) {
	e := setup(t)
	good := map[string]any{"project": e.prj, "kind": "design", "repo": "git@example.com:o/r.git", "path": "docs/a.md"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range good {
			m[a] = b
		}
		m[k] = v
		return m
	}
	e.wantExit(t, 4, e.as("OBS"), "material.add", good)
	for name, in := range map[string]map[string]any{
		"abs path":     with("path", "/etc/passwd"),
		"escaping":     with("path", "../x.md"),
		"empty path":   with("path", ""),
		"empty repo":   with("repo", " "),
		"flag as repo": with("repo", "--upload-pack=x"),
		"guideline":    with("kind", "guideline"),
		"unknown kind": with("kind", "nope"),
		"no project":   with("project", ""),
	} {
		if _, err := e.do(e.as("WRK"), "material.add", in); exitOf(err) != 2 {
			t.Fatalf("%s: want exit 2, got %v", name, err)
		}
	}
	e.wantExit(t, 5, e.as("WRK"), "material.add", with("project", "prj_missing"))
	// 标题缺省取文件名。
	var added Result
	json.Unmarshal(e.must(t, e.as("WRK"), "material.add", good).Value, &added)
	id := added.Material
	if en := e.entry(t, id); en.Material.Title != "a.md" {
		t.Fatalf("title: %q", en.Material.Title)
	}
	// 自有型材料不是托管型。
	var own struct{ Material string }
	r := e.must(t, e.as("WRK"), "material.commit", map[string]any{"project": e.prj, "kind": "minutes", "title": "m", "content": "c", "reason": "r", "based_on": ""})
	json.Unmarshal(r.Value, &own)
	e.st.View(bg, func(tx *store.Tx) error {
		if _, err := Get(tx, own.Material); exitOf(err) != 6 {
			t.Fatalf("owned material is not hosted: %v", err)
		}
		return nil
	})
}

// 读取：经读取器拿到默认分支内容和 commit，回写最近 commit；缓存期内不重复拉起。
func TestContentReadsViaReaderAndCaches(t *testing.T) {
	e := setup(t)
	r := newRepo(t)
	id := e.register(t, r, "docs/design.md")
	os.WriteFile(filepath.Join(r.checkout, "docs/design.md"), []byte("# uncommitted edit\n"), 0o644)

	first, err := e.svc.Content(bg, id)
	if err != nil || first.Status != StatusReading {
		t.Fatalf("first read is async: %+v %v", first, err)
	}
	v := e.await(t, id, false)
	if v.Status != StatusReady || v.Content != "# design v1\n" || v.Commit != r.commit || v.Branch != "main" {
		t.Fatalf("view: %+v", v)
	}
	if got := e.entry(t, id).Commit; got != r.commit {
		t.Fatalf("latest commit not recorded: %q", got)
	}
	runs := e.runs(t)
	if v2, _ := e.svc.Content(bg, id); v2.Status != StatusReady || e.runs(t) != runs {
		t.Fatalf("cache hit must not start another run: %+v runs=%d", v2, e.runs(t))
	}

	// 新提交 + Refresh：忽略缓存，commit 变化记 Event，apa context 据此提示文档已更新。
	next := r.push(t, "v2", func() { os.WriteFile(filepath.Join(r.work, "docs/design.md"), []byte("# design v2\n"), 0o644) })
	git(t, r.checkout, "fetch", "-q", "origin")
	v = e.await(t, id, true)
	if v.Commit != next || v.Content != "# design v2\n" || e.entry(t, id).Commit != next {
		t.Fatalf("after refresh: %+v", v)
	}
	var refreshed int
	for _, ev := range e.events(t, id) {
		if ev.Type == domain.EvMaterialRevised && strings.Contains(string(ev.Payload), next) {
			refreshed++
		}
	}
	if refreshed != 1 {
		t.Fatalf("commit change should be one event, got %d", refreshed)
	}
}

// ACT-HST-04：读不到时只显示登记信息和"暂不可读"，不是错误，也不生成修正事项。
func TestUnreadableDegradesWithoutError(t *testing.T) {
	e := setup(t)
	missingRemote := filepath.Join(t.TempDir(), "gone.git")
	res := e.must(t, e.as("WRK"), "material.add", map[string]any{"project": e.prj, "kind": "design", "repo": missingRemote, "path": "docs/design.md"})
	var out Result
	json.Unmarshal(res.Value, &out)
	v := e.await(t, out.Material, false)
	if v.Status != StatusUnreadable || v.Reason == "" || v.Content != "" || v.Entry.Repo != missingRemote || v.Entry.Material.Title != "design.md" {
		t.Fatalf("view: %+v", v)
	}
	if n := len(e.needsInput(t)); n != 0 {
		t.Fatalf("transient failure must not ask the owner: %d", n)
	}
	// 失败短期缓存：页面轮询不会反复拉起读取器。
	runs := e.runs(t)
	if v2, _ := e.svc.Content(bg, out.Material); v2.Status != StatusUnreadable || e.runs(t) != runs {
		t.Fatalf("failure cache: %+v", v2)
	}
}

func TestUnparsableReaderOutputIsUnreadable(t *testing.T) {
	e := setup(t)
	r := newRepo(t)
	id := e.register(t, r, "docs/design.md")
	e.svc.OnResult(proc.Result{Run: domain.Run{Category: domain.RunReader, Target: id, State: domain.RunSucceeded, Fingerprint: fingerprint(e.entry(t, id))}, Stdout: []byte("not json")})
	v, _ := e.svc.Content(bg, id)
	if v.Status != StatusUnreadable || v.Reason == "" {
		t.Fatalf("view: %+v", v)
	}
	e.svc.OnResult(proc.Result{Run: domain.Run{Category: domain.RunTask, Target: id}}) // 其他类别忽略
	// 读取期间登记被修正：旧路径的结果不能回写（这里即便读到"路径不存在"也不能生成修正事项）。
	exit := 3
	e.svc.OnResult(proc.Result{Run: domain.Run{Category: domain.RunReader, Target: id, State: domain.RunFailed, ExitCode: &exit, Fingerprint: "old\npath"}})
	if n := len(e.needsInput(t)); n != 0 {
		t.Fatalf("obsolete result created an item: %d", n)
	}
	e.svc.OnResult(proc.Result{Run: domain.Run{Category: domain.RunReader, Target: "mat_unknown"}})
	select {
	case err := <-e.errs:
		if exitOf(err) != 5 {
			t.Fatalf("unknown material error: %v", err)
		}
	default:
		t.Fatal("a result for an unknown material must surface an error")
	}
}

// ACT-HST-05：路径在默认分支消失 → needs_input 修正登记；不重复生成；修正后恢复。
func TestPathMissingCreatesNeedsInputThenRelocateRecovers(t *testing.T) {
	e := setup(t)
	r := newRepo(t)
	id := e.register(t, r, "docs/design.md")
	if v := e.await(t, id, false); v.Status != StatusReady {
		t.Fatalf("baseline: %+v", v)
	}

	r.push(t, "move", func() {
		os.Rename(filepath.Join(r.work, "docs/design.md"), filepath.Join(r.work, "docs/arch.md"))
	})
	git(t, r.checkout, "fetch", "-q", "origin")
	v := e.await(t, id, true)
	if v.Status != StatusUnreadable || !strings.Contains(v.Reason, "not found") {
		t.Fatalf("view: %+v", v)
	}

	en := e.entry(t, id)
	its := e.needsInput(t)
	if !en.Missing || en.FixItem == "" || len(its) != 1 || its[0].ID != en.FixItem {
		t.Fatalf("entry=%+v needs_input=%d", en, len(its))
	}
	if it := its[0]; it.Project != e.prj || it.Ask == nil || !strings.Contains(it.Ask.Question, "docs/design.md") || !it.Ask.FreeText {
		t.Fatalf("fix item: %+v", it)
	}
	var sawIndexEvent bool
	for _, ev := range e.events(t, id) {
		if ev.Type == domain.EvMaterialRevised && strings.Contains(string(ev.Payload), `"path_missing":true`) {
			sawIndexEvent = true // AGENTS.md 渲染订阅这条事件重新渲染索引
		}
	}
	if !sawIndexEvent {
		t.Fatal("no material.revised event with path_missing for the index to re-render")
	}

	// 再刷新一次：已有等待本人的修正事项，不重复生成。
	e.await(t, id, true)
	if n := len(e.needsInput(t)); n != 1 {
		t.Fatalf("duplicate fix items: %d", n)
	}

	// 本人修正路径：Missing 清除，下一次读取回到 ready。
	e.must(t, e.as("WRK"), "material.relocate", map[string]any{"id": id, "path": "docs/arch.md"})
	en = e.entry(t, id)
	if en.Missing || en.Path != "docs/arch.md" || en.Material.Location != r.remote+":docs/arch.md" {
		t.Fatalf("after relocate: %+v", en)
	}
	if v := e.await(t, id, true); v.Status != StatusReady || v.Content != "# design v1\n" {
		t.Fatalf("after relocate: %+v", v)
	}
	e.wantExit(t, 2, e.as("WRK"), "material.relocate", map[string]any{"id": id, "path": "../x"})
}

func TestRefreshInputMustPickOneOutcome(t *testing.T) {
	e := setup(t)
	id := e.register(t, newRepo(t), "docs/design.md")
	e.wantExit(t, 2, e.as("WRK"), "material.refresh", map[string]any{"id": id})
	e.wantExit(t, 2, e.as("WRK"), "material.refresh", map[string]any{"id": id, "commit": "abc", "path_missing": true})
	e.wantExit(t, 5, e.as("WRK"), "material.refresh", map[string]any{"id": "mat_none", "commit": "abc"})
	e.wantExit(t, 4, e.as("OBS"), "material.refresh", map[string]any{"id": id, "commit": "abc"})
}

// ACT-HST-06：修改生成委派子事项；没有 parent 时先建 task 父事项。
func TestChangeCreatesParentAndDelegation(t *testing.T) {
	e := setup(t)
	r := newRepo(t)
	id := e.register(t, r, "docs/design.md")

	out, err := e.svc.RequestChange(bg, owner, ChangeIn{ID: id, Assignee: workHost, Request: "补充鉴权章节"})
	if err != nil || !out.ParentCreated || out.Parent == "" || out.Child == "" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	var parent, child domain.Item
	e.st.View(bg, func(tx *store.Tx) (err error) {
		if parent, err = items.Get(tx, out.Parent); err != nil {
			return
		}
		child, err = items.Get(tx, out.Child)
		return
	})
	if parent.Kind != domain.KindTask || parent.Project != e.prj || parent.Assignee != "owner" || parent.Phase != domain.PhaseWaiting {
		t.Fatalf("parent: %+v", parent)
	}
	if child.Kind != domain.KindDelegation || child.Assignee != workHost || child.Project != e.prj ||
		!strings.Contains(child.Body, r.remote) || !strings.Contains(child.Body, "docs/design.md") || !strings.Contains(child.Body, "补充鉴权章节") {
		t.Fatalf("child: %+v", child)
	}
	// 不在 APA 内改文档：没有任何 Revision。
	e.st.View(bg, func(tx *store.Tx) error {
		if h, _ := materials.History(tx, id); len(h) != 0 {
			t.Fatal("a change request must not write a revision")
		}
		return nil
	})

	// 带 parent：挂在该事项下，不再建父事项。
	p2 := e.id(t, e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open", "project": e.prj}))
	out2, err := e.svc.RequestChange(bg, owner, ChangeIn{ID: id, Assignee: workHost, Request: "改", Parent: p2})
	if err != nil || out2.ParentCreated || out2.Parent != p2 || out2.Child == "" {
		t.Fatalf("out=%+v err=%v", out2, err)
	}
}

func TestChangeRules(t *testing.T) {
	e := setup(t)
	id := e.register(t, newRepo(t), "docs/design.md")
	other := e.id(t, e.must(t, owner, "project.create", map[string]any{"name": "other"}))
	foreign := e.id(t, e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open", "project": other}))

	for name, in := range map[string]ChangeIn{
		"blank request":  {ID: id, Assignee: workHost, Request: " "},
		"no assignee":    {ID: id, Request: "x"},
		"foreign parent": {ID: id, Assignee: workHost, Request: "x", Parent: foreign},
		"missing parent": {ID: id, Assignee: workHost, Request: "x", Parent: "itm_none"},
	} {
		if _, err := e.svc.RequestChange(bg, owner, in); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	if _, err := e.svc.RequestChange(bg, owner, ChangeIn{ID: "mat_none", Assignee: workHost, Request: "x"}); exitOf(err) != 5 {
		t.Fatalf("unknown material: %v", err)
	}
	// worker 必须带 parent。
	if _, err := e.svc.RequestChange(bg, e.as("WRK"), ChangeIn{ID: id, Assignee: workHost, Request: "x"}); exitOf(err) != 2 {
		t.Fatalf("worker without parent: %v", err)
	}
	if n := len(e.openItems(t)); n != 1 { // 只剩测试自己建的 foreign
		t.Fatalf("rejected requests must leave no items behind: %d", n)
	}
}

// 名单外的执行者：退出码 21，刚建的父事项被丢弃，不留孤儿。
func TestChangeToDisallowedExecutorLeavesNoOrphan(t *testing.T) {
	e := setup(t)
	id := e.register(t, newRepo(t), "docs/design.md")
	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": e.prj, "executors": []string{workHost}})

	_, err := e.svc.RequestChange(bg, owner, ChangeIn{ID: id, Assignee: "other-host.cli", Request: "改"})
	if exitOf(err) != 21 {
		t.Fatalf("want exit 21, got %v", err)
	}
	if n := len(e.openItems(t)); n != 0 {
		t.Fatalf("orphan parent left open: %d", n)
	}
}

func (e *env) openItems(t *testing.T) []domain.Item {
	t.Helper()
	var its []domain.Item
	e.st.View(bg, func(tx *store.Tx) (err error) {
		its, err = items.List(tx, items.Filter{Phase: domain.PhaseOpen})
		return
	})
	return its
}

func TestNewRequiresReaderAndOnError(t *testing.T) {
	x := command.New(nil, time.Now)
	if _, err := New(x, Options{OnError: func(string, error) {}}); err == nil {
		t.Fatal("reader is required")
	}
	if _, err := New(x, Options{Reader: []string{"r"}}); err == nil {
		t.Fatal("OnError is required")
	}
}

func TestListReturnsOnlyHosted(t *testing.T) {
	e := setup(t)
	id := e.register(t, newRepo(t), "docs/design.md")
	e.must(t, e.as("WRK"), "material.commit", map[string]any{"project": e.prj, "kind": "minutes", "title": "m", "content": "c", "reason": "r"})
	e.st.View(bg, func(tx *store.Tx) error {
		es, err := List(tx, e.prj)
		if err != nil || len(es) != 1 || es[0].Material.ID != id {
			t.Fatalf("list: %+v %v", es, err)
		}
		return nil
	})
}

// 同一材料已有在途读取时复用，不重复入队（不派发，所以读取一直在途）。
func TestInFlightReadIsReused(t *testing.T) {
	e := setup(t)
	id := e.register(t, newRepo(t), "docs/design.md")
	e.svc.opt.Kick = nil
	for i := 0; i < 3; i++ {
		if v, err := e.svc.Content(bg, id); err != nil || v.Status != StatusReading {
			t.Fatalf("read %d: %+v %v", i, v, err)
		}
	}
	if n := e.runs(t); n != 1 {
		t.Fatalf("want 1 queued reader run, got %d", n)
	}
}
