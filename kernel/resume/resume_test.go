package resume

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const packYAML = `name: bgr
version: 1.0.0
description: 后台运行
event_rules:
  - {id: main, mode: observe, when: {event: item.phase_changed, to_phase: open, tag_any: [agent-ok]},
     do: [{action: start_background_run, runner: main_session, prompt: handle, access: edit}]}
  - {id: taskr, mode: observe, when: {event: item.phase_changed, to_phase: open, tag_any: [taskr]},
     do: [{action: start_background_run, runner: task, prompt: work, access: read, executor: work-host.cli}]}
`

const handleMD = "---\nrun_as: main_session\ntimeout: 30s\n---\n处理事项 {item_id}。\n"
const workMD = "---\nagent: task\naccess: read\ntimeout: 30s\n---\n在事项 {item_id} 上完成工作。\n"

// 无头运行替身：记下提示词、参数与运行凭据；带 --resume 且 gone 文件存在时把其内容写到 stderr 并以退出码 7 失败；
// 否则等测试放行后在 stdout 末段写会话摘要。
const script = `printf '%s' "$1" > "$2/prompt.seen"
printf '%s ' "$@" > "$2/args"
printf '%s' "$APA_TOKEN" > "$2/token"
case " $* " in *" --resume "*) if [ -f @DIR@/gone ]; then cat @DIR@/gone >&2; exit 7; fi;; esac
while [ ! -f "$2/go" ]; do sleep 0.05; done
echo '{"session_id":"sess-1"}'
`

type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	bgs  *background.Service
	svc  *Service
	eng  *rules.Engine
	disp *proc.Dispatcher
	runs string
	dir  string
	done chan proc.Result
	errs chan error
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, instances.Module,
		items.Module, executors.Module, proc.Module, runidentity.Module, rules.Module, runner.Module, background.Module, delegation.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, runs: filepath.Join(t.TempDir(), "runs"), dir: t.TempDir(), done: make(chan proc.Result, 16), errs: make(chan error, 16)}
	e.x = command.New(st, func() time.Time { return t0 })
	kinds := items.NewKinds()
	for _, err := range []error{projects.Register(e.x), items.Register(e.x, kinds), executors.Register(e.x, executors.Options{Owner: "owner"}),
		rules.Register(e.x), decisions.Register(e.x), delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Errorf("delegation: %v", err) }})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	ld := packs.New(packs.Options{Builtin: fstest.MapFS{
		"bgr/pack.yaml":         {Data: []byte(packYAML)},
		"bgr/prompts/handle.md": {Data: []byte(handleMD)},
		"bgr/prompts/work.md":   {Data: []byte(workMD)},
	}, Kinds: kinds})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %+v", err, rej)
	}
	onErr := func(_ string, err error) { e.errs <- err }
	e.must(owner, "executor.put", map[string]any{"id": "work-host.cli", "kind": "external-cli"})
	var bgs *background.Service
	e.disp = proc.New(st, config.Config{MaxSubprocesses: 4, MaxShortSubprocesses: 1}, proc.Options{
		Env: func(r domain.Run) map[string]string { return bgs.Env(r) },
		OnResult: func(r proc.Result) {
			if !e.svc.Settle(bg, r) { // 会话失效已改用新会话时，不再按失败收尾
				bgs.Settle(bg, r)
			}
			e.done <- r
		},
	})
	t.Cleanup(e.disp.Close)
	bgs, err = background.New(background.Options{Store: st, Exec: e.x, Packs: ld, Queue: e.disp, RunsDir: e.runs, APIURL: "http://apa.local",
		Owner: "owner", Now: func() time.Time { return t0 }, OnError: onErr})
	if err != nil {
		t.Fatal(err)
	}
	e.bgs = bgs
	e.svc, err = New(Options{Store: st, Packs: ld, Queue: e.disp, RunsDir: e.runs, Owner: "owner", Now: func() time.Time { return t0 }, OnError: onErr})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rules.New(rules.Options{Store: st, Exec: e.x, Packs: ld, Owner: "owner", Now: func() time.Time { return t0 },
		Handlers: map[string]rules.Handler{background.Action: bgs.Handler()}, OnError: func(err error) { t.Errorf("rules: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	e.eng = eng
	if err := eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) must(id command.Identity, cmd string, in any) command.Result {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "x", WebURL: "http://web"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

// harness 登记 hx：三档权限；resume 为真时声明续接；gone 为真时声明退出码 7 且输出含 "no such session" 表示会话不存在。
func (e *env) harness(resume, gone bool) {
	e.t.Helper()
	path := filepath.Join(e.dir, "hx.sh")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(script, "@DIR@", e.dir)), 0o700); err != nil {
		e.t.Fatal(err)
	}
	h := map[string]any{"command": []string{"sh", path, "{prompt}", "{run_dir}"}, "session": map[string]any{"extract": "$.session_id"},
		"access": map[string]any{"read": []string{}, "edit": []string{}, "full": []string{}}}
	if resume {
		h["resume"] = []string{"--resume", "{session_id}"}
	}
	if gone {
		h["exit_codes"] = map[string][]int{SessionMissing: {7}}
		h["exit_output"] = map[string][]string{SessionMissing: {"no such session"}}
	}
	e.must(owner, "executor.register", map[string]any{"harness": "hx", "headless": h})
	e.setPolicy(func(p *policy.Policy) { p.PermissionCap.Default, p.DefaultHarness = policy.PermEdit, "hx" })
}

func (e *env) setPolicy(f func(*policy.Policy)) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error { f(p); return nil })
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) item(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) runList() []proc.Record {
	e.t.Helper()
	var rs []proc.Record
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { rs, err = proc.List(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return rs
}

func (e *env) ofType(typ string) []domain.Event {
	e.t.Helper()
	var out []domain.Event
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 10000)
		for _, x := range es {
			if x.Event.Type == typ {
				out = append(out, x.Event)
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) waitResult() proc.Result {
	e.t.Helper()
	select {
	case r := <-e.done:
		select {
		case err := <-e.errs:
			e.t.Fatalf("settle: %v", err)
		default:
		}
		return r
	case <-time.After(15 * time.Second):
		e.t.Fatal("run did not finish")
	}
	return proc.Result{}
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	t.Fatalf("%s never appeared", path)
	return ""
}

func (e *env) release(runID string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.runs, runID, "go"), nil, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) auto(rule string) {
	e.t.Helper()
	e.must(owner, "rule.set_mode", map[string]string{"rule": "bgr/event_rules/" + rule, "mode": "auto"})
}

func (e *env) newItem(tags []string, extra map[string]any) string {
	e.t.Helper()
	in := map[string]any{"kind": "task", "title": "整理资料", "tags": tags}
	for k, v := range extra {
		in[k] = v
	}
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", in).Value, &out)
	return out.ID
}

func (e *env) project(execs ...string) string {
	e.t.Helper()
	var p struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "project.create", map[string]any{"name": "P"}).Value, &p)
	e.must(owner, "project.allowed_executors.set", map[string]any{"id": p.ID, "executors": execs})
	return p.ID
}

// asked 走完"后台运行 → 运行者提带 resume_note 的决策请求 → 运行结束 → 本人回答"，返回被问的事项 id。
func (e *env) asked(tag string, extra map[string]any) (target string) {
	e.t.Helper()
	target = e.pending(tag, extra)
	e.must(owner, "decision.answer", map[string]any{"id": target, "choice": "B"})
	return target
}

// pending 走到本人回答之前：运行者的目标是该事项（main_session 就是事项本身，task 是内核建的子事项）。
func (e *env) pending(tag string, extra map[string]any) (target string) {
	e.t.Helper()
	root := e.newItem([]string{tag}, extra)
	e.must(owner, "item.transition", map[string]any{"id": root, "to": "open", "reason": "go"})
	r := e.runList()[0].Run
	target = r.Target
	run := runidentity.Identity{Secret: waitFile(e.t, filepath.Join(e.runs, r.ID, "token"))}
	e.must(run, "decision.request", map[string]any{"id": target, "ask": map[string]any{"question": "付款走哪个账户？",
		"options": []map[string]any{{"id": "A", "label": "甲"}, {"id": "B", "label": "乙"}}, "resume_note": "已核对完发票，下一步按所选账户付款"}})
	e.release(r.ID)
	e.waitResult()
	if it := e.item(target); it.Phase != domain.PhaseNeedsInput {
		e.t.Fatalf("after the run: %+v", it)
	}
	return target
}

func lastRun(rs []proc.Record) proc.Record { return rs[len(rs)-1] }

// ACT-RES-02 的内核侧：支持续接且会话可用：同一会话 id 续接，提示词带回 resume_note 和回答，运行凭据是新的；同一回答只续接一次。
func TestResumesWithTheSameSession(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("main")
	id := e.asked("agent-ok", nil)
	first := e.runList()[0].Run
	firstToken := waitFile(t, filepath.Join(e.runs, first.ID, "token"))

	out, err := e.svc.Resume(bg, id)
	if err != nil || out != Resumed {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	rs := e.runList()
	if len(rs) != 2 {
		t.Fatalf("runs = %d", len(rs))
	}
	r := lastRun(rs).Run
	if r.Target != id || r.SessionID != "sess-1" || r.Category != domain.RunBackground || r.Basis == nil || r.Basis.RuleID != "bgr/event_rules/main" {
		t.Fatalf("resume run = %+v", r)
	}
	dir := filepath.Join(e.runs, r.ID)
	// 会话的客户端环境在建会话时冻结：续接同一会话要沿用原来的运行上下文路径，目录是自己的。
	if sp := lastRun(rs).Spec; sp.RunDir != dir || sp.ContextName != first.ID {
		t.Fatalf("resume spec = %+v, want run dir %s, context %s", sp, dir, first.ID)
	}
	if args := waitFile(t, filepath.Join(dir, "args")); !strings.Contains(args, "--resume sess-1") {
		t.Fatalf("args = %q", args)
	}
	prompt := waitFile(t, filepath.Join(dir, "prompt.seen"))
	for _, want := range []string{"已核对完发票，下一步按所选账户付款", "乙", "付款走哪个账户"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q: %q", want, prompt)
		}
	}
	if tok := waitFile(t, filepath.Join(dir, "token")); tok == firstToken {
		t.Fatal("the resume run reused the old credential")
	}
	// 在途时与已处理后再次调用都跳过，不重复拉起。
	if out, err := e.svc.Resume(bg, id); err != nil || out != Skipped {
		t.Fatalf("second call (in flight): %s %v", out, err)
	}
	e.release(r.ID)
	e.waitResult()
	if out, err := e.svc.Resume(bg, id); err != nil || out != Skipped || len(e.runList()) != 2 {
		t.Fatalf("third call (answer already handled): %s %v runs=%d", out, err, len(e.runList()))
	}
	if ev := e.ofType(EvResumed); len(ev) != 1 || ev[0].Basis == nil || ev[0].Basis.RuleID != "bgr/event_rules/main" {
		t.Fatalf("events = %+v", ev)
	}
}

// ACT-RES-03：harness 没声明续接：新会话，上下文含问题、回答、resume_note 和白名单内的挂靠消息（名单外的不进）。
func TestNoResumeDeclarationStartsNewSessionWithContext(t *testing.T) {
	e := setup(t)
	e.harness(false, false)
	e.auto("main")
	id := e.asked("agent-ok", nil)
	e.setPolicy(func(p *policy.Policy) { p.ForwardAllowlist = map[string][]string{"mail": {"alice"}} })
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, m := range []struct{ id, sender string }{{"m1", "alice"}, {"m2", "bob"}} {
			ev := domain.SourceEvent{ID: m.id, Type: "mail", Subject: "s-" + m.id, Sender: m.sender, SenderVerified: true, OccurredAt: t0}
			if err := runner.AppendInternal(tx, domain.Source{ID: "mail"}, ev); err != nil {
				return err
			}
		}
		evs, err := runner.Pending(tx)
		for _, ev := range evs {
			if err == nil {
				err = runner.SetAttachState(tx, ev.ID, runner.AttachPending, id)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	out, err := e.svc.Resume(bg, id)
	if err != nil || out != Fresh {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	r := lastRun(e.runList()).Run
	dir := filepath.Join(e.runs, r.ID)
	if args := waitFile(t, filepath.Join(dir, "args")); strings.Contains(args, "--resume") {
		t.Fatalf("a new session must not resume: %q", args)
	}
	ctxFile := waitFile(t, filepath.Join(dir, "context.md"))
	for _, want := range []string{"付款走哪个账户", "乙", "已核对完发票", "s-m1"} {
		if !strings.Contains(ctxFile, want) {
			t.Fatalf("context lacks %q:\n%s", want, ctxFile)
		}
	}
	if strings.Contains(ctxFile, "s-m2") {
		t.Fatalf("a message outside the allowlist reached the new session:\n%s", ctxFile)
	}
	e.release(r.ID)
	e.waitResult()
}

// ACT-RES-04：续接命令报会话不存在（声明的退出码加输出片段）：改用新会话，事项继续而不是卡在 needs_input；只退一次，不循环。
func TestSessionGoneFallsBackToNewSession(t *testing.T) {
	e := setup(t)
	e.harness(true, true)
	e.auto("main")
	id := e.asked("agent-ok", nil)
	if err := os.WriteFile(filepath.Join(e.dir, "gone"), []byte("error: no such session sess-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := e.svc.Resume(bg, id); err != nil || out != Resumed {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	if res := e.waitResult(); res.Run.State != domain.RunFailed {
		t.Fatalf("the resume run = %+v", res.Run)
	}
	rs := e.runList()
	if len(rs) != 3 {
		t.Fatalf("runs = %d, want original + failed resume + fresh", len(rs))
	}
	fresh := lastRun(rs).Run
	if sp := lastRun(rs).Spec; sp.ContextName != "" || sp.RunDir != filepath.Join(e.runs, fresh.ID) {
		t.Fatalf("a new session has its own run context: %+v", sp)
	}
	if args := waitFile(t, filepath.Join(e.runs, fresh.ID, "args")); strings.Contains(args, "--resume") {
		t.Fatalf("fallback still resumed: %q", args)
	}
	if !strings.Contains(waitFile(t, filepath.Join(e.runs, fresh.ID, "prompt.seen")), "已核对完发票") {
		t.Fatal("the new session lacks the resume note")
	}
	if it := e.item(id); it.Phase == domain.PhaseNeedsInput {
		t.Fatalf("the item is stuck: %+v", it)
	}
	if len(e.ofType(EvSessionLost)) != 1 {
		t.Fatalf("session lost events = %d", len(e.ofType(EvSessionLost)))
	}
	e.release(fresh.ID)
	e.waitResult()
}

// 其他失败（同一退出码但输出不含声明的片段，如模型端点不可达）不当作会话失效、不重试：按后台运行的收尾转 needs_input。
func TestOtherResumeFailureIsNotRetried(t *testing.T) {
	e := setup(t)
	e.harness(true, true)
	e.auto("main")
	id := e.asked("agent-ok", nil)
	if err := os.WriteFile(filepath.Join(e.dir, "gone"), []byte("error: connection refused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Resume(bg, id); err != nil {
		t.Fatal(err)
	}
	e.waitResult()
	if n := len(e.runList()); n != 2 {
		t.Fatalf("runs = %d", n)
	}
	if it := e.item(id); it.Phase != domain.PhaseNeedsInput || !strings.Contains(it.Ask.Question, "failed") {
		t.Fatalf("item = %+v", it)
	}
	if n := len(e.ofType(EvSessionLost)); n != 0 {
		t.Fatalf("session lost events = %d", n)
	}
}

// ACT-RES-05：等待续接时 runner=task 的执行者被移出项目名单：不续接，事项转 needs_input，有拒绝记录。
func TestExecutorRemovedFromProjectIsRefused(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("taskr")
	proj := e.project("work-host.cli")
	child := e.asked("taskr", map[string]any{"project": proj})
	e.must(owner, "project.allowed_executors.set", map[string]any{"id": proj, "executors": []string{"other.cli"}})

	out, err := e.svc.Resume(bg, child)
	if err != nil || out != Refused {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	it := e.item(child)
	if it.Phase != domain.PhaseNeedsInput || !strings.Contains(it.Ask.Question, "work-host.cli") ||
		!slices.Equal([]string{it.Ask.Options[0].ID, it.Ask.Options[1].ID, it.Ask.Options[2].ID}, []string{"rerun", "reassign", "drop"}) {
		t.Fatalf("item = %+v", it)
	}
	if n := len(e.runList()); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if ev := e.ofType(EvRefused); len(ev) != 1 || ev[0].Object != child {
		t.Fatalf("refused = %+v", ev)
	}
}

// ACT-BGG-09：运行结束后规则切回 observe，再回答：复核不通过，不续接，事项转 needs_input。
func TestRuleSwitchedBackToObserveIsRefused(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("main")
	id := e.asked("agent-ok", nil)
	e.must(owner, "rule.set_mode", map[string]string{"rule": "bgr/event_rules/main", "mode": "observe"})

	out, err := e.svc.Resume(bg, id)
	if err != nil || out != Refused {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	if it := e.item(id); it.Phase != domain.PhaseNeedsInput || !strings.Contains(it.Ask.Question, "auto") {
		t.Fatalf("item = %+v", it)
	}
	if n := len(e.runList()); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if ev := e.ofType(EvRefused); len(ev) != 1 || !strings.Contains(ev[0].Reason, "auto") {
		t.Fatalf("refused = %+v", ev)
	}
}

// 档位上限被调低、声明被撤走同样按当前策略拒绝。
func TestPermissionCapLoweredIsRefused(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("main")
	id := e.asked("agent-ok", nil)
	e.setPolicy(func(p *policy.Policy) { p.PermissionCap.Default = policy.PermRead })
	if out, err := e.svc.Resume(bg, id); err != nil || out != Refused {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	if it := e.item(id); it.Phase != domain.PhaseNeedsInput || !strings.Contains(it.Ask.Question, "上限") {
		t.Fatalf("item = %+v", it)
	}
}

// 不是等待续接的已回答决策：没有后台运行、没有 resume_note、还没回答，都跳过且不改事项。
func TestNotResumableIsSkipped(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("main")
	plain := e.newItem(nil, nil)
	if out, err := e.svc.Resume(bg, plain); err != nil || out != Skipped {
		t.Fatalf("no background run: %s %v", out, err)
	}
	root := e.newItem([]string{"agent-ok"}, nil)
	e.must(owner, "item.transition", map[string]any{"id": root, "to": "open", "reason": "go"})
	r := e.runList()[0].Run
	run := runidentity.Identity{Secret: waitFile(t, filepath.Join(e.runs, r.ID, "token"))}
	e.must(run, "decision.request", map[string]any{"id": root, "ask": map[string]any{"question": "q", "options": []map[string]any{{"id": "A", "label": "a"}}}})
	e.release(r.ID)
	e.waitResult()
	if out, err := e.svc.Resume(bg, root); err != nil || out != Skipped {
		t.Fatalf("unanswered: %s %v", out, err)
	}
	e.must(owner, "decision.answer", map[string]any{"id": root, "choice": "A"})
	if out, err := e.svc.Resume(bg, root); err != nil || out != Skipped || len(e.runList()) != 1 {
		t.Fatalf("no resume_note: %s %v runs=%d", out, err, len(e.runList()))
	}
}

// 回答与续接 Run 同一事务：回答提交后的第一个消费者（并发的规则轮次看到的正是这个状态）就已经看到在途的续接 Run，
// 事件规则随后命中也不会另起普通后台运行；账本里有 resume.launched；同一回答不会再续接第二次。
func TestAnswerEnqueuesTheResumeRunInTheSameTransaction(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("main")
	id := e.pending("agent-ok", nil)
	e.svc.Attach(e.x)
	var inflight bool
	e.x.OnCommit(func(command.Commit) {
		if err := e.st.View(bg, func(tx *store.Tx) (err error) { inflight, err = proc.InFlight(tx, id); return }); err != nil {
			t.Error(err)
		}
	})
	e.must(owner, "decision.answer", map[string]any{"id": id, "choice": "B"})
	if !inflight {
		t.Fatal("the resume run was not visible right after the answer committed")
	}
	rs := e.runList()
	if len(rs) != 2 || lastRun(rs).Run.Target != id || lastRun(rs).Run.Basis.RuleID != "bgr/event_rules/main" || len(e.ofType(EvResumed)) != 1 {
		t.Fatalf("runs = %d, resumed events = %d", len(rs), len(e.ofType(EvResumed)))
	}
	// 并发的一轮规则（事项转回 open 命中 auto 规则）看到在途运行而跳过。
	e.must(owner, "item.transition", map[string]any{"id": id, "to": "waiting", "waiting_on": map[string]any{"kind": "external", "ref": "x"}, "reason": "park"})
	e.must(owner, "item.transition", map[string]any{"id": id, "to": "open", "reason": "back"})
	if err := e.eng.Drain(bg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.runList()); n != 2 {
		t.Fatalf("a rule round started another run: %d", n)
	}
	if out, err := e.svc.Resume(bg, id); err != nil || out != Skipped {
		t.Fatalf("the same answer resumed twice: %s %v", out, err)
	}
	e.release(lastRun(rs).Run.ID)
	e.waitResult()
}

// 复核不通过：同一事务里转 needs_input，回答提交后的第一个消费者就看到新问题；没有入队任何 Run。
func TestRefusedAnswerTurnsNeedsInputInTheSameTransaction(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("main")
	id := e.pending("agent-ok", nil)
	e.must(owner, "rule.set_mode", map[string]string{"rule": "bgr/event_rules/main", "mode": "observe"})
	e.svc.Attach(e.x)
	var phase domain.Phase
	e.x.OnCommit(func(command.Commit) { phase = e.item(id).Phase })
	e.must(owner, "decision.answer", map[string]any{"id": id, "choice": "B"})
	if it := e.item(id); phase != domain.PhaseNeedsInput || !strings.Contains(it.Ask.Question, "auto") || it.Ask.Resolution != nil {
		t.Fatalf("phase right after commit = %s, item = %+v", phase, it)
	}
	if n := len(e.runList()); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if ev := e.ofType(EvRefused); len(ev) != 1 || ev[0].Object != id {
		t.Fatalf("refused = %+v", ev)
	}
	// 本人能回答新问题（版本没有被回调打乱）。
	e.must(owner, "decision.answer", map[string]any{"id": id, "choice": "rerun"})
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted empty options")
	}
}

// 自由文本回答原样带进新会话；harness 声明被本人撤走（没有已确认的无头声明）时拒绝。
func TestFreeTextAnswerAndMissingDeclaration(t *testing.T) {
	e := setup(t)
	e.harness(true, false)
	e.auto("main")
	root := e.newItem([]string{"agent-ok"}, nil)
	e.must(owner, "item.transition", map[string]any{"id": root, "to": "open", "reason": "go"})
	r := e.runList()[0].Run
	run := runidentity.Identity{Secret: waitFile(t, filepath.Join(e.runs, r.ID, "token"))}
	e.must(run, "decision.request", map[string]any{"id": root, "ask": map[string]any{"question": "走哪个账户？", "free_text": true, "resume_note": "接着付款"}})
	e.release(r.ID)
	e.waitResult()
	e.must(owner, "decision.answer", map[string]any{"id": root, "text": "用备用账户"})
	e.setPolicy(func(p *policy.Policy) { p.DefaultHarness = "gone-harness" })
	if out, err := e.svc.Resume(bg, root); err != nil || out != Refused {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	if it := e.item(root); it.Phase != domain.PhaseNeedsInput || !strings.Contains(it.Ask.Question, "gone-harness") {
		t.Fatalf("item = %+v", it)
	}
	e.setPolicy(func(p *policy.Policy) { p.DefaultHarness = "" })
	e.must(owner, "decision.answer", map[string]any{"id": root, "choice": "rerun"})
	if out, err := e.svc.Resume(bg, root); err != nil || out != Skipped {
		t.Fatalf("outcome=%s err=%v", out, err)
	}
	it := e.item(root)
	it.Ask = &domain.Ask{AskInput: domain.AskInput{Question: "q"}, Resolution: &domain.Resolution{Text: "用备用账户"}}
	if got := answerText(it.Ask); got != "用备用账户" {
		t.Fatalf("answer = %q", got)
	}
}

// 不是后台运行、也不是续接运行的结果：Settle 不处理，交给调用方的后续收尾。
func TestSettleIgnoresForeignRuns(t *testing.T) {
	e := setup(t)
	if e.svc.Settle(bg, proc.Result{Run: domain.Run{ID: "run_x", Category: domain.RunSource}}) {
		t.Fatal("handled a source run")
	}
	if e.svc.Settle(bg, proc.Result{Run: domain.Run{ID: "run_unknown", Category: domain.RunBackground}}) {
		t.Fatal("handled a background run that is not a resume run")
	}
}

func TestSessionOf(t *testing.T) {
	for in, want := range map[string]string{"": "", "noise\n{\"session_id\":\"s9\"}\n": "s9", "{\"x\":1}": "", "not json": ""} {
		if got := sessionOf([]byte(in)); got != want {
			t.Fatalf("sessionOf(%q) = %q, want %q", in, got, want)
		}
	}
}
