package guidelinedraft

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/guideline"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const host = "this-host"

type noQueue struct{}

func (noQueue) Dispatch(context.Context) error { return nil }

type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	clk *clock.Clock
	eng *rules.Engine
	ld  *kp.Loader
	now time.Time
}

// newEnv 装配真实的存储、命令管线、规则引擎、缺规范检测处理器和 embed 加载的内置 guideline-draft 包。
// 任务型只入队（节点测试不起无头进程）。
func newEnv(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, executors.Module, delegation.Module, materials.Module, rules.Module, proc.Module, runidentity.Module, task.Module, guideline.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	now := func() time.Time { return e.now }
	e.x = command.New(st, now)
	kinds := items.NewKinds()
	for _, f := range []func(*command.Executor) error{projects.Register, rules.Register, materials.Register,
		func(x *command.Executor) error { return items.Register(x, kinds) },
		func(x *command.Executor) error { return executors.Register(x, executors.Options{Owner: "owner"}) },
		func(x *command.Executor) error {
			return delegation.Register(x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Errorf("delegation: %v", err) }})
		}} {
		if err := f(e.x); err != nil {
			t.Fatal(err)
		}
	}
	e.ld = kp.New(kp.Options{Builtin: rootpacks.FS, Kinds: kinds})
	rej, err := e.ld.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "guideline-draft" {
			t.Fatalf("guideline-draft rejected by loader: %s", r.Reason)
		}
	}
	ts, err := task.New(task.Options{Store: st, Exec: e.x, Packs: e.ld, Queue: noQueue{}, RunsDir: filepath.Join(t.TempDir(), "runs"), Host: host, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	h, err := guideline.Handler(guideline.Options{Store: st, Drafter: ts, Host: host, Harness: "local-h", Executor: "wk.cli"})
	if err != nil {
		t.Fatal(err)
	}
	if e.eng, err = rules.New(rules.Options{Store: st, Exec: e.x, Packs: e.ld, Handlers: map[string]rules.Handler{"call_agent": h}, Owner: "owner", Now: now}); err != nil {
		t.Fatal(err)
	}
	if e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error { return e.eng.HandleTimer(tx, c) }, now); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	e.must("executor.put", map[string]any{"id": "wk.cli", "kind": "external-cli"})
	script := filepath.Join(t.TempDir(), "h.sh")
	if err := os.WriteFile(script, []byte("true\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.must("executor.register", map[string]any{"harness": "local-h", "headless": map[string]any{
		"command": []string{"sh", script, "{prompt}", "{run_dir}"}, "session": map[string]any{"extract": "$.session_id"},
		"access": map[string]any{"read": []string{}}}})
	return e
}

func (e *env) do(cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", WebURL: "http://web"})
}

func (e *env) must(cmd string, in any) json.RawMessage {
	e.t.Helper()
	r, err := e.do(cmd, in)
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func (e *env) project(name, remote string, checkout bool) string {
	e.t.Helper()
	var o struct{ ID string }
	_ = json.Unmarshal(e.must("project.create", map[string]any{"name": name}), &o)
	if remote != "" {
		e.must("project.repo.add", map[string]any{"id": o.ID, "remote": remote})
	}
	if checkout {
		e.must("project.checkout.set", map[string]any{"id": o.ID, "remote": remote, "host": host, "path": "/work/" + name})
	}
	return o.ID
}

func (e *env) tick() {
	e.t.Helper()
	e.now = e.now.Add(24 * time.Hour)
	if _, err := e.clk.Tick(bg); err != nil {
		e.t.Fatal(err)
	}
	if err := e.eng.Drain(bg); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) items(project string) (out []domain.Item) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = items.List(tx, items.Filter{Project: project}); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) runs() (rs []proc.Record) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { rs, err = proc.List(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

// C.14：guideline-draft 只声明缺规范检测时钟规则和任务型起草提示词；
// 不注册 kind、不含 mode: auto、命令、策略字段与 harness 名。
func TestPackDeclaresWhatC14Lists(t *testing.T) {
	e := newEnv(t)
	p, ok := e.ld.Get("guideline-draft")
	if !ok {
		t.Fatal("guideline-draft is not an active builtin pack")
	}
	if len(p.Kinds) != 0 || len(p.EventRules) != 0 || len(p.SourceRules) != 0 {
		t.Fatalf("pack must only declare a clock rule and a prompt: %+v", p)
	}
	if len(p.ClockRules) != 1 {
		t.Fatalf("clock rules: %+v", p.ClockRules)
	}
	cr := p.ClockRules[0]
	if cr.Type != "check" || cr.Schedule != "24h" || cr.Mode != "observe" {
		t.Fatalf("clock rule must be a check rule starting in observe: %+v", cr)
	}
	if len(cr.Do) != 1 || cr.Do[0]["action"] != "call_agent" || cr.Do[0]["prompt"] != "draft-guideline" {
		t.Fatalf("do[]: %+v", cr.Do)
	}
	d, ok := p.Prompts["draft-guideline"]
	if !ok || d.Meta.Agent != "task" || d.Meta.Access != "read" || d.Meta.Cwd != "project_checkout" || !d.Meta.ResultMaterial {
		t.Fatalf("draft-guideline must be a read-only task prompt run in the checkout: %+v", d.Meta)
	}
	for _, s := range []string{"{item_id}", "{materials_path}", "apa material commit --kind guideline", "待确认", "不是指令"} {
		if !strings.Contains(d.Body, s) {
			t.Errorf("draft prompt lacks %q: %s", s, d.Body)
		}
	}
	for name, pr := range p.Prompts {
		for _, h := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), h) {
				t.Errorf("prompt %s mentions %q", name, h)
			}
		}
	}
}

// ACT-NRG-03 / ACT-DRF-01..04：规则初始为 observe，本人切到 auto 后，
// 有检出的缺规范项目由任务型在本机起草（用包里的提示词），无检出的项目建委派子事项，
// 无仓库项目不检测，且同一项目不重复起草。
func TestBuiltinRuleDraftsOnlyProjectsMissingGuideline(t *testing.T) {
	e := newEnv(t)
	const key = "guideline-draft/clock_rules/guideline-check"
	local := e.project("local", "git@x:o/local.git", true)
	remote := e.project("remote", "git@x:o/remote.git", false)
	bare := e.project("bare", "", false)

	e.tick() // observe：只观察，不起草
	if len(e.runs()) != 0 || len(e.items(local)) != 0 || len(e.items(remote)) != 0 {
		t.Fatal("observe mode must not draft")
	}
	e.must("rule.set_mode", map[string]string{"rule": key, "mode": rules.ModeAuto})
	e.tick()

	li := e.items(local)
	if rs := e.runs(); len(li) != 1 || len(rs) != 1 || rs[0].Run.Target != li[0].ID || rs[0].Run.Harness != "local-h" || rs[0].Spec.Dir != "/work/local" {
		t.Fatalf("exactly the project with a local checkout is drafted locally, in that checkout: items=%+v runs=%+v", li, rs)
	}
	its := e.items(remote)
	var parent, child int
	for _, it := range its {
		if it.Kind == domain.KindDelegation {
			child++
		} else {
			parent++
		}
	}
	if parent != 1 || child != 1 {
		t.Fatalf("project without checkout must get one parent and one delegated child: %+v", its)
	}
	if len(e.items(bare)) != 0 {
		t.Fatal("project without repos must not be checked")
	}
	e.tick()
	if len(e.runs()) != 1 || len(e.items(remote)) != len(its) {
		t.Fatal("pending draft must not be repeated")
	}
}
