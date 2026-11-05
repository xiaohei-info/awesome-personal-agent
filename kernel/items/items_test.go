package items

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func human(p string, lvl authz.Level, via domain.Via) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: p, Via: via}, Level: lvl}
}

func agent(p string, lvl authz.Level) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: p, Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: lvl}
}

var (
	owner    = human("owner", authz.LevelWeb, domain.ViaWeb)
	worker   = agent("wrk", authz.LevelWorker)
	delegate = agent("dlg", authz.LevelDelegate)
	observer = agent("obs", authz.LevelObserver)
)

type env struct {
	st    *store.Store
	x     *command.Executor
	clk   *clock.Clock
	now   time.Time
	kinds *Kinds
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, now: t0, kinds: NewKinds()}
	e.x = command.New(st, func() time.Time { return e.now })
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := Register(e.x, e.kinds); err != nil {
		t.Fatal(err)
	}
	if e.clk, err = clock.New(st, HandleTimer, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	return e
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

func (e *env) create(t *testing.T, id command.Identity, in map[string]any) string {
	t.Helper()
	if in["title"] == nil {
		in["title"] = "t"
	}
	if in["kind"] == nil {
		in["kind"] = "task"
	}
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, id, "item.create", in).Value, &out)
	if out.ID == "" {
		t.Fatal("create returned no id")
	}
	return out.ID
}

func (e *env) project(t *testing.T, executors any) string {
	t.Helper()
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": "P"}).Value, &out)
	if executors != nil {
		e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": out.ID, "executors": executors})
	}
	return out.ID
}

func (e *env) get(t *testing.T, id string) domain.Item {
	t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) events(t *testing.T) []domain.Event {
	t.Helper()
	var out []domain.Event
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			out = append(out, x.Event)
		}
		return err
	})
	return out
}

func (e *env) verify(t *testing.T) {
	t.Helper()
	if err := e.st.View(bg, Verify); err != nil {
		t.Fatalf("projection diverges from events: %v", err)
	}
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func rejections(es []domain.Event) (n int) {
	for _, x := range es {
		if x.Type == domain.EvCommandRejected {
			n++
		}
	}
	return
}

// ACT-LED-01：命令写出的投影与事件账本一致，坏掉的投影可由事件重建。
func TestProjectionFoldsFromEvents(t *testing.T) {
	e := setup(t)
	a := e.create(t, owner, map[string]any{"phase": "open", "title": "A", "tags": []string{"x"}})
	b := e.create(t, owner, map[string]any{"phase": "open", "title": "B"})
	e.must(t, owner, "item.update", map[string]any{"id": a, "title": "A2", "summary": "s", "tags": nil})
	r := e.must(t, owner, "item.transition", map[string]any{"id": a, "to": "done", "reason": "shipped"})
	e.must(t, owner, "item.relate", map[string]any{"from": a, "to": b, "type": domain.RelBlocks})
	if r.Version != 3 || e.get(t, a).Version != 4 {
		t.Fatalf("version must follow the command pipeline: transition=%d, after relate=%d", r.Version, e.get(t, a).Version)
	}
	if it := e.get(t, a); it.Title != "A2" || it.Phase != domain.PhaseDone || it.Tags != nil || it.Priority != domain.PriorityNormal {
		t.Fatalf("item: %+v", it)
	}
	e.verify(t)

	// 投影被破坏（丢行、改脏）后，Verify 报错，Rebuild 从事件折叠恢复。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM items WHERE id = ?`, b); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM item_relations`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if e.st.View(bg, Verify) == nil {
		t.Fatal("Verify must detect a damaged projection")
	}
	if err := e.st.Update(bg, Rebuild); err != nil {
		t.Fatal(err)
	}
	e.verify(t)
	if e.get(t, b).Title != "B" {
		t.Fatal("rebuilt item missing")
	}
	var rels []domain.Relation
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { rels, err = Relations(tx, a); return })
	if len(rels) != 1 || rels[0].Type != domain.RelBlocks || rels[0].To != b {
		t.Fatalf("relations after rebuild: %+v", rels)
	}
}

// §6.4 / §6.14 第 3 条：六阶段进入约束。
func TestPhaseEntryConstraints(t *testing.T) {
	e := setup(t)
	task := e.create(t, owner, map[string]any{"phase": "open"})
	dlg := e.create(t, owner, map[string]any{"phase": "open", "kind": "delegation"})

	tr := func(id string, in map[string]any) error {
		in["id"] = id
		_, err := e.do(owner, "item.transition", in)
		return err
	}
	for name, c := range map[string]struct {
		id string
		in map[string]any
	}{
		"waiting without waiting_on":    {task, map[string]any{"to": "waiting", "reason": "r"}},
		"needs_input without ask":       {task, map[string]any{"to": "needs_input", "reason": "r"}},
		"delegation done w/o evidence":  {dlg, map[string]any{"to": "done", "reason": "r"}},
		"dropped without reason":        {task, map[string]any{"to": "dropped"}},
		"unknown phase":                 {task, map[string]any{"to": "later", "reason": "r"}},
		"same phase":                    {task, map[string]any{"to": "open", "reason": "r"}},
		"bad waiting_on kind":           {task, map[string]any{"to": "waiting", "reason": "r", "waiting_on": map[string]any{"kind": "x", "ref": "y"}}},
		"time wait with non-time ref":   {task, map[string]any{"to": "waiting", "reason": "r", "waiting_on": map[string]any{"kind": "time", "ref": "tomorrow"}}},
		"blank evidence is no evidence": {dlg, map[string]any{"to": "done", "reason": "r", "evidence": "  "}},
	} {
		if err := tr(c.id, c.in); exit(err) != domain.ExitUsage {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
	}
	if it := e.get(t, task); it.Phase != domain.PhaseOpen {
		t.Fatalf("rejected transitions must not change the phase: %s", it.Phase)
	}

	if err := tr(task, map[string]any{"to": "waiting", "reason": "r", "waiting_on": map[string]any{"kind": "human", "ref": "bob"}}); err != nil {
		t.Fatal(err)
	}
	if it := e.get(t, task); it.Phase != domain.PhaseWaiting || it.WaitingOn == nil || it.WaitingOn.Ref != "bob" {
		t.Fatalf("waiting: %+v", it)
	}
	if err := tr(task, map[string]any{"to": "open", "reason": "back"}); err != nil {
		t.Fatal(err)
	}
	if it := e.get(t, task); it.WaitingOn != nil {
		t.Fatalf("leaving waiting must clear waiting_on: %+v", it.WaitingOn)
	}
	// done 带证据只对 delegation 强制；两者都可重开。
	if err := tr(task, map[string]any{"to": "done", "reason": "r"}); err != nil {
		t.Fatalf("task done needs no evidence: %v", err)
	}
	if err := tr(dlg, map[string]any{"to": "done", "reason": "r", "evidence": "receipt #1"}); err != nil {
		t.Fatalf("delegation done with evidence: %v", err)
	}
	if err := tr(dlg, map[string]any{"to": "open", "reason": "reopen"}); err != nil {
		t.Fatal(err)
	}

	// needs_input 需要 ask；ask 经 update 写入，内核维护字段写不进去。
	e.must(t, owner, "item.update", map[string]any{"id": task, "ask": map[string]any{"question": "ship?"}})
	if err := tr(task, map[string]any{"to": "needs_input", "reason": "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, "item.update", map[string]any{"id": task, "ask": nil}); exit(err) != domain.ExitUsage {
		t.Fatalf("clearing ask while needs_input must fail: %v", err)
	}
	if _, err := e.do(owner, "item.update", map[string]any{"id": task, "ask": map[string]any{"question": "q", "risk_class": "low"}}); exit(err) != domain.ExitUsage {
		t.Fatalf("kernel-maintained ask field must be refused: %v", err)
	}
	e.verify(t)
}

func TestInvalidWritesAreRefused(t *testing.T) {
	e := setup(t)
	id := e.create(t, owner, map[string]any{"phase": "open"})
	for name, in := range map[string]map[string]any{
		"empty title":        {"kind": "task", "title": " "},
		"empty kind":         {"kind": "", "title": "t"},
		"bad priority":       {"kind": "task", "title": "t", "priority": "meh"},
		"blank tag":          {"kind": "task", "title": "t", "tags": []string{""}},
		"unknown project":    {"kind": "task", "title": "t", "project": "prj_nope"},
		"kernel field":       {"kind": "task", "title": "t", "chain_depth": 3},
		"unknown phase":      {"kind": "task", "title": "t", "phase": "later"},
		"waiting w/o wait":   {"kind": "task", "title": "t", "phase": "waiting"},
		"delegation done":    {"kind": "delegation", "title": "t", "phase": "done"},
		"unknown field":      {"kind": "task", "title": "t", "color": "red"},
		"blank ask question": {"kind": "task", "title": "t", "ask": map[string]any{"question": ""}},
	} {
		if _, err := e.do(owner, "item.create", in); exit(err) != domain.ExitUsage {
			t.Errorf("create %s: want usage error, got %v", name, err)
		}
	}
	for name, in := range map[string]map[string]any{
		"nothing":      {"id": id},
		"phase":        {"id": id, "phase": "done"},
		"kind":         {"id": id, "kind": "commitment"},
		"empty title":  {"id": id, "title": ""},
		"bad priority": {"id": id, "priority": "x"},
	} {
		if _, err := e.do(owner, "item.update", in); exit(err) != domain.ExitUsage {
			t.Errorf("update %s: want usage error, got %v", name, err)
		}
	}
	if _, err := e.do(owner, "item.update", map[string]any{"id": "itm_nope", "title": "x"}); exit(err) != domain.ExitNotFound {
		t.Fatal("missing item must be not_found")
	}
	if got := len(e.events(t)); got != 1 {
		t.Fatalf("refused writes must leave no events, have %d", got)
	}
}

// §14.3：observer 只能以 proposed 新建；接受建议、改他人事项各有门槛。
func TestAuthorizationLevels(t *testing.T) {
	e := setup(t)
	if _, err := e.do(observer, "item.create", map[string]any{"kind": "task", "title": "s", "phase": "open"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("observer creating open: %v", err)
	}
	if _, err := e.do(observer, "item.create", map[string]any{"kind": "task", "title": "s", "phase": "done"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("observer creating done: %v", err)
	}
	p := e.create(t, observer, map[string]any{"title": "suggestion"}) // 缺省阶段就是 proposed
	if it := e.get(t, p); it.Phase != domain.PhaseProposed {
		t.Fatalf("default phase: %s", it.Phase)
	}
	if _, err := e.do(observer, "item.update", map[string]any{"id": p, "title": "x"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("observer update: %v", err)
	}

	// worker 不能自己接受建议（那是代答），本人可以。
	mine := e.create(t, worker, map[string]any{"title": "mine", "assignee": "wrk"})
	if _, err := e.do(worker, "item.transition", map[string]any{"id": mine, "to": "open", "reason": "accept"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker accepting a suggestion: %v", err)
	}
	e.must(t, owner, "item.transition", map[string]any{"id": mine, "to": "open", "reason": "accept"})
	// 对自己的事项 worker 可写，别人的不行；delegate 改别人的需要证据。
	e.must(t, worker, "item.update", map[string]any{"id": mine, "summary": "s"})
	other := e.create(t, owner, map[string]any{"phase": "open", "assignee": "someone"})
	if _, err := e.do(worker, "item.update", map[string]any{"id": other, "summary": "s"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker on others' item: %v", err)
	}
	b, _ := json.Marshal(map[string]any{"id": other, "summary": "s"})
	if _, err := e.x.Do(bg, command.Request{Identity: delegate, Command: "item.update", Input: b, Owner: "owner"}); exit(err) != domain.ExitUsage {
		t.Fatalf("delegate without evidence: %v", err)
	}
	e.must(t, delegate, "item.update", map[string]any{"id": other, "summary": "s"})
	// worker 不能把非 proposed 事项直接建出来，除非是给自己的。
	if _, err := e.do(worker, "item.create", map[string]any{"kind": "task", "title": "x", "phase": "open", "assignee": "someone"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker creating open for others: %v", err)
	}
	e.verify(t)
}

// §14.7 / §6.14 第 5 条：写 assignee 与写 project 都校验名单，拒绝留痕。
func TestAllowedExecutorsChecked(t *testing.T) {
	e := setup(t)
	locked := e.project(t, []string{"bob"})
	open := e.project(t, nil)
	ownerOnly := e.project(t, []string{})

	deny := func(why string, id command.Identity, cmd string, in map[string]any) {
		t.Helper()
		before := rejections(e.events(t))
		_, err := e.do(id, cmd, in)
		var de *domain.Error
		if exit(err) != domain.ExitExecutorNotAllowed || !errors.As(err, &de) {
			t.Fatalf("%s: want exit 21, got %v", why, err)
		}
		if rejections(e.events(t)) != before+1 {
			t.Fatalf("%s: rejection must be recorded", why)
		}
	}
	deny("create with outsider", owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open", "project": locked, "assignee": "alice"})
	deny("owner-only project", owner, "item.create", map[string]any{"kind": "task", "title": "t", "project": ownerOnly, "assignee": "bob"})
	if got := e.events(t)[len(e.events(t))-1]; got.Object == "" || got.Type != domain.EvCommandRejected {
		t.Fatalf("rejection event must name the item: %+v", got)
	}

	for _, p := range []map[string]any{
		{"project": locked, "assignee": "bob"},
		{"project": locked, "assignee": "owner"}, // 本人始终允许
		{"project": locked},                      // 无执行者不校验
		{"project": open, "assignee": "alice"},
		{"assignee": "alice"},
	} {
		p["kind"], p["title"] = "task", "t"
		e.must(t, owner, "item.create", p)
	}

	// 先建在无项目下再移入（或先建在项目里再改派）都拦得住。
	loose := e.create(t, owner, map[string]any{"phase": "open", "assignee": "alice"})
	deny("move into project", owner, "item.update", map[string]any{"id": loose, "project": locked})
	e.must(t, owner, "item.update", map[string]any{"id": loose, "project": open})
	inside := e.create(t, owner, map[string]any{"phase": "open", "project": locked, "assignee": "bob"})
	deny("reassign", owner, "item.update", map[string]any{"id": inside, "assignee": "alice"})
	deny("reassign by delegate", delegate, "item.update", map[string]any{"id": inside, "assignee": "alice"})
	e.must(t, owner, "item.update", map[string]any{"id": inside, "assignee": nil}) // 清空执行者不违规
	if it := e.get(t, inside); it.Assignee != "" || it.Project != locked {
		t.Fatalf("%+v", it)
	}
	e.verify(t)
}

func TestKindSpecSchema(t *testing.T) {
	e := setup(t)
	if err := e.kinds.Register("ticket", json.RawMessage(`{"type":"object","required":["n"],"properties":{"n":{"type":"integer"},"tag":{"enum":["a","b"]}},"additionalProperties":false}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"n":"x"}`, `{"n":1.5}`, `{"n":1,"extra":true}`, `{"n":1,"tag":"c"}`, `[1]`} {
		if _, err := e.do(owner, "item.create", map[string]any{"kind": "ticket", "title": "t", "phase": "open", "spec": json.RawMessage(bad)}); exit(err) != domain.ExitUsage {
			t.Errorf("spec %s must be refused: %v", bad, err)
		}
	}
	if _, err := e.do(owner, "item.create", map[string]any{"kind": "ticket", "title": "t", "phase": "open"}); exit(err) != domain.ExitUsage {
		t.Error("registered kind without required spec must be refused")
	}
	id := e.create(t, owner, map[string]any{"kind": "ticket", "phase": "open", "spec": json.RawMessage(`{"n":2,"tag":"a"}`)})
	if _, err := e.do(owner, "item.update", map[string]any{"id": id, "spec": json.RawMessage(`{"n":"no"}`)}); exit(err) != domain.ExitUsage {
		t.Fatal("update must re-validate spec")
	}
	e.must(t, owner, "item.update", map[string]any{"id": id, "spec": json.RawMessage(`{"n":3}`)})
	if got := string(e.get(t, id).Spec); got != `{"n":3}` {
		t.Fatalf("spec: %s", got)
	}
	// 未注册的 kind 原样存取，ext 内核永不解读。
	free := e.create(t, owner, map[string]any{"kind": "custom", "phase": "open", "spec": json.RawMessage(`{"anything":[1,2]}`), "ext": json.RawMessage(`{"x":{"y":1}}`)})
	if it := e.get(t, free); string(it.Spec) != `{"anything":[1,2]}` || string(it.Ext) != `{"x":{"y":1}}` {
		t.Fatalf("unregistered kind: %s %s", it.Spec, it.Ext)
	}
	for _, schema := range []string{`{"pattern":"x"}`, `{"type":"object"`, `{"type":"nope"}`} {
		if e.kinds.Register("bad", json.RawMessage(schema)) == nil {
			t.Errorf("schema %s must be refused at registration", schema)
		}
	}
}

// §6.8：waiting_on 为时间时登记 Timer，到期以 system/clock 身份回 open，与命令同一版本序列。
func TestWaitUntilTimer(t *testing.T) {
	e := setup(t)
	wake := t0.Add(time.Hour)
	waiting := func(id string) {
		e.must(t, owner, "item.transition", map[string]any{"id": id, "to": "waiting", "reason": "wait",
			"waiting_on": map[string]any{"kind": "time", "ref": wake.Format(time.RFC3339)}})
	}
	id := e.create(t, owner, map[string]any{"phase": "open"})
	waiting(id)
	if n, err := e.clk.Tick(bg); n != 0 || err != nil {
		t.Fatalf("not due yet: %d %v", n, err)
	}
	e.now = wake.Add(time.Minute)
	if n, err := e.clk.Tick(bg); n != 1 || err != nil {
		t.Fatalf("due: %d %v", n, err)
	}
	it := e.get(t, id)
	if it.Phase != domain.PhaseOpen || it.WaitingOn != nil {
		t.Fatalf("after timer: %+v", it)
	}
	last := e.events(t)[len(e.events(t))-1]
	if last.Type != domain.EvItemPhaseChanged || last.Actor.Type != domain.ActorSystem || last.Actor.Via != domain.ViaClock {
		t.Fatalf("timer event: %+v", last)
	}
	if n, _ := e.clk.Tick(bg); n != 0 {
		t.Fatal("one-shot timer must be gone")
	}
	// 版本与命令管线同源：定时改动后旧版本的写入冲突。
	b, _ := json.Marshal(map[string]any{"id": id, "summary": "s"})
	v := int64(2)
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "item.update", Input: b, ExpectVersion: &v}); exit(err) != domain.ExitConflict {
		t.Fatalf("stale version after timer: %v", err)
	}
	v = 3
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "item.update", Input: b, ExpectVersion: &v}); err != nil {
		t.Fatal(err)
	}
	e.verify(t)

	// 提前离开 waiting 取消 Timer；改 waiting_on 重排 Timer。
	early := e.create(t, owner, map[string]any{"phase": "open"})
	waiting(early)
	e.must(t, owner, "item.transition", map[string]any{"id": early, "to": "dropped", "reason": "no"})
	moved := e.create(t, owner, map[string]any{"phase": "open"})
	waiting(moved)
	later := e.now.Add(48 * time.Hour)
	e.must(t, owner, "item.update", map[string]any{"id": moved, "waiting_on": map[string]any{"kind": "time", "ref": later.Format(time.RFC3339)}})
	e.now = e.now.Add(time.Hour)
	if n, _ := e.clk.Tick(bg); n != 0 {
		t.Fatalf("cancelled or rescheduled timers must not fire, fired %d", n)
	}
	if e.get(t, early).Phase != domain.PhaseDropped || e.get(t, moved).Phase != domain.PhaseWaiting {
		t.Fatal("phases changed by a stale timer")
	}
	e.now = later.Add(time.Second)
	if n, _ := e.clk.Tick(bg); n != 1 || e.get(t, moved).Phase != domain.PhaseOpen {
		t.Fatalf("rescheduled timer must fire at the new time (fired %d)", n)
	}
	e.verify(t)
}

// 其他类别的 Timer 不归事项中心：处理器报错而不是吞掉。
func TestTimerHandlerRejectsForeignKinds(t *testing.T) {
	e := setup(t)
	err := e.st.Update(bg, func(tx *store.Tx) error {
		return HandleTimer(tx, clock.Command{Target: "x", Kind: domain.TimerRemind})
	})
	if err == nil {
		t.Fatal("remind timers belong to another module")
	}
	// 目标已被删除或不再等待：清掉 Timer，不报错。
	err = e.st.Update(bg, func(tx *store.Tx) error {
		return HandleTimer(tx, clock.Command{Target: "itm_gone", Kind: domain.TimerWaitUntil, Actor: domain.Actor{Type: domain.ActorSystem, Principal: "clock", Via: domain.ViaClock}})
	})
	if err != nil {
		t.Fatalf("stale timer: %v", err)
	}
}

func TestRelate(t *testing.T) {
	e := setup(t)
	a := e.create(t, owner, map[string]any{"phase": "open"})
	b := e.create(t, owner, map[string]any{"phase": "open"})
	c := e.create(t, owner, map[string]any{"phase": "open"})
	rel := func(from, to, typ string, remove bool) error {
		_, err := e.do(owner, "item.relate", map[string]any{"from": from, "to": to, "type": typ, "remove": remove})
		return err
	}
	if err := rel(b, a, domain.RelParent, false); err != nil {
		t.Fatal(err)
	}
	if err := rel(c, b, domain.RelParent, false); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"duplicate":       rel(b, a, domain.RelParent, false),
		"self":            rel(a, a, domain.RelRelates, false),
		"parent cycle":    rel(a, c, domain.RelParent, false),
		"missing target":  rel(a, "itm_nope", domain.RelRelates, false),
		"blank type":      rel(a, b, "", false),
		"remove missing":  rel(a, b, domain.RelBlocks, true),
		"unknown source":  rel("itm_nope", a, domain.RelRelates, false),
		"type with space": rel(a, b, "a b", false),
	} {
		if err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	if err := rel(a, b, "custom_type", false); err != nil {
		t.Fatalf("relation types are open: %v", err)
	}
	if err := rel(b, a, domain.RelParent, true); err != nil {
		t.Fatal(err)
	}
	var rels []domain.Relation
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { rels, err = Relations(tx, b); return })
	if len(rels) != 2 { // b→? 之前：c parent b、a custom b；b parent a 已删除
		t.Fatalf("relations of b: %+v", rels)
	}
	if _, err := e.do(observer, "item.relate", map[string]any{"from": a, "to": b, "type": "relates"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("observer relate: %v", err)
	}
	e.verify(t)
}

// §6.4 回退：撤销 agent 变更写反向命令，只在 Web；不覆盖后来的改动。
func TestRevertAgentChange(t *testing.T) {
	e := setup(t)
	id := e.create(t, owner, map[string]any{"phase": "open", "title": "orig", "assignee": "wrk"})
	e.must(t, worker, "item.update", map[string]any{"id": id, "title": "agent edit"})
	seq := e.events(t)[len(e.events(t))-1].Seq

	if _, err := e.do(worker, "item.revert", map[string]any{"id": id, "event": seq}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker revert: %v", err)
	}
	before := rejections(e.events(t))
	if _, err := e.do(delegate, "item.revert", map[string]any{"id": id, "event": seq}); exit(err) != domain.ExitWebOnly {
		t.Fatalf("delegate revert must be web-only: %v", err)
	}
	if rejections(e.events(t)) != before+1 {
		t.Fatal("web-only rejection must be recorded")
	}
	r := e.must(t, owner, "item.revert", map[string]any{"id": id, "event": seq})
	if it := e.get(t, id); it.Title != "orig" || it.Version != r.Version {
		t.Fatalf("revert: %+v", it)
	}
	last := e.events(t)[len(e.events(t))-1]
	if last.Type != domain.EvItemUpdated || last.Actor.Principal != "owner" {
		t.Fatalf("revert is a new forward event by the owner: %+v", last)
	}
	e.verify(t)

	// 阶段转移的反向命令回到原阶段；关系变更反向。
	e.must(t, worker, "item.transition", map[string]any{"id": id, "to": "done", "reason": "r"})
	seq = e.events(t)[len(e.events(t))-1].Seq
	e.must(t, owner, "item.revert", map[string]any{"id": id, "event": seq})
	if e.get(t, id).Phase != domain.PhaseOpen {
		t.Fatal("transition revert")
	}
	other := e.create(t, owner, map[string]any{"phase": "open"})
	e.must(t, worker, "item.relate", map[string]any{"from": id, "to": other, "type": "relates"})
	seq = e.events(t)[len(e.events(t))-1].Seq
	e.must(t, owner, "item.revert", map[string]any{"id": id, "event": seq})
	var rels []domain.Relation
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { rels, err = Relations(tx, id); return })
	if len(rels) != 0 {
		t.Fatalf("relation revert: %+v", rels)
	}
	e.verify(t)

	// 非 agent 的变更、非最新变更、不存在的事件都不能撤销。
	e.must(t, worker, "item.update", map[string]any{"id": id, "summary": "one"})
	stale := e.events(t)[len(e.events(t))-1].Seq
	e.must(t, owner, "item.update", map[string]any{"id": id, "summary": "two"})
	human := e.events(t)[len(e.events(t))-1].Seq
	if _, err := e.do(owner, "item.revert", map[string]any{"id": id, "event": stale}); exit(err) != domain.ExitConflict {
		t.Fatalf("revert behind a later change: %v", err)
	}
	if _, err := e.do(owner, "item.revert", map[string]any{"id": id, "event": human}); exit(err) != domain.ExitUsage {
		t.Fatalf("human change is not revertable: %v", err)
	}
	if _, err := e.do(owner, "item.revert", map[string]any{"id": id, "event": 999999}); exit(err) != domain.ExitNotFound {
		t.Fatalf("unknown event: %v", err)
	}
	// 撤销改派时同样遵守名单：名单收紧后，恢复旧执行者被拒。
	locked := e.project(t, []string{"wrk", "ann"})
	asg := e.create(t, owner, map[string]any{"phase": "open", "project": locked, "assignee": "wrk"})
	e.must(t, worker, "item.update", map[string]any{"id": asg, "assignee": "ann"})
	seq = e.events(t)[len(e.events(t))-1].Seq
	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": locked, "executors": []string{"ann"}})
	if _, err := e.do(owner, "item.revert", map[string]any{"id": asg, "event": seq}); exit(err) != domain.ExitExecutorNotAllowed {
		t.Fatalf("revert into a disallowed executor: %v", err)
	}
	if e.get(t, asg).Assignee != "ann" {
		t.Fatal("refused revert must not change the item")
	}
}

func TestListFilters(t *testing.T) {
	e := setup(t)
	p := e.project(t, nil)
	a := e.create(t, owner, map[string]any{"phase": "open", "project": p, "assignee": "ann"})
	e.create(t, owner, map[string]any{"phase": "open", "kind": "commitment"})
	e.create(t, owner, map[string]any{})
	count := func(f Filter) (n int) {
		_ = e.st.View(bg, func(tx *store.Tx) error { its, err := List(tx, f); n = len(its); return err })
		return
	}
	for f, want := range map[Filter]int{
		{}: 3, {Phase: domain.PhaseOpen}: 2, {Phase: domain.PhaseProposed}: 1, {Kind: "commitment"}: 1,
		{Project: p}: 1, {Assignee: "ann", Phase: domain.PhaseOpen}: 1, {Assignee: "nobody"}: 0,
	} {
		if got := count(f); got != want {
			t.Errorf("%+v: got %d, want %d", f, got, want)
		}
	}
	if e.get(t, a).Project != p {
		t.Fatal("project not stored")
	}
	if err := e.st.View(bg, func(tx *store.Tx) error { _, err := Get(tx, "itm_none"); return err }); exit(err) != domain.ExitNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestSchemaValueTypes(t *testing.T) {
	e := setup(t)
	schema := `{"type":"object","properties":{"s":{"type":"string"},"b":{"type":"boolean"},"x":{"type":"number"},"l":{"type":"array","items":{"type":"integer"}},"o":{"type":"object"},"z":{"type":"null"}}}`
	if err := e.kinds.Register("typed", json.RawMessage(schema)); err != nil {
		t.Fatal(err)
	}
	mk := func(spec string) error {
		_, err := e.do(owner, "item.create", map[string]any{"kind": "typed", "title": "t", "phase": "open", "spec": json.RawMessage(spec)})
		return err
	}
	if err := mk(`{"s":"a","b":true,"x":1.5,"l":[1,2],"o":{},"z":null}`); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for _, bad := range []string{`{"s":1}`, `{"b":"y"}`, `{"x":"1"}`, `{"l":[1,"a"]}`, `{"o":[]}`, `{"z":0}`} {
		if exit(mk(bad)) != domain.ExitUsage {
			t.Errorf("%s must be refused", bad)
		}
	}
	for _, s := range []string{`{"required":"n"}`, `{"properties":{"a":1}}`, `{"additionalProperties":{}}`, `{"items":true}`, `{"enum":1}`, `[]`} {
		if e.kinds.Register("bad", json.RawMessage(s)) == nil {
			t.Errorf("schema %s must be refused", s)
		}
	}
	if (*Kinds)(nil).check("x", nil) != nil {
		t.Fatal("nil registry accepts everything")
	}
}
