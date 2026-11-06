package backgroundrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/background"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const (
	mainRule = "background-run/event_rules/handle-labelled"
	taskRule = "background-run/event_rules/run-labelled"
	// 无头运行替身：记下提示词与运行凭据，等测试以运行者身份回执后退出。
	script = `printf '%s' "$1" > "$2/prompt.seen"
printf '%s' "$APA_TOKEN" > "$2/token"
while [ ! -f "$2/go" ]; do sleep 0.05; done
echo '{"session_id":"s1"}'
`
)

// taskRuleYAML 是部署方追加的 runner=task 规则：executor 因部署而异，所以内置包不带它，只带任务型提示词。
const taskRuleYAML = `  - id: run-labelled
    mode: observe
    when: {event: item.phase_changed, to_phase: open, kind: [task], tag_any: [run-it]}
    do:
      - {action: start_background_run, runner: task, prompt: run-task, access: read, executor: %s}
`

type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	runs string
	done chan proc.Result
}

// 真实内置包 + 真实规则引擎与后台运行模块；只有无头 harness 是脚本替身（外部边界）。
func newEnv(t *testing.T, taskExecutor string) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, instances.Module,
		items.Module, executors.Module, proc.Module, runidentity.Module, rules.Module, background.Module, delegation.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, runs: filepath.Join(t.TempDir(), "runs"), done: make(chan proc.Result, 8)}
	e.x = command.New(st, func() time.Time { return t0 })
	kinds := items.NewKinds()
	for _, err := range []error{projects.Register(e.x), items.Register(e.x, kinds), executors.Register(e.x, executors.Options{Owner: "owner"}), rules.Register(e.x),
		delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Errorf("delegation: %v", err) }})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ld := packs.New(packs.Options{Builtin: e.packFS(taskExecutor), Kinds: kinds})
	rej, err := ld.Load()
	if err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %+v", err, rej)
	}
	var svc *background.Service
	disp := proc.New(st, config.Config{MaxSubprocesses: 4, MaxShortSubprocesses: 1}, proc.Options{
		Env:      func(r domain.Run) map[string]string { return svc.Env(r) },
		OnResult: func(r proc.Result) { svc.Settle(bg, r); e.done <- r },
	})
	t.Cleanup(disp.Close)
	svc, err = background.New(background.Options{Store: st, Exec: e.x, Packs: ld, Queue: disp, RunsDir: e.runs, APIURL: "http://apa.local",
		Owner: "owner", Now: func() time.Time { return t0 }, OnError: func(_ string, err error) { t.Errorf("settle: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rules.New(rules.Options{Store: st, Exec: e.x, Packs: ld, Owner: "owner", Now: func() time.Time { return t0 },
		Handlers: map[string]rules.Handler{background.Action: svc.Handler()}, OnError: func(err error) { t.Errorf("rules: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	// harness：Web 默认 harness，权限上限 edit。
	e.must(owner, "executor.put", map[string]any{"id": "work-host.cli", "kind": "external-cli"})
	sh := filepath.Join(t.TempDir(), "h.sh")
	if err := os.WriteFile(sh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e.must(owner, "executor.register", map[string]any{"harness": "hx", "headless": map[string]any{
		"command": []string{"sh", sh, "{prompt}", "{run_dir}"}, "session": map[string]any{"extract": "$.session_id"},
		"access": map[string][]string{"read": {}, "edit": {}}}})
	if err := st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "t", func(p *policy.Policy) error {
			p.PermissionCap.Default, p.DefaultHarness = policy.PermEdit, "hx"
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// packFS 取内置 background-run 包；taskExecutor 非空时按 C.12 的"用户覆盖"方式追加一条 runner=task 规则。
func (e *env) packFS(taskExecutor string) fstest.MapFS {
	m := fstest.MapFS{}
	err := readAll(rootpacks.FS, "background-run", func(p string, b []byte) {
		if p == "background-run/pack.yaml" && taskExecutor != "" {
			b = append(b, []byte(strings.Replace(taskRuleYAML, "%s", taskExecutor, 1))...)
		}
		m[p] = &fstest.MapFile{Data: b}
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) must(id command.Identity, cmd string, in any) json.RawMessage {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "x", WebURL: "http://web"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func (e *env) auto(rule string) {
	e.must(owner, "rule.set_mode", map[string]string{"rule": rule, "mode": "auto"})
}

func (e *env) newOpen(title string, tags []string, extra map[string]any) string {
	e.t.Helper()
	in := map[string]any{"kind": "task", "title": title, "tags": tags}
	for k, v := range extra {
		in[k] = v
	}
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", in), &out)
	e.must(owner, "item.transition", map[string]any{"id": out.ID, "to": "open", "reason": "go"})
	return out.ID
}

func (e *env) item(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) runs_() []proc.Record {
	e.t.Helper()
	var rs []proc.Record
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { rs, err = proc.List(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return rs
}

func (e *env) observed() int {
	n := 0
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, _ := eventlog.Read(tx, 0, 10000)
		for _, x := range es {
			if x.Event.Type == rules.EvObserved {
				n++
			}
		}
		return nil
	})
	return n
}

func waitFile(t *testing.T, p string) string {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	t.Fatalf("%s never appeared", p)
	return ""
}

func (e *env) finish(runID string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.runs, runID, "go"), nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
	select {
	case r := <-e.done:
		if r.Run.State != domain.RunSucceeded {
			e.t.Fatalf("run = %+v", r.Run)
		}
	case <-time.After(15 * time.Second):
		e.t.Fatal("run did not finish")
	}
}

func readAll(fsys interface {
	ReadFile(string) ([]byte, error)
	ReadDir(string) ([]os.DirEntry, error)
}, dir string, f func(string, []byte)) error {
	ents, err := fsys.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, d := range ents {
		p := dir + "/" + d.Name()
		if d.IsDir() {
			if err := readAll(fsys, p, f); err != nil {
				return err
			}
			continue
		}
		b, err := fsys.ReadFile(p)
		if err != nil {
			return err
		}
		f(p, b)
	}
	return nil
}

// C.14：包只声明 start_background_run 规则样例（初始 observe，不是 auto）、主会话角色提示词与任务型提示词；
// 无 kind、时钟规则；提示词不出现 harness 名。
func TestPackDeclaresWhatC14Lists(t *testing.T) {
	e := newEnv(t, "")
	_ = e
	ld := packs.New(packs.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	rej, err := ld.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "background-run" {
			t.Fatalf("rejected by loader: %s", r.Reason)
		}
	}
	p, ok := ld.Get("background-run")
	if !ok {
		t.Fatal("background-run is not an active builtin pack")
	}
	if len(p.Kinds) != 0 || len(p.ClockRules) != 0 || len(p.EventRules) != 1 {
		t.Fatalf("kinds=%d clock=%d event=%d", len(p.Kinds), len(p.ClockRules), len(p.EventRules))
	}
	r := p.EventRules[0]
	if r.ID != "handle-labelled" || r.Mode != "observe" || len(r.Do) != 1 || r.Do[0]["action"] != "start_background_run" ||
		r.Do[0]["runner"] != "main_session" || r.Do[0]["prompt"] != "handle-item" || r.Do[0]["access"] != "edit" || r.Do[0]["cwd"] != "run_dir" {
		t.Fatalf("rule: %+v", r)
	}
	if _, set := r.Do[0]["harness"]; set {
		t.Fatal("sample rule pins a harness; it must use the Web default")
	}
	h := p.Prompts["handle-item"]
	if h.Meta.RunAs != "main_session" || h.Meta.EscalateTo != "" || !strings.Contains(h.Body, "{item_id}") ||
		!strings.Contains(h.Body, "apa delegate") || !strings.Contains(h.Body, "apa ask") || !strings.Contains(h.Body, "apa report") {
		t.Fatalf("handle-item: %+v", h)
	}
	k := p.Prompts["run-task"]
	if k.Meta.Agent != "task" || !strings.Contains(k.Body, "{item_id}") || !strings.Contains(k.Body, "apa report {item_id} --outcome done") {
		t.Fatalf("run-task: %+v", k)
	}
	for name, pr := range p.Prompts {
		for _, hn := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), hn) {
				t.Errorf("prompt %s mentions %q", name, hn)
			}
		}
	}
}

// flow-bg-main-session（ACT-BGM-01/02）：加载后规则是 observe，命中只记录"本来会做什么"，不拉起运行。
func TestMainSessionObserveOnlyRecords(t *testing.T) {
	e := newEnv(t, "")
	e.newOpen("修复", []string{"agent-ok"}, nil)
	e.newOpen("无标签", nil, nil)
	if n := len(e.runs_()); n != 0 {
		t.Fatalf("observe launched %d runs", n)
	}
	if n := e.observed(); n != 1 {
		t.Fatalf("observed records = %d (only the labelled item matches)", n)
	}
}

// flow-bg-main-session（ACT-BGM-03/05）：本人切到 auto 后，内置规则拉起 main_session 无头运行，
// 目标是事项本身、不占 assignee，access=edit；提示词是包里的处理事项模板并代入事项 id。
func TestMainSessionAutoLaunchesWithHandleItemPrompt(t *testing.T) {
	e := newEnv(t, "")
	e.auto(mainRule)
	id := e.newOpen("修复", []string{"agent-ok"}, nil)
	rs := e.runs_()
	if len(rs) != 1 {
		t.Fatalf("runs = %d", len(rs))
	}
	r := rs[0].Run
	if r.Category != domain.RunBackground || r.Runner != "main_session" || r.Launch != "headless" || r.Target != id ||
		r.Access != "edit" || r.Harness != "hx" || r.Basis == nil || r.Basis.RuleID != mainRule {
		t.Fatalf("run = %+v basis=%+v", r, r.Basis)
	}
	if it := e.item(id); it.Assignee != "" {
		t.Fatalf("runner took the assignee: %q", it.Assignee)
	}
	prompt := waitFile(t, filepath.Join(e.runs, r.ID, "prompt.seen"))
	if !strings.Contains(prompt, "你在处理事项 "+id) || !strings.Contains(prompt, "apa delegate") || strings.Contains(prompt, "{item_id}") {
		t.Fatalf("prompt = %q", prompt)
	}
	// 运行者委派后把事项转 waiting 并退出：内核不再拉起第二次。
	secret := waitFile(t, filepath.Join(e.runs, r.ID, "token"))
	e.must(runidentity.Identity{Secret: secret}, "item.transition", map[string]any{"id": id, "to": "waiting", "reason": "delegated",
		"waiting_on": map[string]any{"kind": "external", "ref": "child"}})
	e.finish(r.ID)
	if got := e.item(id).Phase; got != domain.PhaseWaiting || len(e.runs_()) != 1 {
		t.Fatalf("phase=%s runs=%d", got, len(e.runs_()))
	}
}

// 规则的 when 是平铺条件：没有 agent-ok 标签、或 kind 不是 task 的事项即使 auto 也不被拉起。
func TestUnlabelledItemIsNotLaunched(t *testing.T) {
	e := newEnv(t, "")
	e.auto(mainRule)
	e.newOpen("无标签", []string{"other"}, nil)
	if n := len(e.runs_()); n != 0 {
		t.Fatalf("runs = %d", n)
	}
}

// flow-bg-task-runner（ACT-BGT-01）：包里的任务型提示词驱动 runner=task：内核先建子事项（assignee=执行者），
// Run 目标是子事项，运行者按提示词 `apa report` 在子事项上回执 done 带证据，父事项随之回 open。
func TestTaskRunnerPromptWorksOnChild(t *testing.T) {
	e := newEnv(t, "work-host.cli")
	e.auto(taskRule)
	var p struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "project.create", map[string]any{"name": "P"}), &p)
	e.must(owner, "project.allowed_executors.set", map[string]any{"id": p.ID, "executors": []string{"work-host.cli"}})
	parent := e.newOpen("修复", []string{"run-it"}, map[string]any{"project": p.ID})
	rs := e.runs_()
	if len(rs) != 1 || rs[0].Run.Runner != "task" || rs[0].Run.Target == parent || rs[0].Run.Access != "read" {
		t.Fatalf("runs = %+v", rs)
	}
	child := e.item(rs[0].Run.Target)
	if child.Assignee != "work-host.cli" || child.Phase != domain.PhaseOpen {
		t.Fatalf("child = %+v", child)
	}
	prompt := waitFile(t, filepath.Join(e.runs, rs[0].Run.ID, "prompt.seen"))
	if !strings.Contains(prompt, "子事项 "+child.ID) || !strings.Contains(prompt, "apa report "+child.ID+" --outcome done") {
		t.Fatalf("prompt = %q", prompt)
	}
	secret := waitFile(t, filepath.Join(e.runs, rs[0].Run.ID, "token"))
	e.must(runidentity.Identity{Secret: secret}, "delegate.report", map[string]any{"id": child.ID, "outcome": "done", "summary": "fixed",
		"evidence": map[string]any{"pr": "https://example.invalid/pr/1"}})
	e.finish(rs[0].Run.ID)
	if got := e.item(child.ID).Phase; got != domain.PhaseDone {
		t.Fatalf("child phase = %s", got)
	}
	if got := e.item(parent).Phase; got != domain.PhaseOpen {
		t.Fatalf("parent phase = %s", got)
	}
}

// flow-bg-task-runner（ACT-BGT-02）：执行者不在项目名单内：不拉起，事项转 needs_input。
func TestTaskRunnerExecutorOutsideAllowlistIsRefused(t *testing.T) {
	e := newEnv(t, "other-host.cli")
	e.auto(taskRule)
	var p struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "project.create", map[string]any{"name": "P"}), &p)
	e.must(owner, "project.allowed_executors.set", map[string]any{"id": p.ID, "executors": []string{"work-host.cli"}})
	parent := e.newOpen("修复", []string{"run-it"}, map[string]any{"project": p.ID})
	it := e.item(parent)
	if len(e.runs_()) != 0 || it.Phase != domain.PhaseNeedsInput || it.Ask == nil || !strings.Contains(it.Ask.Question, "other-host.cli") {
		t.Fatalf("runs=%d item=%+v", len(e.runs_()), it)
	}
}

// C.11：包里把 mode 写成 auto 会被加载器拒绝（包不能自行放行无人值守）。
func TestPackCannotDeclareAuto(t *testing.T) {
	m := fstest.MapFS{}
	_ = readAll(rootpacks.FS, "background-run", func(p string, b []byte) {
		if p == "background-run/pack.yaml" {
			b = []byte(strings.Replace(string(b), "mode: observe", "mode: auto", 1))
		}
		m[p] = &fstest.MapFile{Data: b}
	})
	ld := packs.New(packs.Options{Builtin: m, Kinds: items.NewKinds()})
	rej, _ := ld.Load()
	if len(rej) != 1 || !strings.Contains(rej[0].Reason, "auto") {
		t.Fatalf("rejections = %+v", rej)
	}
}
