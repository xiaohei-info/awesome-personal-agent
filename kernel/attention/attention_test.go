package attention

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg = context.Background()
	t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	owner  = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	worker = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: authz.LevelWorker}
)

type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	q   *delivery.Queue
	a   *Attention
	clk *clock.Clock
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, delivery.Module, kcontext.Module, channel.Module, Module} {
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
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.q = delivery.New(delivery.Options{Now: now, UnclaimedAfter: 5 * time.Minute, Lease: 24 * time.Hour})
	e.a = New(Options{Queue: e.q, Now: now})
	e.a.Attach(e.x, st, func(err error) { t.Errorf("attention: %v", err) })
	e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error {
		switch {
		case e.a.Owns(c):
			return e.a.HandleTimer(tx, c)
		case decisions.Owns(c):
			return decisions.HandleTimer(tx, c)
		}
		return items.HandleTimer(tx, c)
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	e.update(e.a.Start)
	return e
}

func (e *env) update(fn func(*store.Tx) error) {
	e.t.Helper()
	if err := e.st.Update(bg, fn); err != nil {
		e.t.Fatal(err)
	}
}

// advance 把时间拨到 d 之后并跑一次时钟扫描（含触达的周期 Timer）。
func (e *env) advance(d time.Duration) {
	e.t.Helper()
	e.now = e.now.Add(d)
	if _, err := e.clk.Tick(bg); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) policy(f func(*policy.Policy)) {
	e.t.Helper()
	e.update(func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error { f(p); return nil })
		return err
	})
}

func (e *env) do(id command.Identity, cmd string, in any) json.RawMessage {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func (e *env) item(extra map[string]any) string {
	e.t.Helper()
	in := map[string]any{"title": "t", "kind": "task", "phase": "open", "assignee": "wrk"}
	for k, v := range extra {
		in[k] = v
	}
	var out struct{ ID string }
	_ = json.Unmarshal(e.do(owner, "item.create", in), &out)
	return out.ID
}

func (e *env) ask(item string, extra map[string]any) {
	e.t.Helper()
	a := map[string]any{"question": "ship it?", "options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}
	for k, v := range extra {
		a[k] = v
	}
	e.do(worker, "decision.request", map[string]any{"id": item, "ask": a})
}

// asked 建事项并提交决策请求。
func (e *env) asked(extra map[string]any) string {
	e.t.Helper()
	id := e.item(nil)
	e.ask(id, extra)
	return id
}

func (e *env) claim(route string) (d *domain.Delivery) {
	e.t.Helper()
	e.update(func(tx *store.Tx) (err error) { d, err = e.q.Claim(tx, route); return })
	return
}

func (e *env) unread() (ds []domain.Delivery) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ds, err = e.q.Unread(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) injectable() (ds []domain.Delivery) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ds, err = e.q.Injectable(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) get(id string) (d domain.Delivery) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { d, err = e.q.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) phase(id string) domain.Phase {
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it.Phase
}

// route 把默认打扰策略的 decision / reminder 都指向渠道 chan，便于用 claim 观察。
func (e *env) toChannel() {
	e.policy(func(p *policy.Policy) {
		p.Disturb.Routes["decision"], p.Disturb.Routes["reminder"] = "chan", "chan"
	})
}

func quiet(from, to time.Time) func(*policy.Policy) {
	return func(p *policy.Policy) {
		p.Disturb.Quiet = &policy.Quiet{Start: from.Format("15:04"), End: to.Format("15:04")}
	}
}

// ---- ACT-DLV-02 ----

func TestQuietHoursHoldDeliveryUntilClockReleasesIt(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(quiet(e.now.Add(-time.Hour), e.now.Add(2*time.Minute)))
	e.asked(nil)

	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("delivered during quiet hours: %+v", d)
	}
	e.advance(time.Minute)
	if d := e.claim("channel:chan"); d != nil {
		t.Fatal("delivered before quiet hours ended")
	}
	e.advance(90 * time.Second)
	d := e.claim("channel:chan")
	if d == nil || d.Class != domain.DeliveryDecision || d.Content.Title == "" || len(d.Content.Options) != 2 {
		t.Fatalf("quiet end did not release the decision: %+v", d)
	}
}

// ---- ACT-DLV-03 ----

func TestNearDeadlineDecisionIgnoresQuietOnlyWhenExempted(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(quiet(e.now.Add(-time.Hour), e.now.Add(time.Hour)))
	soon := e.now.Add(10 * time.Minute)

	e.asked(map[string]any{"deadline": soon})
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("default must be quiet-first, got %+v", d)
	}

	e.policy(func(p *policy.Policy) { p.Disturb.QuietExempt = true })
	second := e.asked(map[string]any{"deadline": soon})
	d := e.claim("channel:chan")
	if d == nil || d.Item != second {
		t.Fatalf("exempt near-deadline decision not delivered: %+v", d)
	}
	// 截止远的决策即使开启豁免也要静默
	e.asked(map[string]any{"deadline": e.now.Add(5 * time.Hour)})
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("far deadline bypassed quiet: %+v", d)
	}
}

// ---- ACT-DLV-04 ----

func TestLowPriorityRemindersMergeButDecisionsDoNot(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(func(p *policy.Policy) { p.Disturb.MergeWindowSec = 120 })
	for i := 0; i < 3; i++ {
		e.item(map[string]any{"priority": "low", "deadline": e.now.Add(time.Duration(i+1) * 10 * time.Second)})
	}
	e.asked(nil)
	e.advance(30 * time.Second) // 三个截止都到了，产生三条低优先级提醒

	d := e.claim("channel:chan")
	if d == nil || d.Class != domain.DeliveryDecision {
		t.Fatalf("the decision must go out alone and immediately: %+v", d)
	}
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("reminders sent before the merge window ended: %+v", d)
	}
	e.advance(2 * time.Minute)
	d = e.claim("channel:chan")
	if d == nil || d.Class != domain.DeliveryReminder {
		t.Fatalf("no merged digest after window: %+v", d)
	}
	if !strings.Contains(d.Content.Title, "3") {
		t.Errorf("digest should say how many it merged: %q", d.Content.Title)
	}
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("reminders were not merged into one: %+v", d)
	}
}

func TestSingleReminderInWindowIsNotWrappedInADigest(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(func(p *policy.Policy) { p.Disturb.MergeWindowSec = 60 })
	id := e.item(map[string]any{"title": "pay rent", "priority": "low", "deadline": e.now.Add(10 * time.Second)})
	e.advance(20 * time.Second)
	e.advance(2 * time.Minute)
	d := e.claim("channel:chan")
	if d == nil || d.Item != id || !strings.Contains(d.Content.Title, "pay rent") {
		t.Fatalf("lone reminder should go out as itself: %+v", d)
	}
}

// ---- ACT-DLV-05 ----

func TestPriorityDecidesRoute(t *testing.T) {
	e := setup(t)
	e.policy(func(p *policy.Policy) {
		p.Disturb.Routes["reminder:high"] = "chan"
		p.Disturb.Routes["reminder:low"] = "inject"
		p.Disturb.MergeWindowSec = 0
	})
	hi := e.item(map[string]any{"priority": "high", "deadline": e.now.Add(10 * time.Second)})
	lo := e.item(map[string]any{"priority": "low", "deadline": e.now.Add(10 * time.Second)})
	e.advance(20 * time.Second)

	d := e.claim("channel:chan")
	if d == nil || d.Item != hi {
		t.Fatalf("high priority must reach the channel: %+v", d)
	}
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("low priority leaked to the channel: %+v", d)
	}
	inj := e.injectable()
	if len(inj) != 1 || inj[0].Item != lo {
		t.Fatalf("low priority must be injectable: %+v", inj)
	}
	// 同一条投递经 apa context --format inject 带出，且只记注入、不算送达
	var ctxRes kcontext.Result
	e.update(func(tx *store.Tx) (err error) {
		ctxRes, err = kcontext.Build(tx, kcontext.Request{Session: "s1"})
		return
	})
	if len(ctxRes.Deliveries) != 1 || ctxRes.Deliveries[0].Item != lo || !strings.Contains(ctxRes.Inject(), "截止已过") {
		t.Fatalf("apa context did not carry the injected reminder: %+v", ctxRes.Deliveries)
	}
	if got := e.get(inj[0].ID); got.State == domain.DeliveryAcknowledged || len(got.Injections) != 1 {
		t.Fatalf("injection must not acknowledge: %+v", got)
	}
}

func TestWebRouteGoesStraightToUnread(t *testing.T) {
	e := setup(t) // 默认 health/wake 之外 decision 走 inject；改成 web
	e.policy(func(p *policy.Policy) { p.Disturb.Routes["decision"] = "web" })
	id := e.asked(nil)
	if u := e.unread(); len(u) != 1 || u[0].Item != id {
		t.Fatalf("web route should be unread at once: %+v", u)
	}
}

// ---- ACT-DLV-06 ----

func TestUnclaimedChannelFallsBackToWebUnread(t *testing.T) {
	e := setup(t)
	e.toChannel()
	id := e.asked(nil)
	if len(e.unread()) != 0 {
		t.Fatal("fresh delivery is not unread yet")
	}
	e.advance(10 * time.Minute) // 超过 UnclaimedAfter，时钟扫描时清扫
	u := e.unread()
	if len(u) != 1 || u[0].Item != id || u[0].State != domain.DeliveryFallbackWeb {
		t.Fatalf("expected one fallback_web delivery, got %+v", u)
	}
}

// ---- ACT-DLV-07 ----

func TestAnsweringSupersedesOldDelivery(t *testing.T) {
	e := setup(t)
	e.toChannel()
	id := e.asked(nil)
	e.do(owner, "decision.answer", map[string]any{"id": id, "choice": "A"})

	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("stale delivery still claimable: %+v", d)
	}
	if got := e.get("decision:" + id + ":" + versionOf(e, id, -1)); got.State != domain.DeliverySuperseded {
		t.Fatalf("old delivery state = %s", got.State)
	}
}

// versionOf 返回事项当前版本减 back（回答前的版本），用来推导旧投递 id。
func versionOf(e *env, id string, back int) string {
	e.t.Helper()
	var v int64
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { v, err = command.Version(tx, id); return })
	return itoa(v + int64(back))
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestEditingPendingDecisionReissuesAtNewVersion(t *testing.T) {
	e := setup(t)
	e.toChannel()
	id := e.asked(nil)
	e.do(owner, "item.update", map[string]any{"id": id, "title": "renamed"})
	d := e.claim("channel:chan")
	if d == nil || d.Item != id {
		t.Fatalf("no current-version delivery: %+v", d)
	}
	if d2 := e.claim("channel:chan"); d2 != nil {
		t.Fatalf("old version must be superseded, got a second claim: %+v", d2)
	}
}

// ---- 不变量 ----

func TestProposedItemsProduceNoDeliveries(t *testing.T) {
	e := setup(t)
	e.toChannel()
	id := e.item(map[string]any{"phase": "proposed", "deadline": e.now.Add(10 * time.Second)})
	e.advance(time.Minute)
	if d := e.claim("channel:chan"); d != nil || len(e.injectable()) != 0 || len(e.unread()) != 0 {
		t.Fatalf("proposed %s produced a delivery", id)
	}
}

func TestDecisionWaitsForScreeningThenDelivers(t *testing.T) {
	e := setup(t)
	e.toChannel()
	// 把另一个事项的 ask 标为初筛未返回：内核维护字段，只能直接改投影来模拟
	other := e.asked(nil)
	e.update(func(tx *store.Tx) error {
		_, _, _, err := items.Revise(tx, other, "test", time.Now(), func(it *domain.Item) error { it.Ask.ScreeningPending = true; return nil })
		return err
	})
	e.update(func(tx *store.Tx) error { _, err := command.Bump(tx, other); return err })
	e.update(func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemUpdated, Object: other, Actor: owner.Actor, Payload: json.RawMessage("{}"), At: e.now})
		return err
	})
	e.update(e.a.Sync)
	if d := e.claim("channel:chan"); d != nil && d.Item == other {
		t.Fatalf("delivered while screening pending: %+v", d)
	}
	e.update(func(tx *store.Tx) error {
		_, _, _, err := items.Revise(tx, other, "test", time.Now(), func(it *domain.Item) error { it.Ask.ScreeningPending = false; return nil })
		return err
	})
	e.update(func(tx *store.Tx) error { _, err := command.Bump(tx, other); return err })
	e.update(func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemUpdated, Object: other, Actor: owner.Actor, Payload: json.RawMessage("{}"), At: e.now})
		return err
	})
	e.update(e.a.Sync)
	d := e.claim("channel:chan")
	if d == nil || d.Item != other {
		t.Fatalf("not delivered after screening ended: %+v", d)
	}
}

// ---- ACT-TMO-02 / ACT-LIF-06 ----

func TestUnansweredDecisionPastDeadlineStaysAndIsRemindedAgain(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(func(p *policy.Policy) { p.Disturb.MergeWindowSec = 0 })
	id := e.asked(map[string]any{"deadline": e.now.Add(time.Minute)})
	first := e.claim("channel:chan")
	if first == nil || first.Class != domain.DeliveryDecision {
		t.Fatalf("decision not delivered: %+v", first)
	}
	e.advance(2 * time.Minute)

	if e.phase(id) != domain.PhaseNeedsInput {
		t.Fatalf("item left needs_input: %s", e.phase(id))
	}
	re := e.claim("channel:chan")
	if re == nil || re.Class != domain.DeliveryReminder || re.Item != id || !strings.Contains(re.Content.Summary, "ship it?") {
		t.Fatalf("no deadline re-reminder: %+v", re)
	}
	e.advance(5 * time.Minute)
	if d := e.claim("channel:chan"); d != nil && d.Class == domain.DeliveryReminder && d.ID != re.ID {
		t.Fatalf("reminder repeated every tick: %+v", d)
	}
}

func TestDeadlineReminderOffSwitchIsHonored(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(func(p *policy.Policy) { p.Disturb.RemindOnDeadline = false })
	e.asked(map[string]any{"deadline": e.now.Add(time.Minute)})
	e.claim("channel:chan")
	e.advance(2 * time.Minute)
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("reminded though policy turned it off: %+v", d)
	}
}

func TestDeadlinePassedReminderSkipsFinishedItems(t *testing.T) {
	e := setup(t)
	e.toChannel()
	id := e.item(map[string]any{"deadline": e.now.Add(time.Minute)})
	e.do(owner, "item.transition", map[string]any{"id": id, "to": "done", "reason": "finished"})
	e.advance(2 * time.Minute)
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("reminded about a done item: %+v", d)
	}
}

func TestTickIsIdempotentAndHeldEntriesDieWithTheirItem(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(func(p *policy.Policy) { p.Disturb.MergeWindowSec = 300 })
	id := e.item(map[string]any{"priority": "low", "deadline": e.now.Add(time.Second)})
	e.advance(5 * time.Second)
	e.advance(5 * time.Second)
	e.do(owner, "item.transition", map[string]any{"id": id, "to": "done", "reason": "finished"})
	e.advance(10 * time.Minute)
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("held reminder for a finished item was released: %+v", d)
	}
}

func TestNewInvalidQuietPolicyIsRejected(t *testing.T) {
	e := setup(t)
	for name, q := range map[string]*policy.Quiet{
		"same": {Start: "22:00", End: "22:00"},
		"bad":  {Start: "25:00", End: "07:00"},
		"zone": {Start: "22:00", End: "07:00", Zone: "Mars/Base"},
	} {
		err := e.st.Update(bg, func(tx *store.Tx) error {
			_, err := policy.Update(tx, owner.Actor, "t", func(p *policy.Policy) error { p.Disturb.Quiet = q; return nil })
			return err
		})
		if err == nil {
			t.Errorf("%s: quiet %+v accepted", name, q)
		}
	}
}

func TestOvernightQuietWrapsPastMidnight(t *testing.T) {
	p := policy.Default()
	p.Disturb.Quiet = &policy.Quiet{Start: "22:00", End: "07:00", Zone: "Asia/Shanghai"}
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.UTC) } // 上海 = UTC+8
	if end, ok := quietEnd(p, at(15, 0)); !ok || !end.Equal(at(23, 0)) {                  // 23:00 上海，次日 07:00 上海 = 23:00 UTC
		t.Errorf("late evening: %v %v", end, ok)
	}
	if end, ok := quietEnd(p, at(20, 0)); !ok || !end.Equal(at(23, 0)) { // 04:00 上海 → 当天 07:00
		t.Errorf("early morning: %v %v", end, ok)
	}
	if _, ok := quietEnd(p, at(3, 0)); ok { // 11:00 上海
		t.Error("midday must not be quiet")
	}
}

// 回归：已退回 Web 未读的投递，事项被回答后不再展示。
func TestAnsweredDecisionLeavesWebUnread(t *testing.T) {
	for name, route := range map[string]string{"web route": "web", "unclaimed channel": "chan"} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.policy(func(p *policy.Policy) { p.Disturb.Routes["decision"] = route })
			id := e.asked(nil)
			e.advance(10 * time.Minute) // 渠道无人领取，清扫后退回 Web
			if len(e.unread()) != 1 {
				t.Fatalf("setup: expected one unread, got %+v", e.unread())
			}
			e.do(owner, "decision.answer", map[string]any{"id": id, "choice": "A"})
			if u := e.unread(); len(u) != 0 {
				t.Fatalf("answered decision still unread: %+v", u)
			}
			if got := e.get("decision:" + id + ":" + versionOf(e, id, -1)); got.State != domain.DeliverySuperseded {
				t.Fatalf("old delivery state = %s", got.State)
			}
		})
	}
}

// 回归：静默期内决策从远截止推进到临近截止，豁免生效，不必等静默结束。
func TestDecisionBecomingNearDeadlineDuringQuietIsReleased(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(quiet(e.now.Add(-time.Hour), e.now.Add(5*time.Hour)))
	e.policy(func(p *policy.Policy) { p.Disturb.QuietExempt = true })
	id := e.asked(map[string]any{"deadline": e.now.Add(2 * time.Hour)})
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("far deadline bypassed quiet: %+v", d)
	}
	e.advance(time.Hour) // 距截止 1h，仍不临近
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("not near yet: %+v", d)
	}
	e.advance(40 * time.Minute) // 距截止 20 分钟，进入临近窗口
	d := e.claim("channel:chan")
	if d == nil || d.Item != id {
		t.Fatalf("decision not released when it became near its deadline: %+v", d)
	}
}

// 静默期内才开启豁免开关：已暂存的临近截止决策随即放行。
func TestEnablingExemptionDuringQuietReleasesHeldNearDeadlineDecision(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(quiet(e.now.Add(-time.Hour), e.now.Add(5*time.Hour)))
	id := e.asked(map[string]any{"deadline": e.now.Add(10 * time.Minute)})
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("held decision leaked: %+v", d)
	}
	e.policy(func(p *policy.Policy) { p.Disturb.QuietExempt = true })
	e.advance(time.Minute)
	if d := e.claim("channel:chan"); d == nil || d.Item != id {
		t.Fatalf("exemption switched on but held decision not released: %+v", d)
	}
}

// 回归：合并进摘要的提醒算已提醒；之后多个 Tick 不再重复产出，新增的逾期提醒是新的投递，且不含旧成员。
func TestMergedMembersAreRememberedAndLaterRemindersGetFreshDigest(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(func(p *policy.Policy) { p.Disturb.MergeWindowSec = 60 })
	e.item(map[string]any{"title": "old-1", "priority": "low", "deadline": e.now.Add(5 * time.Second)})
	e.item(map[string]any{"title": "old-2", "priority": "low", "deadline": e.now.Add(6 * time.Second)})
	e.advance(10 * time.Second)
	e.advance(2 * time.Minute)
	first := e.claim("channel:chan")
	if first == nil || !strings.Contains(first.Content.Title, "2") {
		t.Fatalf("expected a 2-member digest: %+v", first)
	}
	for i := 0; i < 6; i++ { // 多个窗口过去，旧成员不能被重新暂存和重发
		e.advance(time.Minute)
	}
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("merged members re-reminded: %+v", d)
	}

	e.item(map[string]any{"title": "new-1", "priority": "low", "deadline": e.now.Add(-time.Second)})
	e.advance(time.Minute)
	e.advance(2 * time.Minute)
	d := e.claim("channel:chan")
	if d == nil || d.ID == first.ID || !strings.Contains(d.Content.Title, "new-1") || strings.Contains(d.Content.Title+d.Content.Summary, "old-") {
		t.Fatalf("new reminder swallowed or mixed with old members: %+v", d)
	}
}

// 决策投递带推荐（§6.9）；不给推荐时省略。
func TestDecisionDeliveryCarriesRecommend(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.asked(map[string]any{"recommend": "A"})
	if d := e.claim("channel:chan"); d == nil || d.Content.Recommend != "A" || len(d.Content.Options) != 2 {
		t.Fatalf("delivery %+v", d)
	}
	e.asked(nil)
	if d := e.claim("channel:chan"); d == nil || d.Content.Recommend != "" {
		t.Fatalf("delivery without recommend: %+v", d)
	}
}

// 表单链接只发给只能发文字的渠道上的非 web_only 决策；一次性、24 小时过期（§10.3）。
func TestFormLinkOnlyOnTextOnlyChannels(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.asked(nil)
	if d := e.claim("channel:chan"); d == nil || d.Content.FormURL != "" {
		t.Fatalf("interactive channel got a form link: %+v", d)
	}

	e.policy(func(p *policy.Policy) { p.Channels = map[string]policy.Channel{"chan": {TextOnly: true}} })
	id := e.asked(nil)
	d := e.claim("channel:chan")
	token, ok := strings.CutPrefix(d.Content.FormURL, "/forms/")
	if !ok || token == "" {
		t.Fatalf("no form link: %+v", d.Content)
	}

	e.update(func(tx *store.Tx) error {
		f, err := channel.RedeemForm(tx, token, e.now)
		if err != nil || f.Item != id || f.Version != d.Version {
			t.Fatalf("redeem: %+v %v", f, err)
		}
		if _, err := channel.RedeemForm(tx, token, e.now); !hasCode(err, domain.CodeConflict) {
			t.Fatalf("second redeem: %v", err)
		}
		if _, err := channel.RedeemForm(tx, "unknown", e.now); !hasCode(err, domain.CodeNotFound) {
			t.Fatalf("unknown token: %v", err)
		}
		return nil
	})

	e.asked(nil)
	expired := strings.TrimPrefix(e.claim("channel:chan").Content.FormURL, "/forms/")
	e.update(func(tx *store.Tx) error {
		if _, err := channel.RedeemForm(tx, expired, e.now.Add(channel.FormTTL)); !hasCode(err, domain.CodeConflict) {
			t.Fatalf("expired token: %v", err)
		}
		return nil
	})

	// web_only 类不发表单链接。
	e.policy(func(p *policy.Policy) {
		p.WebOnly.Rules = []policy.WebOnlyRule{{Tag: "money", Category: policy.CategoryFunds}}
	})
	web := e.item(map[string]any{"tags": []string{"money"}})
	e.ask(web, nil)
	if d := e.claim("channel:chan"); d == nil || d.Item != web || d.Content.FormURL != "" {
		t.Fatalf("web_only decision got a form link: %+v", d)
	}
}

func hasCode(err error, code domain.Code) bool {
	de, ok := err.(*domain.Error)
	return ok && de.Code == code
}
