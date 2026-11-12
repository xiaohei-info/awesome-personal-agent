package stale

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	builtin "github.com/xiaohei-info/awesome-personal-agent/packs"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const (
	ruleKey = "follow-up/clock_rules/stale-check"
	// 验收的缩短版：3 分钟没进展算久未推进，每 2 分钟检查一次。
	shortPack = `name: follow-up
version: 2.0.0
description: short stale
clock_rules:
  - id: stale-check
    type: stale
    schedule: "*/2 * * * *"
    stale_after: 3m
`
)

type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	svc *Service
	clk *clock.Clock
	q   *delivery.Queue
	eng *rules.Engine
	ld  *packs.Loader
	now time.Time
}

// setup 起一套真实的账本、命令、触达、时钟与规则；short 为真时用数据目录里的缩短版 stale 规则覆盖内置包。
func setup(t *testing.T, short bool) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, delivery.Module, attention.Module, rules.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: t0}
	now := func() time.Time { return e.now }
	e.x = command.New(st, now)
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := rules.Register(e.x); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if short {
		if err := os.MkdirAll(filepath.Join(dir, "follow-up"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "follow-up", "pack.yaml"), []byte(shortPack), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.ld = packs.New(packs.Options{Builtin: builtin.FS, DataDir: dir, Kinds: items.NewKinds()})
	if rej, err := e.ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %+v", err, rej)
	}
	e.q = delivery.New(delivery.Options{Now: now})
	att := attention.New(attention.Options{Queue: e.q, Now: now})
	if e.svc, err = New(Options{Packs: e.ld, Pack: "follow-up", Reminder: att, Now: now}); err != nil {
		t.Fatal(err)
	}
	if e.eng, err = rules.New(rules.Options{Store: st, Exec: e.x, Packs: e.ld, Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, e.svc.Sync); err != nil {
		t.Fatal(err)
	}
	e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error {
		if e.svc.Owns(c) {
			return e.svc.HandleTimer(tx, c)
		}
		if rules.Owns(c) { // 内置包里别的周期规则：与本模块无关，只需让时钟不报错
			return e.eng.HandleTimer(tx, c)
		}
		return items.HandleTimer(tx, c)
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(cmd string, in any) json.RawMessage {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func (e *env) create(title, kind, phase string, extra map[string]any) string {
	e.t.Helper()
	in := map[string]any{"title": title, "kind": kind, "phase": phase, "assignee": "owner"}
	for k, v := range extra {
		in[k] = v
	}
	var o struct{ ID string }
	_ = json.Unmarshal(e.do("item.create", in), &o)
	return o.ID
}

func (e *env) advance(d time.Duration) {
	e.t.Helper()
	e.now = e.now.Add(d)
	if _, err := e.clk.Tick(bg); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) group() []string {
	e.t.Helper()
	var l items.TodayList
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		l, err = items.TodayItemsAfter(tx, e.now, e.svc.After())
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	var ids []string
	for _, it := range l.Stale {
		ids = append(ids, it.ID)
	}
	return ids
}

func (e *env) unread() []domain.Delivery {
	e.t.Helper()
	var ds []domain.Delivery
	// 默认路由不一定是 Web 回退：未读与待送出都算"已产出的提醒"。
	if err := e.st.View(bg, func(tx *store.Tx) error {
		u, err := e.q.Unread(tx)
		if err != nil {
			return err
		}
		b, err := e.q.Backlog(tx)
		ds = append(u, b...)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return ds
}

func (e *env) get(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) events(typ string) []eventlog.Entry {
	e.t.Helper()
	var es []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		es, err = eventlog.Query(tx, eventlog.Filter{Type: typ}, 0, 500)
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return es
}

// scenario 按验收文档建 ST-1..ST-6，第 2 分钟给 ST-2 写进展，推进到第 4 分钟（检查已触发两次）。
type scenario struct{ st1, st2, st3, st4, st5, st6 string }

func (e *env) scenario() scenario {
	e.t.Helper()
	var s scenario
	s.st1 = e.create("ST-1", "task", "open", nil)
	s.st2 = e.create("ST-2", "task", "open", nil)
	s.st3 = e.create("ST-3", "commitment", "open", nil)
	s.st4 = e.create("ST-4", "task", "open", map[string]any{"planned_for": "2026-03-03"})
	s.st5 = e.create("ST-5", "task", "open", nil)
	e.do("item.transition", map[string]any{"id": s.st5, "to": "waiting", "reason": "sent", "waiting_on": map[string]string{"kind": "human", "ref": "x"}})
	s.st6 = e.create("ST-6", "decision", "open", nil)
	e.advance(2 * time.Minute)
	e.do("item.progress", map[string]any{"id": s.st2, "text": "推了一步"})
	e.advance(2 * time.Minute)
	return s
}

// ACT-STL-02：超过阈值的任务与承诺列入久未推进一组，其余不列。
func TestStaleGroupHonoursRuleThreshold(t *testing.T) {
	e := setup(t, true)
	if got := e.svc.After(); got != 3*time.Minute {
		t.Fatalf("After = %v, want 3m from the pack rule", got)
	}
	s := e.scenario()
	got := e.group()
	if len(got) != 2 || !slices.Contains(got, s.st1) || !slices.Contains(got, s.st3) {
		t.Fatalf("stale group = %v, want exactly ST-1 %s and ST-3 %s", got, s.st1, s.st3)
	}
}

// ACT-STL-03：同一次检查只产出一条合并提醒，不调模型；同一批再检查不重复。
func TestCheckMergesOneReminderAndDoesNotRepeat(t *testing.T) {
	e := setup(t, true)
	s := e.scenario() // 第 4 分钟的检查已触发
	us := e.unread()
	if len(us) != 1 {
		t.Fatalf("unread = %d, want exactly one merged reminder: %+v", len(us), us)
	}
	c := us[0].Content
	if us[0].Class != domain.DeliveryReminder || !strings.Contains(c.Summary, "ST-1") || !strings.Contains(c.Summary, "ST-3") ||
		strings.Contains(c.Summary, "ST-2") || c.WebURL != TodayURL {
		t.Fatalf("reminder content wrong: %+v", us[0])
	}
	e.do("item.progress", map[string]any{"id": s.st2, "text": "又推了一步"}) // ST-2 继续保持新鲜
	e.advance(2 * time.Minute)                                          // 下一次检查，同一批事项
	if n := len(e.unread()); n != 1 {
		t.Fatalf("second check re-reminded the same batch: unread = %d", n)
	}
	e.advance(4 * time.Minute) // ST-2 也过了阈值：批变了，合并成新的一条
	if n := len(e.unread()); n != 2 {
		t.Fatalf("a changed batch should get its own merged reminder: unread = %d", n)
	}
}

// ACT-STL-04：提醒之后不碰事项，它们仍是 open，账本里没有系统改阶段的事件。
func TestReminderNeverChangesItemState(t *testing.T) {
	e := setup(t, true)
	s := e.scenario()
	e.advance(10 * time.Minute)
	for _, id := range []string{s.st1, s.st3} {
		if it := e.get(id); it.Phase != domain.PhaseOpen {
			t.Fatalf("%s phase = %s, want open", id, it.Phase)
		}
	}
	for _, en := range e.events(domain.EvItemPhaseChanged) {
		if en.Event.Actor.Type == domain.ActorSystem {
			t.Fatalf("system changed a phase: %+v", en.Event)
		}
	}
}

// ACT-STL-05：本人一次批量选继续、改期、放弃，各自生效、各自一条 Event、操作者是本人（Web）。
func TestOwnerBatchResolves(t *testing.T) {
	e := setup(t, true)
	s := e.scenario()
	st7 := e.create("ST-7", "task", "open", nil)
	e.advance(4 * time.Minute)
	if got := strings.Join(e.group(), ","); !strings.Contains(got, st7) {
		t.Fatalf("ST-7 not in stale group: %q", got)
	}
	before := len(e.events(domain.EvItemProgressAdded)) + len(e.events(domain.EvItemUpdated)) + len(e.events(domain.EvItemPhaseChanged))
	e.do("item.progress", map[string]any{"id": s.st1, "text": "继续做"})
	e.do("item.update", map[string]any{"id": s.st3, "planned_for": "2026-03-03"})
	e.do("item.transition", map[string]any{"id": st7, "to": "dropped", "reason": "不做了"})
	if e.get(s.st1).Phase != domain.PhaseOpen || e.get(s.st3).PlannedFor != "2026-03-03" || e.get(st7).Phase != domain.PhaseDropped {
		t.Fatalf("choices not applied: %+v %+v %+v", e.get(s.st1), e.get(s.st3), e.get(st7))
	}
	for _, id := range e.group() { // ST-2 此时也过了阈值，属于正常；本人处理过的三条必须离开
		if id == s.st1 || id == s.st3 || id == st7 {
			t.Fatalf("resolved item %s still in stale group", id)
		}
	}
	after := len(e.events(domain.EvItemProgressAdded)) + len(e.events(domain.EvItemUpdated)) + len(e.events(domain.EvItemPhaseChanged))
	if after-before != 3 {
		t.Fatalf("events added = %d, want 3 separate ones", after-before)
	}
	for _, typ := range []string{domain.EvItemProgressAdded, domain.EvItemUpdated} {
		es := e.events(typ)
		if last := es[len(es)-1].Event.Actor; last.Principal != "owner" || last.Via != domain.ViaWeb {
			t.Fatalf("%s actor = %+v, want owner via web", typ, last)
		}
	}
}

// ACT-STL-06：内置包默认 14d、stale 类型、每天检查；新事项不会被误列。
func TestBuiltinDefaultsToFourteenDays(t *testing.T) {
	e := setup(t, false)
	if got := e.svc.After(); got != 14*24*time.Hour {
		t.Fatalf("After = %v, want 14d", got)
	}
	r, ok := e.svc.rule()
	if !ok || r.schedule != "24h" {
		t.Fatalf("builtin stale rule = %+v ok=%v, want daily", r, ok)
	}
	e.create("ST-8", "task", "open", nil)
	e.advance(24 * time.Hour)
	if got := e.group(); len(got) != 0 {
		t.Fatalf("fresh item listed as stale: %v", got)
	}
	if n := len(e.unread()); n != 0 {
		t.Fatalf("reminder for nothing stale: %d", n)
	}
	e.advance(14 * 24 * time.Hour)
	if got := e.group(); len(got) != 1 {
		t.Fatalf("after 14d+ the item should be stale, got %v", got)
	}
}

// 规则模式：observe 只记 rule.observed 不提醒，off 什么都不做。
func TestModesGateTheReminder(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		observed int
	}{{rules.ModeObserve, 1}, {rules.ModeOff, 0}} {
		t.Run(tc.mode, func(t *testing.T) {
			e := setup(t, true)
			e.do("rule.set_mode", map[string]string{"rule": ruleKey, "mode": tc.mode})
			e.scenario()
			if n := len(e.unread()); n != 0 {
				t.Fatalf("%s mode produced %d reminders", tc.mode, n)
			}
			if n := len(e.events(rules.EvObserved)); n < tc.observed || (tc.observed == 0 && n != 0) {
				t.Fatalf("%s mode observed events = %d", tc.mode, n)
			}
		})
	}
}

// 热加载把规则换成别的周期后，Sync 重排；规则消失则撤销 Timer。
func TestSyncFollowsThePack(t *testing.T) {
	e := setup(t, true)
	if _, ok := e.svc.rule(); !ok {
		t.Fatal("rule missing")
	}
	bad, err := New(Options{Packs: e.ld, Pack: "no-such-pack", Reminder: noReminder{}, Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.Update(bg, bad.Sync); err != nil {
		t.Fatal(err)
	}
	e.create("X", "task", "open", nil)
	e.advance(30 * time.Minute) // Timer 已撤销：没有检查，没有提醒
	if n := len(e.unread()); n != 0 {
		t.Fatalf("cancelled timer still fired: %d reminders", n)
	}
	if bad.After() != items.StaleAfter {
		t.Fatalf("After without a rule = %v, want default", bad.After())
	}
}

type noReminder struct{}

func (noReminder) RemindGroup(*store.Tx, domain.Item, string, string, string, string) error {
	return fmt.Errorf("unexpected reminder")
}

func TestParseAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 14 * 24 * time.Hour, "14d": 14 * 24 * time.Hour, "3m": 3 * time.Minute, "72h": 72 * time.Hour} {
		if got, err := parseAfter(in); err != nil || got != want {
			t.Errorf("parseAfter(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"0d", "-1d", "x", "1.5d"} {
		if _, err := parseAfter(in); err == nil {
			t.Errorf("parseAfter(%q) should fail", in)
		}
	}
}

func (e *env) timer() domain.Timer {
	e.t.Helper()
	var tm domain.Timer
	var ok bool
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { tm, ok, err = clock.Get(tx, timerID); return }); err != nil || !ok {
		e.t.Fatalf("stale timer: ok=%v err=%v", ok, err)
	}
	return tm
}

// Sync 幂等：重启或热加载（规则没变）不能把下一次检查往后推；周期变了才重排。
func TestSyncIsIdempotentAndRearmsOnChange(t *testing.T) {
	e := setup(t, false) // 内置 24h
	armed := e.timer().At
	e.advance(12 * time.Hour)
	for i := 0; i < 3; i++ {
		if err := e.st.Update(bg, e.svc.Sync); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.timer().At; !got.Equal(armed) {
		t.Fatalf("Sync moved the timer from %v to %v", armed, got)
	}
	// 错过的检查由时钟补上：模拟停机跨过到期点后重启，再 Tick 即检查。
	e.create("old", "task", "open", nil)
	e.now = e.now.Add(15 * 24 * time.Hour)
	if err := e.st.Update(bg, e.svc.Sync); err != nil {
		t.Fatal(err)
	}
	if _, err := e.clk.Tick(bg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.unread()); n != 1 {
		t.Fatalf("overdue check after restart produced %d reminders, want 1", n)
	}
	// 周期变了：重排。
	short := setup(t, true)
	old := short.timer()
	if old.Recurrence != "*/2 * * * *" {
		t.Fatalf("recurrence = %q", old.Recurrence)
	}
	e.svc.o.Packs = short.ld
	if err := e.st.Update(bg, e.svc.Sync); err != nil {
		t.Fatal(err)
	}
	if got := e.timer(); got.Recurrence != "*/2 * * * *" {
		t.Fatalf("schedule change not re-armed: %+v", got)
	}
}
