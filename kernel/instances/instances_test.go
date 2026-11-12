package instances

import (
	"context"
	"encoding/json"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

// 附录 C.13 的简报包：schedule 必填，projects 是字符串数组。
const briefPack = `name: brief
version: 1.0.0
description: 周期简报
instance_params:
  type: object
  required: [schedule]
  additionalProperties: false
  properties:
    schedule: {type: string}
    projects: {type: array, items: {type: string}}
clock_rules:
  - id: scheduled-brief
    type: recur
    schedule: "{instance.schedule}"
    do:
      - action: call_agent
        prompt: write-brief
`

const prompt = "---\nagent: task\ntimeout: 15m\naccess: read\n---\nwrite a brief for {item_id}\n"

const plainPack = "name: plain\nversion: 1.0.0\ndescription: 无实例参数\n"

type env struct {
	st  *store.Store
	x   *command.Executor
	clk *clock.Clock
	now time.Time
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
	e := &env{st: st, now: t0}
	ld := packs.New(packs.Options{Kinds: items.NewKinds(), Builtin: fstest.MapFS{
		"brief/pack.yaml":              {Data: []byte(briefPack)},
		"brief/prompts/write-brief.md": {Data: []byte(prompt)},
		"plain/pack.yaml":              {Data: []byte(plainPack)},
	}})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load packs: %v %v", err, rej)
	}
	e.x = command.New(st, func() time.Time { return e.now })
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := Register(e.x, ld); err != nil {
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

func (e *env) must(t *testing.T, cmd string, in any) command.Result {
	t.Helper()
	r, err := e.do(owner, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func (e *env) create(t *testing.T, params string) string {
	t.Helper()
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, "instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(params)}).Value, &out)
	return out.ID
}

func (e *env) get(t *testing.T, id string) (in Instance) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { in, err = Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *env) timers(t *testing.T) (n int, at time.Time) {
	t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT at FROM clock_timer`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var ns int64
			_ = r.Scan(&ns)
			n, at = n+1, time.Unix(0, ns).UTC()
		}
		return nil
	})
	return
}

func (e *env) events(t *testing.T, typ string) (out []domain.Event) {
	t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			if x.Event.Type == typ {
				out = append(out, x.Event)
			}
		}
		return err
	})
	return
}

func (e *env) tick(t *testing.T, at time.Time) int {
	t.Helper()
	e.now = at
	n, err := e.clk.Tick(bg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateValidatesParamsAgainstPackSchema(t *testing.T) {
	e := setup(t)
	bad := []struct{ name, pack, params string }{
		{"missing required", "brief", `{}`},
		{"wrong type", "brief", `{"schedule": 5}`},
		{"unknown property", "brief", `{"schedule":"24h","x":1}`},
		{"nested item type", "brief", `{"schedule":"24h","projects":[1]}`},
		{"pack without instance_params", "plain", `{}`},
		{"pack not loaded", "ghost", `{}`},
		{"cron with a wrong field count", "brief", `{"schedule":"0 8 * *"}`},
		{"cron out of range", "brief", `{"schedule":"0 25 * * *"}`},
	}
	for _, c := range bad {
		if _, err := e.do(owner, "instance.create", map[string]any{"pack": c.pack, "params": json.RawMessage(c.params)}); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if n, _ := e.timers(t); n != 0 {
		t.Fatalf("rejected creates left %d timers", n)
	}
	if got := e.events(t, EvCreated); len(got) != 0 {
		t.Fatalf("rejected creates left events: %v", got)
	}
}

// 附录 C.13 的简报示例 `0 8 * * 1-5`（本机时区的工作日 8 点）能建实例；CRON_TZ 前缀指定时区。
func TestCreateAcceptsCronSchedule(t *testing.T) {
	e := setup(t)
	e.create(t, `{"schedule":"0 8 * * 1-5"}`)
	_, at := e.timers(t)
	if l := at.In(time.Local); l.Hour() != 8 || l.Minute() != 0 || l.Weekday() == time.Saturday || l.Weekday() == time.Sunday || !at.After(t0) {
		t.Fatalf("first beat %v", l)
	}
	e = setup(t)
	e.create(t, `{"schedule":"CRON_TZ=UTC 0 8 * * 1-5"}`)
	if _, at := e.timers(t); !at.Equal(t0.AddDate(0, 0, 1)) { // t0 是周四 08:00，下一拍是周五 08:00
		t.Fatalf("first beat %v", at)
	}
}

func TestCreateStoresInstanceAndRecurringTimerAtomically(t *testing.T) {
	e := setup(t)
	id := e.create(t, `{"schedule":"24h","projects":["a"]}`)
	in := e.get(t, id)
	if in.Pack != "brief" || !in.Enabled || string(in.Params) != `{"schedule":"24h","projects":["a"]}` {
		t.Fatalf("instance: %+v", in)
	}
	if n, at := e.timers(t); n != 1 || !at.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("timer count %d at %v", n, at)
	}
	if got := e.events(t, EvCreated); len(got) != 1 || got[0].Object != id {
		t.Fatalf("events: %v", got)
	}
}

func TestOneShotRemindFiresOnceThenTimerIsGone(t *testing.T) {
	e := setup(t)
	id := e.create(t, `{"schedule":"2026-01-01T09:00:00Z"}`)
	if e.tick(t, t0.Add(30*time.Minute)) != 0 {
		t.Fatal("fired early")
	}
	if e.tick(t, t0.Add(2*time.Hour)) != 1 || e.tick(t, t0.Add(3*time.Hour)) != 0 {
		t.Fatal("one-shot should fire exactly once")
	}
	fired := e.events(t, EvFired)
	if len(fired) != 1 || fired[0].Object != id || !fired[0].At.Equal(t0.Add(time.Hour)) {
		t.Fatalf("fired: %+v", fired)
	}
	if fired[0].Actor.Via != domain.ViaClock || fired[0].Actor.Type != domain.ActorSystem {
		t.Fatalf("actor: %+v", fired[0].Actor)
	}
}

func TestRecurringFiresOncePerPeriodAndCatchesUpOnlyLatest(t *testing.T) {
	e := setup(t)
	id := e.create(t, `{"schedule":"24h"}`)
	d1 := t0.Add(24 * time.Hour)
	if e.tick(t, d1) != 1 || e.tick(t, d1.Add(time.Hour)) != 0 {
		t.Fatal("same period must fire once")
	}
	// 停机错过 d2..d5，醒来只补最近一次（d5），下一次回到原节拍 d6。
	if e.tick(t, d1.Add(4*24*time.Hour+time.Hour)) != 1 {
		t.Fatal("catch-up should fire once")
	}
	fired := e.events(t, EvFired)
	if len(fired) != 2 || !fired[1].At.Equal(d1.Add(4*24*time.Hour)) {
		t.Fatalf("fired: %+v", fired)
	}
	if _, at := e.timers(t); !at.Equal(d1.Add(5 * 24 * time.Hour)) {
		t.Fatalf("next beat %v", at)
	}
	_ = id
}

func TestToggleOffCancelsTimerAndOnRestartsPeriod(t *testing.T) {
	e := setup(t)
	id := e.create(t, `{"schedule":"24h"}`)
	e.must(t, "instance.toggle", map[string]any{"id": id, "enabled": false})
	if n, _ := e.timers(t); n != 0 || e.get(t, id).Enabled {
		t.Fatal("disabled instance still has a timer")
	}
	if e.tick(t, t0.Add(72*time.Hour)) != 0 {
		t.Fatal("disabled instance fired")
	}
	e.now = t0.Add(72 * time.Hour)
	e.must(t, "instance.toggle", map[string]any{"id": id, "enabled": true})
	if _, at := e.timers(t); !at.Equal(t0.Add(96 * time.Hour)) {
		t.Fatalf("re-enabled period should restart from now, got %v", at)
	}
	// 重复启用不重置周期、不记事件。
	e.now = t0.Add(80 * time.Hour)
	e.must(t, "instance.toggle", map[string]any{"id": id, "enabled": true})
	if _, at := e.timers(t); !at.Equal(t0.Add(96 * time.Hour)) {
		t.Fatalf("no-op toggle moved the timer: %v", at)
	}
	if got := e.events(t, EvToggled); len(got) != 2 {
		t.Fatalf("toggle events: %d", len(got))
	}
}

func TestUpdateRevalidatesAndReschedules(t *testing.T) {
	e := setup(t)
	id := e.create(t, `{"schedule":"24h"}`)
	if _, err := e.do(owner, "instance.update", map[string]any{"id": id, "params": json.RawMessage(`{"schedule":7}`)}); err == nil {
		t.Fatal("invalid params accepted on update")
	}
	if string(e.get(t, id).Params) != `{"schedule":"24h"}` {
		t.Fatal("failed update changed params")
	}
	e.now = t0.Add(time.Hour)
	e.must(t, "instance.update", map[string]any{"id": id, "params": json.RawMessage(`{"schedule":"12h"}`)})
	if n, at := e.timers(t); n != 1 || !at.Equal(t0.Add(13*time.Hour)) {
		t.Fatalf("timer %d at %v", n, at)
	}
	if _, err := e.do(owner, "instance.update", map[string]any{"id": "ins_nope", "project": ""}); err == nil {
		t.Fatal("unknown instance updated")
	}
}

func TestProjectMustExist(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, "instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h"}`), "project": "prj_nope"}); err == nil {
		t.Fatal("unknown project accepted")
	}
	var p struct{ ID string }
	_ = json.Unmarshal(e.must(t, "project.create", map[string]any{"name": "P"}).Value, &p)
	r := e.must(t, "instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h"}`), "project": p.ID})
	var out struct{ ID string }
	_ = json.Unmarshal(r.Value, &out)
	if e.get(t, out.ID).Project != p.ID {
		t.Fatal("project not stored")
	}
}

func TestOnlyOwnerLevelCallersManageInstances(t *testing.T) {
	e := setup(t)
	worker := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelWorker}
	if _, err := e.do(worker, "instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h"}`)}); err == nil {
		t.Fatal("worker created an instance")
	}
	if n, _ := e.timers(t); n != 0 {
		t.Fatal("denied create left a timer")
	}
}

func TestHandleTimerRejectsForeignTimersAndIgnoresDeletedTarget(t *testing.T) {
	e := setup(t)
	err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := HandleTimer(tx, clock.Command{TimerID: "due_itm_1", Target: "itm_1", Kind: domain.TimerDue}); err == nil {
			t.Error("foreign timer swallowed")
		}
		return HandleTimer(tx, clock.Command{TimerID: timerID("ins_gone"), Target: "ins_gone", Kind: domain.TimerRecur})
	})
	if err != nil {
		t.Fatalf("stale timer for missing instance should be dropped quietly: %v", err)
	}
}

func TestUnrelatedUpdateKeepsScheduleState(t *testing.T) {
	e := setup(t)
	// 周期：改别的参数不重置节拍。
	rec := e.create(t, `{"schedule":"24h"}`)
	e.now = t0.Add(20 * time.Hour)
	e.must(t, "instance.update", map[string]any{"id": rec, "params": json.RawMessage(`{"schedule":"24h","projects":["a"]}`)})
	if _, at := e.timers(t); !at.Equal(t0.Add(24 * time.Hour)) {
		t.Fatalf("unrelated edit moved the next beat to %v", at)
	}
	// 一次性：已触发后改别的参数、停用再启用，都不再触发。
	one := e.create(t, `{"schedule":"2026-01-01T09:00:00Z"}`)
	e.tick(t, t0.Add(2*time.Hour))
	before := len(e.events(t, EvFired))
	e.must(t, "instance.update", map[string]any{"id": one, "params": json.RawMessage(`{"schedule":"2026-01-01T09:00:00Z","projects":["a"]}`)})
	e.must(t, "instance.toggle", map[string]any{"id": one, "enabled": false})
	e.must(t, "instance.toggle", map[string]any{"id": one, "enabled": true})
	e.tick(t, t0.Add(5*time.Hour))
	if got := len(e.events(t, EvFired)); got != before {
		t.Fatalf("fired one-shot fired again: %d -> %d", before, got)
	}
}

func TestNonStringScheduleIsRejected(t *testing.T) {
	// 包的 schema 允许 schedule 是别的类型时，也不能得到一个没有 Timer 的启用实例。
	if _, _, err := schedule(json.RawMessage(`{"schedule":5}`)); err == nil {
		t.Fatal("non-string schedule accepted")
	}
}

func TestDelegateManagesObserverDenied(t *testing.T) {
	e := setup(t)
	lvl := func(l authz.Level) command.Trusted {
		return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "a", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: l}
	}
	in := map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h"}`)}
	if _, err := e.do(lvl(authz.LevelObserver), "instance.create", in); err == nil {
		t.Fatal("observer created an instance")
	}
	if _, err := e.do(lvl(authz.LevelDelegate), "instance.create", in); err != nil {
		t.Fatalf("delegate denied: %v", err)
	}
}
