package rules

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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/health"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg = context.Background()
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	delegate = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Session: "s1", OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
)

type env struct {
	t     *testing.T
	st    *store.Store
	x     *command.Executor
	clk   *clock.Clock
	loads *packs.Loader
	eng   *Engine
	data  string
	now   time.Time
	rem   *fakeReminder
}

// fakeReminder 记录经触达的提醒；触达本身的静默、合并与去重在 kernel/attention 测试。
type fakeReminder struct{ calls []string }

func (f *fakeReminder) Remind(_ *store.Tx, it domain.Item, kind string, due time.Time, _, _ string) error {
	f.calls = append(f.calls, kind+":"+it.ID+":"+due.Format(time.RFC3339))
	return nil
}

// newEnv 用真实的存储、命令管线、事项中心、时钟与能力包加载器；包放在数据目录里，便于改内容后热加载。
func newEnv(t *testing.T, files map[string]string, h map[string]Handler) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, instances.Module, delivery.Module, chain.Module, health.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: t0, data: t.TempDir(), rem: &fakeReminder{}}
	e.x = command.New(st, func() time.Time { return e.now })
	kinds := items.NewKinds()
	for _, f := range []func(*command.Executor) error{projects.Register, Register, func(x *command.Executor) error { return items.Register(x, kinds) }} {
		if err := f(e.x); err != nil {
			t.Fatal(err)
		}
	}
	if e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error {
		if instances.Owns(c) {
			return instances.HandleTimer(tx, c)
		}
		return e.eng.HandleTimer(tx, c)
	}, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	e.loads = packs.New(packs.Options{Builtin: fstest.MapFS{}, DataDir: e.data, Kinds: kinds})
	if err := instances.Register(e.x, e.loads); err != nil {
		t.Fatal(err)
	}
	e.pack(files)
	if e.eng, err = New(Options{Store: st, Exec: e.x, Packs: e.loads, Handlers: h, Owner: "owner", Now: func() time.Time { return e.now }, Reminder: e.rem,
		OnError: func(err error) { t.Errorf("auto consume: %v", err) }}); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	return e
}

// pack 写入包文件并热加载、对齐规则状态。
func (e *env) pack(files map[string]string) {
	e.t.Helper()
	for n, c := range files {
		p := filepath.Join(e.data, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			e.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	if rej, err := e.loads.Load(); err != nil || len(rej) != 0 {
		e.t.Fatalf("load: %v %v", err, rej)
	}
	if e.eng != nil {
		if err := e.eng.Reconcile(bg); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
}

func (e *env) must(id command.Identity, cmd string, in any) command.Result {
	e.t.Helper()
	r, err := e.do(id, cmd, in)
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func (e *env) newItem(in map[string]any) string {
	e.t.Helper()
	in["title"], in["kind"] = "t", "task"
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", in).Value, &out)
	return out.ID
}

func (e *env) open(id string) {
	e.t.Helper()
	e.must(owner, "item.transition", map[string]any{"id": id, "to": "open", "reason": "go"})
}

func (e *env) item(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) events() []domain.Event {
	var out []domain.Event
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 10000)
		for _, x := range es {
			out = append(out, x.Event)
		}
		return err
	})
	return out
}

func (e *env) ofType(typ string) []domain.Event {
	var out []domain.Event
	for _, ev := range e.events() {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func (e *env) mode(rule string) string {
	e.t.Helper()
	var all []State
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { all, err = List(tx); return })
	for _, s := range all {
		if s.Key == rule {
			return s.Mode
		}
	}
	e.t.Fatalf("rule %s not found in %v", rule, all)
	return ""
}

func (e *env) setMode(id command.Identity, rule, mode string) error {
	_, err := e.do(id, "rule.set_mode", map[string]string{"rule": rule, "mode": mode})
	return err
}

const tagRule = `name: acc
version: 1.0.0
description: rules
event_rules:
  - id: acc-tag
    mode: observe
    when:
      event: item.phase_changed
      to_phase: open
      tag: [acc-rule]
    do:
      - action: add_tag
        tag: acc-hit
`

const tagKey = "acc/event_rules/acc-tag"

func exit(err error) int {
	if de, ok := err.(*domain.Error); ok {
		return de.ExitCode()
	}
	return -1
}

// ACT-RULE-02：observe 只记"本来会做什么"，事项没有 acc-hit 标签，也没有任何规则写入。
func TestObserveRecordsWouldDoWithoutChangingState(t *testing.T) {
	e := newEnv(t, map[string]string{"acc/pack.yaml": tagRule}, nil)
	if got := e.mode(tagKey); got != ModeObserve {
		t.Fatalf("initial mode = %s", got)
	}
	id := e.newItem(map[string]any{"tags": []string{"acc-rule"}})
	e.open(id)

	if slices.Contains(e.item(id).Tags, "acc-hit") {
		t.Fatal("observe changed business state")
	}
	obs := e.ofType(EvObserved)
	if len(obs) != 1 || obs[0].Object != id || obs[0].Basis == nil || obs[0].Basis.RuleID != tagKey {
		t.Fatalf("observed records = %+v", obs)
	}
	if !strings.Contains(string(obs[0].Payload), "add_tag") {
		t.Fatalf("record does not say what would be done: %s", obs[0].Payload)
	}
	for _, ev := range e.events() {
		if ev.Actor.Via == domain.ViaRule && ev.Type != EvObserved {
			t.Fatalf("observe produced %s", ev.Type)
		}
	}
}

// ACT-RULE-03：只有 Web 本人能切模式；token 被拒绝且模式不变；Web 切换记 rule.mode_changed。
func TestSetModeIsWebOnly(t *testing.T) {
	e := newEnv(t, map[string]string{"acc/pack.yaml": tagRule}, nil)
	if err := e.setMode(delegate, tagKey, ModeAuto); exit(err) != domain.ExitWebOnly {
		t.Fatalf("delegate set auto: %v", err)
	}
	worker := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "w", Via: domain.ViaCLI}, Level: authz.LevelWorker}
	if err := e.setMode(worker, tagKey, ModeAuto); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker set auto: %v", err)
	}
	if e.mode(tagKey) != ModeObserve {
		t.Fatal("mode changed by non-Web caller")
	}
	if err := e.setMode(owner, tagKey, ModeAuto); err != nil {
		t.Fatal(err)
	}
	ch := e.ofType(domain.EvRuleModeChanged)
	if e.mode(tagKey) != ModeAuto || len(ch) != 1 || !strings.Contains(string(ch[0].Payload), `"to":"auto"`) {
		t.Fatalf("mode=%s events=%+v", e.mode(tagKey), ch)
	}
	if err := e.setMode(owner, tagKey, "bogus"); exit(err) != domain.ExitUsage {
		t.Fatalf("bogus mode: %v", err)
	}
	if err := e.setMode(owner, "acc/event_rules/nope", ModeOff); exit(err) != domain.ExitNotFound {
		t.Fatalf("unknown rule: %v", err)
	}
}

// ACT-RULE-04：auto 执行动作，Event 记策略版本与规则 id。
func TestAutoExecutesAndRecordsBasis(t *testing.T) {
	e := newEnv(t, map[string]string{"acc/pack.yaml": tagRule}, nil)
	if err := e.setMode(owner, tagKey, ModeAuto); err != nil {
		t.Fatal(err)
	}
	id := e.newItem(map[string]any{"tags": []string{"acc-rule"}})
	e.open(id)

	if !slices.Contains(e.item(id).Tags, "acc-hit") {
		t.Fatalf("tags = %v", e.item(id).Tags)
	}
	var act *domain.Event
	for _, ev := range e.ofType(domain.EvItemUpdated) {
		if ev.Actor.Via == domain.ViaRule {
			act = &ev
		}
	}
	if act == nil || act.Basis == nil || act.Basis.RuleID != tagKey || act.ChainDepth < 1 {
		t.Fatalf("rule event = %+v", act)
	}
	if len(e.ofType(EvObserved)) != 0 {
		t.Fatal("auto must not also write observe records")
	}
	// 不命中的事项（无 tag）不被打标签。
	other := e.newItem(map[string]any{})
	e.open(other)
	if slices.Contains(e.item(other).Tags, "acc-hit") {
		t.Fatal("rule matched an item without the tag")
	}
}

// ACT-RULE-05：off 时跳过，既不执行也不留观察记录。
func TestOffSkipsEntirely(t *testing.T) {
	e := newEnv(t, map[string]string{"acc/pack.yaml": tagRule}, nil)
	if err := e.setMode(owner, tagKey, ModeOff); err != nil {
		t.Fatal(err)
	}
	id := e.newItem(map[string]any{"tags": []string{"acc-rule"}})
	e.open(id)
	if slices.Contains(e.item(id).Tags, "acc-hit") || len(e.ofType(EvObserved)) != 0 {
		t.Fatal("off rule ran")
	}
	for _, ev := range e.events() {
		if ev.Actor.Via == domain.ViaRule {
			t.Fatalf("off rule wrote %s", ev.Type)
		}
	}
}

const loopRules = `name: loop
version: 1.0.0
description: loop
event_rules:
  - id: a
    when:
      tag: acc-loop
    do:
      - action: set_phase
        phase: waiting
  - id: b
    when:
      event: item.phase_changed
    do:
      - action: add_tag
        tag: acc-b
`

// ACT-RULE-06：规则动作产生的 Event 不再触发规则，只发生第一层动作。
func TestRuleActionEventsDoNotRetriggerRules(t *testing.T) {
	e := newEnv(t, map[string]string{"loop/pack.yaml": loopRules}, nil)
	for _, r := range []string{"loop/event_rules/a", "loop/event_rules/b"} {
		if err := e.setMode(owner, r, ModeAuto); err != nil {
			t.Fatal(err)
		}
	}
	id := e.newItem(map[string]any{"tags": []string{"acc-loop"}})

	it := e.item(id)
	if it.Phase != domain.PhaseWaiting {
		t.Fatalf("first-level action did not run: phase=%s", it.Phase)
	}
	if slices.Contains(it.Tags, "acc-b") {
		t.Fatal("rule B was triggered by rule A's event")
	}
	if n := len(e.events()); n > 6 {
		t.Fatalf("event count %d suggests a loop", n)
	}
	// 人来改阶段仍会触发 B：只排除规则自己的 Event。
	e.must(owner, "item.transition", map[string]any{"id": id, "to": "open", "reason": "back"})
	if !slices.Contains(e.item(id).Tags, "acc-b") {
		t.Fatal("rule B should react to the owner's phase change")
	}
}

// healthDeliveries 跑一遍健康告警的投递产出，返回当前全部健康类投递。
func (e *env) healthDeliveries() []domain.Delivery {
	e.t.Helper()
	q := delivery.New(delivery.Options{Now: func() time.Time { return e.now }})
	var out []domain.Delivery
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		if err = health.NewNotifier(q).Sync(tx); err == nil {
			out, err = q.Unread(tx)
		}
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// ACT-RULE-07：规则内容变化回到 observe，产出健康类事件与一条健康类投递；再次触发只记录。
func TestContentChangeFallsBackToObserve(t *testing.T) {
	e := newEnv(t, map[string]string{"acc/pack.yaml": tagRule}, nil)
	if err := e.setMode(owner, tagKey, ModeAuto); err != nil {
		t.Fatal(err)
	}
	e.pack(map[string]string{"acc/pack.yaml": strings.Replace(tagRule, "acc-hit", "acc-hit2", 1)})

	if e.mode(tagKey) != ModeObserve {
		t.Fatalf("mode after change = %s", e.mode(tagKey))
	}
	alerts := e.ofType(domain.EvHealthAlert)
	if len(alerts) != 1 || alerts[0].Basis == nil || alerts[0].Basis.RuleID != tagKey {
		t.Fatalf("health alerts = %+v", alerts)
	}
	if ds := e.healthDeliveries(); len(ds) != 1 || ds[0].Class != domain.DeliveryHealth || ds[0].Item != alerts[0].Object {
		t.Fatalf("health deliveries = %+v", ds)
	}
	id := e.newItem(map[string]any{"tags": []string{"acc-rule"}})
	e.open(id)
	if slices.Contains(e.item(id).Tags, "acc-hit2") || len(e.ofType(EvObserved)) != 1 {
		t.Fatal("changed rule must only observe")
	}
	// 同内容再加载不重复降级、不重复告警。
	if err := e.setMode(owner, tagKey, ModeAuto); err != nil {
		t.Fatal(err)
	}
	e.pack(nil)
	if e.mode(tagKey) != ModeAuto || len(e.ofType(domain.EvHealthAlert)) != 1 {
		t.Fatal("unchanged content must keep the reviewed mode")
	}
}

const execRule = `name: alw
version: 1.0.0
description: executors
event_rules:
  - id: spawn
    mode: observe
    when:
      event: item.phase_changed
      to_phase: open
    do:
      - action: create_item
        kind: task
        assignee: other-host.cli
`

// ACT-ALW-09：规则的 create_item 也过项目允许的执行者；不建事项，有拒绝事件。
func TestRuleCreateItemHonoursAllowedExecutors(t *testing.T) {
	e := newEnv(t, map[string]string{"alw/pack.yaml": execRule}, nil)
	var proj struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "project.create", map[string]any{"name": "P"}).Value, &proj)
	e.must(owner, "project.allowed_executors.set", map[string]any{"id": proj.ID, "executors": []string{"work-host.cli"}})
	if err := e.setMode(owner, "alw/event_rules/spawn", ModeAuto); err != nil {
		t.Fatal(err)
	}
	id := e.newItem(map[string]any{"project": proj.ID})
	e.open(id)

	if n := len(e.ofType(domain.EvItemCreated)); n != 1 {
		t.Fatalf("rule created an item despite the allowlist (%d created events)", n)
	}
	rej := e.ofType(domain.EvCommandRejected)
	if len(rej) != 1 || rej[0].Actor.Via != domain.ViaRule || rej[0].Basis == nil || rej[0].Basis.RuleID != "alw/event_rules/spawn" {
		t.Fatalf("rejections = %+v", rej)
	}

	// 名单允许时同一条规则建出提议阶段的事项，项目取自触发事项。
	e.must(owner, "project.allowed_executors.set", map[string]any{"id": proj.ID, "executors": []string{"other-host.cli"}})
	id2 := e.newItem(map[string]any{"project": proj.ID})
	e.open(id2)
	var created *domain.Item
	for _, ev := range e.ofType(domain.EvItemCreated) {
		var p struct{ Item domain.Item }
		_ = json.Unmarshal(ev.Payload, &p)
		if ev.Actor.Via == domain.ViaRule {
			created = &p.Item
		}
	}
	if created == nil || created.Project != proj.ID || created.Assignee != "other-host.cli" || created.Phase != domain.PhaseProposed {
		t.Fatalf("rule-created item = %+v", created)
	}
}

const bgRule = `name: bg
version: 1.0.0
description: background
event_rules:
  - id: run
    mode: observe
    when:
      event: item.phase_changed
      to_phase: open
    do:
      - action: start_background_run
        runner: main_session
        prompt: handle
        access: edit
        cwd: run_dir
`

// start_background_run 未注册处理器时按未知动作失败：记健康类事件，不伪造成功；注册后用规则身份走命令模型。
func TestUnregisteredActionFailsLoudlyAndRegisteredOneRuns(t *testing.T) {
	files := map[string]string{"bg/pack.yaml": bgRule, "bg/prompts/handle.md": "---\nrun_as: main_session\ntimeout: 30m\n---\n处理 {item_id}\n"}
	e := newEnv(t, files, nil)
	if err := e.setMode(owner, "bg/event_rules/run", ModeAuto); err != nil {
		t.Fatal(err)
	}
	id := e.newItem(map[string]any{})
	e.open(id)
	al := e.ofType(domain.EvHealthAlert)
	if len(al) != 1 || !strings.Contains(al[0].Reason, "start_background_run") {
		t.Fatalf("alerts = %+v", al)
	}
	if ds := e.healthDeliveries(); len(ds) != 1 {
		t.Fatalf("health deliveries = %+v", ds)
	}

	var got Call
	e2 := newEnv(t, files, map[string]Handler{"start_background_run": func(_ context.Context, c Call) error {
		got = c
		_, err := c.Do("item.update", map[string]any{"id": c.Item.ID, "tags": []string{"bg-ran"}, "reason": "bg"})
		return err
	}})
	if err := e2.setMode(owner, "bg/event_rules/run", ModeAuto); err != nil {
		t.Fatal(err)
	}
	id2 := e2.newItem(map[string]any{})
	e2.open(id2)
	if got.Rule != "bg/event_rules/run" || str(got.Action, "runner") != "main_session" || !slices.Contains(e2.item(id2).Tags, "bg-ran") {
		t.Fatalf("handler call = %+v tags=%v", got, e2.item(id2).Tags)
	}
	if len(e2.ofType(domain.EvHealthAlert)) != 0 {
		t.Fatal("successful handler must not raise alerts")
	}
}

const clockRule = `name: tick
version: 1.0.0
description: clock
clock_rules:
  - id: daily
    type: recur
    schedule: 24h
    mode: observe
    do:
      - action: create_item
        kind: task
`

// 时钟规则：到期后按模式运行；observe 只记，auto 经命令模型建事项。
func TestClockRuleFiresByMode(t *testing.T) {
	e := newEnv(t, map[string]string{"tick/pack.yaml": clockRule}, nil)
	key := "tick/clock_rules/daily"
	tick := func() {
		t.Helper()
		e.now = e.now.Add(24 * time.Hour)
		if _, err := e.clk.Tick(bg); err != nil {
			t.Fatal(err)
		}
		if err := e.eng.Drain(bg); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if len(e.ofType(EvObserved)) != 1 || len(e.ofType(domain.EvItemCreated)) != 0 {
		t.Fatalf("observe tick: observed=%d created=%d", len(e.ofType(EvObserved)), len(e.ofType(domain.EvItemCreated)))
	}
	if err := e.setMode(owner, key, ModeAuto); err != nil {
		t.Fatal(err)
	}
	tick()
	cr := e.ofType(domain.EvItemCreated)
	if len(cr) != 1 || cr[0].Actor.Via != domain.ViaRule || cr[0].Basis == nil || cr[0].Basis.RuleID != key {
		t.Fatalf("created = %+v", cr)
	}
	if err := e.setMode(owner, key, ModeOff); err != nil {
		t.Fatal(err)
	}
	tick()
	if len(e.ofType(domain.EvItemCreated)) != 1 || len(e.ofType(EvObserved)) != 1 {
		t.Fatal("off clock rule ran")
	}
}

// cron 表达式的时钟规则落成周期 Timer（不再报"不支持"）。
func TestCronScheduleCreatesTimer(t *testing.T) {
	cron := strings.Replace(clockRule, "schedule: 24h", `schedule: "CRON_TZ=UTC 0 8 * * 1-5"`, 1)
	e := newEnv(t, map[string]string{"tick/pack.yaml": cron}, nil)
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	var rec string
	err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT recurrence FROM clock_timer`)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			return r.Scan(&rec)
		}
		return nil
	})
	if err != nil || rec != "CRON_TZ=UTC 0 8 * * 1-5" {
		t.Fatalf("timer recurrence %q err %v", rec, err)
	}
}

// 历史事件不回放：引擎首次接管账本时，已有事件不会让 auto 规则补做动作。
func TestHistoryIsNotReplayed(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := e.newItem(map[string]any{"tags": []string{"acc-rule"}})
	e.open(id)
	e.pack(map[string]string{"acc/pack.yaml": strings.Replace(tagRule, "observe", "off", 1)})
	if err := e.setMode(owner, tagKey, ModeAuto); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Drain(bg); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.item(id).Tags, "acc-hit") {
		t.Fatal("history replayed")
	}
}

// 条件只支持相等与成员：不认识的条件键让规则失败可见，而不是静默不命中。
func TestUnknownConditionRaisesHealthAlert(t *testing.T) {
	bad := strings.Replace(tagRule, "tag: [acc-rule]", "title_contains: x", 1)
	e := newEnv(t, map[string]string{"acc/pack.yaml": bad}, nil)
	id := e.newItem(map[string]any{})
	e.open(id)
	if al := e.ofType(domain.EvHealthAlert); len(al) == 0 || !strings.Contains(al[0].Reason, "title_contains") {
		t.Fatalf("alerts = %+v", al)
	}
}

// create_timer 经命令模型写 Timer；set_phase waiting 自带 waiting_on。
func TestTimerAndWaitingActions(t *testing.T) {
	src := `name: tm
version: 1.0.0
description: timers
event_rules:
  - id: r
    when:
      event: item.phase_changed
      to_phase: open
    do:
      - action: create_timer
        after: 48h
      - action: set_phase
        phase: waiting
`
	e := newEnv(t, map[string]string{"tm/pack.yaml": src}, nil)
	if err := e.setMode(owner, "tm/event_rules/r", ModeAuto); err != nil {
		t.Fatal(err)
	}
	id := e.newItem(map[string]any{})
	e.open(id)
	if len(e.ofType(EvTimerCreated)) != 1 {
		t.Fatalf("timer events = %+v", e.ofType(EvTimerCreated))
	}
	if it := e.item(id); it.Phase != domain.PhaseWaiting || it.WaitingOn == nil {
		t.Fatalf("item = %+v", it)
	}
	if len(e.ofType(domain.EvHealthAlert)) != 0 {
		t.Fatalf("alerts: %+v", e.ofType(domain.EvHealthAlert))
	}

	// 到期：规则模块接手，经触达对触发事项提醒一次，时钟不报错，一次性 Timer 随之清掉。
	e.now = t0.Add(49 * time.Hour)
	if n, err := e.clk.Tick(bg); err != nil || n != 1 {
		t.Fatalf("tick = %d, %v", n, err)
	}
	if want := "rule:" + id + ":" + t0.Add(48*time.Hour).Format(time.RFC3339); len(e.rem.calls) != 1 || e.rem.calls[0] != want {
		t.Fatalf("reminders = %v, want [%s]", e.rem.calls, want)
	}
	e.now = e.now.Add(time.Hour)
	if n, err := e.clk.Tick(bg); err != nil || n != 0 || len(e.rem.calls) != 1 {
		t.Fatalf("timer must fire once: tick = %d, %v, reminders %v", n, err, e.rem.calls)
	}
}

// create_timer 的周期 Timer：事项结束后到期即撤销，不再提醒；proposed 事项不提醒。
func TestRuleTimerStopsWhenItemEnds(t *testing.T) {
	e := newEnv(t, nil, nil)
	id := e.newItem(map[string]any{})
	mk := func() {
		e.t.Helper()
		e.must(delegate, "rule.create_timer", map[string]any{"target": id, "kind": "recur", "at": e.now.Add(time.Hour), "every": "1h"})
	}
	mk()
	e.now = e.now.Add(90 * time.Minute)
	if _, err := e.clk.Tick(bg); err != nil || len(e.rem.calls) != 0 {
		t.Fatalf("proposed item must not be reminded: %v %v", err, e.rem.calls)
	}
	e.open(id)
	e.now = e.now.Add(time.Hour)
	if _, err := e.clk.Tick(bg); err != nil || len(e.rem.calls) != 1 {
		t.Fatalf("open item reminded once per period: %v %v", err, e.rem.calls)
	}
	e.must(owner, "item.transition", map[string]any{"id": id, "to": "dropped", "reason": "done with it"})
	e.now = e.now.Add(time.Hour)
	if _, err := e.clk.Tick(bg); err != nil || len(e.rem.calls) != 1 {
		t.Fatalf("ended item: %v %v", err, e.rem.calls)
	}
	e.now = e.now.Add(time.Hour)
	if n, err := e.clk.Tick(bg); err != nil || n != 0 {
		t.Fatalf("timer of an ended item must be cancelled: %d %v", n, err)
	}
	if _, err := e.do(delegate, "rule.create_timer", map[string]any{"target": "itm_nope", "kind": "remind", "at": e.now}); exit(err) != 5 {
		t.Fatalf("timer on a missing item: %v", err)
	}
}

// 条件只有相等与成员判断。
func TestMatchEqualityAndMembership(t *testing.T) {
	it := &domain.Item{ID: "i", Phase: domain.PhaseOpen, Kind: "task", Project: "p1", Assignee: "a", Tags: []string{"x", "y"},
		Origin: &domain.Origin{Source: "mail"}}
	tr := &trigger{entry: eventlog.Entry{Event: domain.Event{Type: domain.EvItemPhaseChanged}}, item: it, before: &domain.Item{Phase: domain.PhaseProposed}}
	for _, c := range []struct {
		when map[string]any
		want bool
	}{
		{map[string]any{"event": "item.phase_changed"}, true},
		{map[string]any{"event": []any{"item.created", "item.phase_changed"}}, true},
		{map[string]any{"event": "item.created"}, false},
		{map[string]any{"kind": []any{"task"}, "project": "p1", "assignee": "a", "source": "mail"}, true},
		{map[string]any{"project": "p2"}, false},
		{map[string]any{"from_phase": "proposed", "to_phase": "open", "phase": "open"}, true},
		{map[string]any{"from_phase": "waiting"}, false},
		{map[string]any{"tag": "y"}, true},
		{map[string]any{"tag_any": []any{"z", "x"}}, true},
		{map[string]any{"tag_any": []any{"z"}}, false},
		{map[string]any{}, true},
	} {
		got, err := match(c.when, tr)
		if err != nil || got != c.want {
			t.Errorf("match(%v) = %v, %v; want %v", c.when, got, err, c.want)
		}
	}
	if _, err := match(map[string]any{"nope": 1}, tr); err == nil {
		t.Error("unknown condition must be an error")
	}
	if ok, _ := match(map[string]any{"kind": "task"}, &trigger{}); ok {
		t.Error("item conditions cannot match an event without an item")
	}
	if ok, _ := match(map[string]any{"from_phase": "open"}, &trigger{item: it}); ok {
		t.Error("from_phase cannot match without a previous state")
	}
}

// create_item 的 parent=trigger 把新事项挂在触发事项下；缺 kind 等错误可见。
func TestCreateItemUnderTriggerAndTimerErrors(t *testing.T) {
	src := `name: ch
version: 1.0.0
description: child
event_rules:
  - id: child
    when:
      event: item.phase_changed
      to_phase: open
    do:
      - action: create_item
        kind: task
        parent: trigger
  - id: badtimer
    when:
      event: item.phase_changed
      to_phase: open
    do:
      - action: create_timer
`
	e := newEnv(t, map[string]string{"ch/pack.yaml": src}, nil)
	for _, r := range []string{"ch/event_rules/child", "ch/event_rules/badtimer"} {
		if err := e.setMode(owner, r, ModeAuto); err != nil {
			t.Fatal(err)
		}
	}
	id := e.newItem(map[string]any{})
	e.open(id)
	var childID string
	for _, ev := range e.ofType(domain.EvItemCreated) {
		if ev.Actor.Via == domain.ViaRule {
			childID = ev.Object
		}
	}
	if childID == "" {
		t.Fatal("rule did not create the child item")
	}
	rel := false
	for _, ev := range e.events() {
		rel = rel || (ev.Object == childID && strings.Contains(string(ev.Payload), `"parent"`) && ev.Actor.Via == domain.ViaRule && ev.Type != domain.EvItemCreated)
	}
	if !rel {
		t.Fatal("child was not related to the triggering item")
	}
	if al := e.ofType(domain.EvHealthAlert); len(al) != 1 || !strings.Contains(al[0].Reason, "create_timer") {
		t.Fatalf("alerts = %+v", al)
	}
}

// 实例到期（附录 C.10、C.13）：instance.fired 由规则执行接手，按包的实例驱动时钟规则建 open 事项，
// 后续动作以它为对象；不看模式。同一周期只一份（重处理同一事件不重复建），错过多个周期只补最近一次。
func TestInstanceFiringRunsInstanceRulesOncePerPeriod(t *testing.T) {
	files := map[string]string{"bf/pack.yaml": `name: bf
version: 1.0.0
description: brief
instance_params:
  type: object
  required: [schedule]
  properties:
    schedule: {type: string}
clock_rules:
  - id: scheduled
    type: recur
    schedule: "{instance.schedule}"
    do:
      - action: create_item
        kind: brief
      - action: call_agent
        prompt: write
`, "bf/prompts/write.md": "---\nagent: task\n---\n写简报 {item_id}。\n"}
	var calls []Call
	e := newEnv(t, files, map[string]Handler{"call_agent": func(_ context.Context, c Call) error { calls = append(calls, c); return nil }})
	var ins struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "instance.create", map[string]any{"pack": "bf", "params": map[string]any{"schedule": "24h"}}).Value, &ins)
	briefs := func() []domain.Item {
		var out []domain.Item
		_ = e.st.View(bg, func(tx *store.Tx) (err error) { out, err = items.List(tx, items.Filter{Kind: "brief"}); return })
		return out
	}
	fire := func(after time.Duration) {
		e.t.Helper()
		e.now = e.now.Add(after)
		if _, err := e.clk.Tick(bg); err != nil {
			e.t.Fatal(err)
		}
		if err := e.eng.Drain(bg); err != nil {
			e.t.Fatal(err)
		}
	}
	fire(24*time.Hour + time.Minute)
	bs := briefs()
	if len(calls) != 1 || len(bs) != 1 || !calls[0].Bound || calls[0].Item.ID != bs[0].ID || bs[0].Phase != domain.PhaseOpen ||
		calls[0].Instance == nil || calls[0].Instance.ID != ins.ID || !calls[0].Instance.Due.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("calls %+v briefs %+v", calls, bs)
	}
	fire(72 * time.Hour) // 错过两个周期：只补最近一次
	if len(calls) != 2 || len(briefs()) != 2 {
		t.Fatalf("missed periods: calls %d briefs %d", len(calls), len(briefs()))
	}
	// 重处理同一批事件（如崩溃后游标没推进）：事项按幂等键返回首次结果，不多建
	if err := e.st.Update(bg, func(tx *store.Tx) error { _, err := tx.Exec(`UPDATE rules_cursor SET seq = 0`); return err }); err != nil {
		t.Fatal(err)
	}
	fire(0)
	if n := len(briefs()); n != 2 || calls[len(calls)-1].Item.ID != calls[1].Item.ID {
		t.Fatalf("reprocessing duplicated the period: briefs %d", n)
	}
	if al := e.ofType(domain.EvHealthAlert); len(al) != 0 {
		t.Fatalf("alerts: %+v", al)
	}
}

func TestHandleTimerRejectsForeignTimers(t *testing.T) {
	e := newEnv(t, nil, nil)
	err := e.st.Update(bg, func(tx *store.Tx) error {
		return e.eng.HandleTimer(tx, clock.Command{TimerID: "due_x", Kind: domain.TimerDue, Target: "x"})
	})
	if err == nil {
		t.Fatal("foreign timer accepted")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing dependencies accepted")
	}
}
