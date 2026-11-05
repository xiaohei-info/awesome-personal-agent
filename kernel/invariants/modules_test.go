package invariants_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func open(t *testing.T, mods ...store.Module) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range mods {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func do(t *testing.T, x *command.Executor, id command.Identity, cmd string, in any) (command.Result, error) {
	t.Helper()
	b, _ := json.Marshal(in)
	return x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", WebURL: "http://web"})
}

func mustDo(t *testing.T, x *command.Executor, id command.Identity, cmd string, in any) json.RawMessage {
	t.Helper()
	r, err := do(t, x, id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func idOf(raw json.RawMessage) string {
	var o struct{ ID string }
	_ = json.Unmarshal(raw, &o)
	return o.ID
}

const loopRules = `name: loop
version: 1.0.0
description: loop
event_rules:
  - id: a
    when:
      tag: inv-loop
    do:
      - action: set_phase
        phase: waiting
  - id: b
    when:
      event: item.phase_changed
    do:
      - action: add_tag
        tag: inv-b
`

type noReminder struct{}

func (noReminder) Remind(*store.Tx, domain.Item, string, time.Time, string, string) error { return nil }

// 不变量 6：规则动作产生的 Event 不再触发规则。规则 A 把事项推到 waiting，规则 B 监听所有阶段变更：
// B 不能被 A 的 Event 触发；但本人改阶段照样触发 B（只排除规则自己的 Event）。
func TestInv6RuleActionEventsDoNotRetriggerRules(t *testing.T) {
	st := open(t, eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, instances.Module, rules.Module)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	x := command.New(st, func() time.Time { return now })
	kinds := items.NewKinds()
	for _, f := range []func(*command.Executor) error{projects.Register, rules.Register, func(x *command.Executor) error { return items.Register(x, kinds) }} {
		if err := f(x); err != nil {
			t.Fatal(err)
		}
	}
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "loop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "loop", "pack.yaml"), []byte(loopRules), 0o644); err != nil {
		t.Fatal(err)
	}
	ld := packs.New(packs.Options{Builtin: fstest.MapFS{}, DataDir: data, Kinds: kinds})
	if err := instances.Register(x, ld); err != nil {
		t.Fatal(err)
	}
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %v", err, rej)
	}
	var eng *rules.Engine
	var err error
	if eng, err = rules.New(rules.Options{Store: st, Exec: x, Packs: ld, Owner: "owner", Now: func() time.Time { return now }, Reminder: noReminder{},
		OnError: func(err error) { t.Errorf("auto consume: %v", err) }}); err != nil {
		t.Fatal(err)
	}
	if _, err := clock.New(st, func(tx *store.Tx, c clock.Command) error { return eng.HandleTimer(tx, c) }, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"loop/event_rules/a", "loop/event_rules/b"} {
		mustDo(t, x, owner, "rule.set_mode", map[string]string{"rule": r, "mode": rules.ModeAuto})
	}
	get := func(id string) (it domain.Item) {
		if err := st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
			t.Fatal(err)
		}
		return
	}
	id := idOf(mustDo(t, x, owner, "item.create", map[string]any{"title": "t", "kind": "task", "tags": []string{"inv-loop"}}))

	it := get(id)
	if it.Phase != domain.PhaseWaiting {
		t.Fatalf("rule A's own action did not run: phase=%s", it.Phase)
	}
	if slices.Contains(it.Tags, "inv-b") {
		t.Fatal("rule B was triggered by rule A's Event")
	}
	var ruleEvents int
	if err := st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, id)
		for _, en := range es {
			if en.Event.Actor.Via == domain.ViaRule {
				ruleEvents++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ruleEvents != 1 {
		t.Fatalf("exactly one rule-made Event expected (first level only), got %d", ruleEvents)
	}
	mustDo(t, x, owner, "item.transition", map[string]any{"id": id, "to": "open", "reason": "back"})
	if !slices.Contains(get(id).Tags, "inv-b") {
		t.Fatal("rule B must still react to the owner's phase change")
	}
}

// 不变量 7：同一事项同时只有一个运行。先到者占位，后到的（任何类别、并发也一样）被拒且不写入；终态后可再入队。
func TestInv7OneRunPerItem(t *testing.T) {
	st := open(t, proc.Module)
	enqueue := func(cat domain.RunCategory, target string) (domain.Run, error) {
		var r domain.Run
		err := st.Update(bg, func(tx *store.Tx) (err error) {
			r, err = proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: cat, Target: target, TimeoutSec: 30}, Spec: proc.Spec{Command: []string{"/bin/true"}}}, time.Now())
			return
		})
		return r, err
	}
	count := func() (n int) {
		_ = st.View(bg, func(tx *store.Tx) error { rs, err := proc.List(tx); n = len(rs); return err })
		return
	}
	first, err := enqueue(domain.RunTask, "item-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, cat := range []domain.RunCategory{domain.RunTask, domain.RunBackground} {
		if _, err := enqueue(cat, "item-a"); exit(err) != domain.ExitConflict {
			t.Fatalf("second %s run on the same item: want conflict, got %v", cat, err)
		}
	}
	if count() != 1 {
		t.Fatalf("rejected enqueue wrote a run: %d", count())
	}
	if _, err := enqueue(domain.RunTask, "item-b"); err != nil {
		t.Fatalf("a different item is independent: %v", err)
	}

	// 并发抢同一事项：恰好一个成功。
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := enqueue(domain.RunBackground, "item-c"); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d concurrent runs got in for one item", won)
	}

	// 终态后释放。
	if err := st.Update(bg, func(tx *store.Tx) error {
		return proc.Settle(tx, first.ID, domain.RunFailed, nil, "test", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueue(domain.RunBackground, "item-a"); err != nil {
		t.Fatalf("a finished run must free the item: %v", err)
	}
}

// 不变量 8：运行身份没有代答能力。运行凭据不能回答决策、接受或丢弃建议；各留一条拒绝 Event，事项不变。
func TestInv8RunIdentityCannotAnswer(t *testing.T) {
	st := open(t, eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, proc.Module, runidentity.Module)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	x := command.New(st, func() time.Time { return now })
	for _, f := range []func(*command.Executor) error{projects.Register, decisions.Register, func(x *command.Executor) error { return items.Register(x, items.NewKinds()) }} {
		if err := f(x); err != nil {
			t.Fatal(err)
		}
	}
	newItem := func(phase string) string {
		return idOf(mustDo(t, x, owner, "item.create", map[string]any{"title": "t", "kind": "task", "phase": phase}))
	}
	target, asked, proposed := newItem("open"), newItem("open"), newItem("proposed")
	mustDo(t, x, owner, "decision.request", map[string]any{"id": asked, "ask": ask(nil)})

	var secret string
	if err := st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: target, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, now)
		if err != nil {
			return err
		}
		secret, err = runidentity.Issue(tx, r.ID, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run := runidentity.Identity{Secret: secret}
	if _, err := do(t, x, run, "item.update", map[string]any{"id": target, "summary": "progress"}); err != nil {
		t.Fatalf("control: the run credential can write its own target: %v", err)
	}

	rejections := func() (n int) {
		_ = st.View(bg, func(tx *store.Tx) error {
			es, err := eventlog.Read(tx, 0, 10000)
			for _, en := range es {
				if en.Event.Type == domain.EvCommandRejected {
					n++
				}
			}
			return err
		})
		return
	}
	before := rejections()
	for cmd, in := range map[string]map[string]any{
		"decision.answer":  {"id": asked, "choice": "A"},
		"proposal.accept":  {"id": proposed},
		"proposal.dismiss": {"id": proposed, "reason": "x"},
	} {
		if _, err := do(t, x, run, cmd, in); exit(err) != domain.ExitPermissionDenied {
			t.Errorf("%s by a run credential: want exit 4, got %v", cmd, err)
		}
	}
	if got := rejections() - before; got != 3 {
		t.Errorf("want 3 rejection Events, got %d", got)
	}
	var a, p domain.Item
	if err := st.View(bg, func(tx *store.Tx) (err error) {
		if a, err = items.Get(tx, asked); err != nil {
			return err
		}
		p, err = items.Get(tx, proposed)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if a.Phase != domain.PhaseNeedsInput || a.Ask.Resolution != nil || p.Phase != domain.PhaseProposed {
		t.Fatalf("items changed: %+v / %+v", a, p)
	}
}

// 不变量 9：Revision 内容追加不改。多次提交只增不改；存储层拒绝改写已写入的内容、原因与基线（物理删除是 §6.15 的另一条路径，不属于改写）；回退也是追加。
func TestInv9RevisionsAreAppendOnly(t *testing.T) {
	st := open(t, eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, materials.Module)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	x := command.New(st, func() time.Time { now = now.Add(time.Minute); return now })
	if err := projects.Register(x); err != nil {
		t.Fatal(err)
	}
	if err := materials.Register(x); err != nil {
		t.Fatal(err)
	}
	prj := idOf(mustDo(t, x, owner, "project.create", map[string]any{"name": "inv"}))
	commit := func(in map[string]any) (r struct{ Material, Revision string }) {
		_ = json.Unmarshal(mustDo(t, x, worker, "material.commit", in), &r)
		return
	}
	r1 := commit(map[string]any{"project": prj, "kind": "minutes", "title": "周会", "content": "v1", "reason": "first"})
	r2 := commit(map[string]any{"id": r1.Material, "content": "v2", "reason": "second", "based_on": r1.Revision})
	r3 := commit(map[string]any{"id": r1.Material, "content": "v3", "reason": "third", "based_on": r2.Revision})
	mustDo(t, x, owner, "material.revert", map[string]any{"id": r1.Material, "to": r1.Revision, "reason": "back"})

	history := func() (h []materials.Revision) {
		if err := st.View(bg, func(tx *store.Tx) (err error) { h, err = materials.History(tx, r1.Material); return }); err != nil {
			t.Fatal(err)
		}
		return
	}
	h := history()
	if len(h) != 4 {
		t.Fatalf("revert must append a revision, history has %d", len(h))
	}
	for i, want := range []string{"v1", "v2", "v3", "v1"} {
		if h[i].Content != want {
			t.Fatalf("revision %d content %q, want %q", i, h[i].Content, want)
		}
	}
	for _, id := range []string{r1.Revision, r2.Revision, r3.Revision} {
		for _, sql := range []string{"UPDATE revisions SET content = 'x' WHERE id = ?", "UPDATE revisions SET reason = 'x' WHERE id = ?", "UPDATE revisions SET based_on = 'x' WHERE id = ?"} {
			if err := st.Update(bg, func(tx *store.Tx) error { _, err := tx.Exec(sql, id); return err }); err == nil {
				t.Errorf("storage accepted %q on a written revision", sql)
			}
		}
	}
	for i, want := range []string{"v1", "v2", "v3", "v1"} {
		if got := history()[i].Content; got != want {
			t.Fatalf("revision %d was rewritten to %q", i, got)
		}
	}
}
