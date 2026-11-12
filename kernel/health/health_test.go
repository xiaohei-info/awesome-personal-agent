package health

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

type fakePacks []packs.Rejection

func (f fakePacks) Health() []packs.Rejection { return f }

type env struct {
	t  *testing.T
	st *store.Store
	q  *delivery.Queue
	x  *command.Executor
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, policy.Module, command.Module, proc.Module, delivery.Module,
		registry.Module, runner.Module, agentsmd.Module, chain.Module, direct.Module, items.Module, clock.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, func() time.Time { return t0 })
	if err := registry.Register(x, registry.Options{Bundled: []string{"pull-feed"}}); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, st: st, x: x, q: delivery.New(delivery.Options{Now: func() time.Time { return t0 }})}
}

func (e *env) update(fn func(*store.Tx) error) {
	e.t.Helper()
	if err := e.st.Update(bg, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) collect(p PackHealth) View {
	e.t.Helper()
	var v View
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		v, err = Collect(tx, Options{Deliveries: e.q, Packs: p, Now: func() time.Time { return t0 }})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *env) enqueue(cat domain.RunCategory, target string, at time.Time) domain.Run {
	e.t.Helper()
	var r domain.Run
	e.update(func(tx *store.Tx) (err error) {
		r, err = proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: cat, Target: target, TimeoutSec: 30},
			Spec: proc.Spec{Command: []string{"/bin/true"}}}, at)
		return
	})
	return r
}

func (e *env) source(id string, enabled bool, st runner.Stats) {
	e.t.Helper()
	in, _ := json.Marshal(map[string]any{"id": id, "type": "pull", "source_type": "feed", "command": "pull-feed", "schedule": "15m", "timeout": 60, "enabled": enabled})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: registry.CmdRegister, Input: in}); err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	e.update(func(tx *store.Tx) error { return registry.SetRunState(tx, id, "", b) })
}

func (e *env) event(typ, object, reason string, payload any) {
	e.t.Helper()
	p, _ := json.Marshal(payload)
	e.update(func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: typ, Object: object, Reason: reason, Payload: p, At: t0,
			Actor: domain.Actor{Type: domain.ActorSystem, Principal: "system", Via: domain.ViaRule}, Basis: &domain.Basis{PolicyVersion: 1, RuleID: "r"}})
		return err
	})
}

// 全新账本：九类信号都正常，两个队列都是零。
func TestEmptyLedgerIsHealthy(t *testing.T) {
	v := setup(t).collect(nil)
	if !v.Healthy() || v.Outcall.Long != (QueueHealth{}) || v.Outcall.Short != (QueueHealth{}) {
		t.Fatalf("%+v", v)
	}
}

// flow-health / ACT-HLT-02：长时与短任务的排队数与最长等待分开显示，互不混算。
func TestQueuesReportedSeparatelyWithLongestWait(t *testing.T) {
	e := setup(t)
	e.enqueue(domain.RunTask, "item_a", t0.Add(-10*time.Minute))
	e.enqueue(domain.RunBackground, "item_b", t0.Add(-3*time.Minute))
	e.enqueue(domain.RunSource, "src_a", t0.Add(-40*time.Second))
	v := e.collect(nil)
	if got := v.Outcall.Long; got.Queued != 2 || got.LongestWait != 10*time.Minute {
		t.Fatalf("long %+v", got)
	}
	if got := v.Outcall.Short; got.Queued != 1 || got.LongestWait != 40*time.Second {
		t.Fatalf("short %+v", got)
	}
}

// 外呼超时：timeout 态的 Run 被列出；成功与失败的不算超时。
func TestOutcallTimeoutsListed(t *testing.T) {
	e := setup(t)
	a := e.enqueue(domain.RunTask, "item_a", t0)
	b := e.enqueue(domain.RunTask, "item_b", t0)
	c := e.enqueue(domain.RunSource, "src_c", t0)
	e.update(func(tx *store.Tx) error {
		if _, err := tx.Exec("UPDATE outcall_run SET state = 'running' WHERE id IN (?, ?)", a.ID, b.ID); err != nil {
			return err
		}
		if err := proc.Settle(tx, a.ID, domain.RunTimeout, nil, "timeout", t0); err != nil {
			return err
		}
		if err := proc.Settle(tx, b.ID, domain.RunSucceeded, nil, "", t0); err != nil {
			return err
		}
		return proc.Settle(tx, c.ID, domain.RunFailed, nil, "spawn failed", t0)
	})
	v := e.collect(nil)
	if len(v.Outcall.Timeouts) != 1 || v.Outcall.Timeouts[0].ID != a.ID {
		t.Fatalf("%+v", v.Outcall.Timeouts)
	}
	if v.Outcall.Long.Queued != 0 || v.Healthy() {
		t.Fatalf("%+v", v)
	}
}

func TestSourceFailuresIncludeDisabledAndNeedsInput(t *testing.T) {
	e := setup(t)
	e.source("src_ok", true, runner.Stats{Runs: 5})
	e.source("src_flaky", true, runner.Stats{ConsecutiveFailures: 3, LastError: "http 502"})
	e.source("src_auth", true, runner.Stats{NeedsInputItem: "item_auth"})
	e.source("src_off", false, runner.Stats{LastError: "exit 3"})
	v := e.collect(nil)
	got := map[string]SourceFailure{}
	for _, f := range v.SourceFailures {
		got[f.Source] = f
	}
	if len(got) != 3 || got["src_ok"].Source != "" {
		t.Fatalf("%+v", v.SourceFailures)
	}
	if f := got["src_flaky"]; f.ConsecutiveFailures != 3 || f.LastError != "http 502" || !f.Enabled {
		t.Fatalf("%+v", f)
	}
	if got["src_auth"].NeedsInputItem != "item_auth" || got["src_off"].Enabled {
		t.Fatalf("%+v", got)
	}
}

func TestDeliveryBacklogCountsOnlyUnsent(t *testing.T) {
	e := setup(t)
	mk := func(id string) {
		e.update(func(tx *store.Tx) error {
			_, _, err := e.q.Create(tx, domain.Delivery{ID: id, Item: "item_1", Class: domain.DeliveryBrief, Route: delivery.ChannelRoute("c"), Version: 1,
				Content: domain.DeliveryContent{Title: id}})
			return err
		})
	}
	mk("d1")
	mk("d2")
	mk("d3")
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE delivery_delivery SET state = 'acknowledged' WHERE id = 'd1'")
		return err
	})
	v := e.collect(nil)
	if len(v.DeliveryBacklog) != 2 || v.DeliveryBacklog[0].ID != "d2" || v.DeliveryBacklog[1].ID != "d3" {
		t.Fatalf("%+v", v.DeliveryBacklog)
	}
}

func TestEventBackedSignals(t *testing.T) {
	e := setup(t)
	e.event(domain.EvHealthAlert, "item_1", chain.ExceededReason, map[string]any{"chain_depth": 4, "threshold": 3})
	e.event(domain.EvHealthAlert, "item_2", "web_only screening did not complete: model timeout", map[string]any{"kind": "screening", "reason": "model timeout"})
	e.event(domain.EvHealthAlert, "rule:inbound/x", RuleFallbackReason, map[string]string{"rule": "inbound/x"})
	e.event(domain.EvHealthAlert, "src_1", "source src_1 disabled", map[string]string{"kind": "source"})
	e.event(domain.EvHealthAlert, "pack:work", "capability pack work rejected", map[string]string{"kind": "pack"})
	e.event(domain.EvHealthAlert, "item_4", "rule action failed: boom", map[string]string{"rule": "r/x", "action": "create_timer", "error": "boom"})
	e.event(domain.EvHealthAlert, "item_3", "decision-card-copy draft failed", map[string]string{"kind": DraftKind, "template": "decision-card-copy"})
	v := e.collect(nil)
	if len(v.ChainTrips) != 1 || v.ChainTrips[0].Object != "item_1" || v.ChainTrips[0].Depth != 4 {
		t.Fatalf("trips %+v", v.ChainTrips)
	}
	if len(v.ModelUnavailable) != 2 || v.ModelUnavailable[0].Object != "item_2" || v.ModelUnavailable[1].Object != "item_3" {
		t.Fatalf("model %+v", v.ModelUnavailable)
	}
	if len(v.RuleFallbacks) != 1 || v.RuleFallbacks[0].Rule != "inbound/x" {
		t.Fatalf("rules %+v", v.RuleFallbacks)
	}
	// 来源与能力包告警各有专属信号，不重复进通用列表；规则动作失败进通用列表并使视图不健康。
	if len(v.OtherAlerts) != 1 || v.OtherAlerts[0].Object != "item_4" || v.Healthy() {
		t.Fatalf("other %+v healthy=%v", v.OtherAlerts, v.Healthy())
	}
}

// flow-follow-up / ACT-FUP-06：跟进判断失败进"模型不可用"并使视图不健康；同一事项之后同模板成功即清除。
func TestFollowupFailureSurfacesAndRecovers(t *testing.T) {
	e := setup(t)
	fail := func(item string) {
		e.event(domain.EvHealthAlert, item, "follow-up judgement failed, will retry: model 500",
			map[string]string{"kind": FollowupKind, "template": "judge-follow", "item": item})
	}
	ok := func(item, state string, reused bool) {
		e.event(domain.EvRunFinished, item, "judge "+state, map[string]any{"template": "judge-follow", "state": state, "reused": reused})
	}
	ok("item_1", "succeeded", false) // 失败之前的成功不算恢复
	fail("item_1")
	fail("item_2")
	v := e.collect(nil)
	if v.Healthy() || len(v.ModelUnavailable) != 2 || v.ModelUnavailable[0].Kind != FollowupKind {
		t.Fatalf("not surfaced: %+v", v.ModelUnavailable)
	}
	ok("item_1", "failed", false)
	ok("item_1", "succeeded", true) // 复用缓存没有真实调用
	if v = e.collect(nil); len(v.ModelUnavailable) != 2 {
		t.Fatalf("failed/reused call cleared the signal: %+v", v.ModelUnavailable)
	}
	ok("item_1", "succeeded", false)
	// item_1 恢复；item_2 仍在失败，不能被 item_1 的成功盖住。
	if v = e.collect(nil); len(v.ModelUnavailable) != 1 || v.ModelUnavailable[0].Object != "item_2" || v.Healthy() {
		t.Fatalf("after item_1 recovered: %+v", v.ModelUnavailable)
	}
	ok("item_2", "succeeded", false)
	if v = e.collect(nil); !v.Healthy() {
		t.Fatalf("not cleared after recovery: %+v", v)
	}
	// 再次失败重新出现。
	fail("item_1")
	if v = e.collect(nil); len(v.ModelUnavailable) != 1 {
		t.Fatalf("new failure hidden: %+v", v.ModelUnavailable)
	}
}

// 初筛不重试：任一事项之后同模板成功即算恢复；模板不同的成功不算。
func TestScreeningRecoversOnLaterSuccessOfSameTemplate(t *testing.T) {
	e := setup(t)
	e.event(domain.EvHealthAlert, "item_1", "web_only screening did not complete: timeout", map[string]any{"kind": ScreeningKind, "template": "screen"})
	e.event(domain.EvRunFinished, "item_9", "judge succeeded", map[string]any{"template": "other", "state": "succeeded"})
	if v := e.collect(nil); len(v.ModelUnavailable) != 1 || v.ModelUnavailable[0].Kind != ScreeningKind {
		t.Fatalf("%+v", v.ModelUnavailable)
	}
	e.event(domain.EvRunFinished, "item_2", "judge succeeded", map[string]any{"template": "screen", "state": "succeeded"})
	if v := e.collect(nil); !v.Healthy() {
		t.Fatalf("%+v", v.ModelUnavailable)
	}
}

// decision-card 起草失败进"模型不可用"并使视图不健康；起草逐事项一次、不重试，之后任一事项同模板的真实成功调用即清除，
// 复用缓存、失败或别的模板（卡片文案与阈值判定各自恢复）都不算。
func TestDraftFailureSurfacesAndRecoversOnSameTemplate(t *testing.T) {
	e := setup(t)
	e.event(domain.EvHealthAlert, "item_1", "decision-card-copy draft failed", map[string]string{"kind": DraftKind, "template": "decision-card-copy"})
	e.event(domain.EvHealthAlert, "item_1", "delivery-threshold draft failed", map[string]string{"kind": DraftKind, "template": "delivery-threshold"})
	if v := e.collect(nil); v.Healthy() || len(v.ModelUnavailable) != 2 || v.ModelUnavailable[0].Kind != DraftKind {
		t.Fatalf("not surfaced: %+v", v.ModelUnavailable)
	}
	run := func(item, tmpl, state string, reused bool) {
		e.event(domain.EvRunFinished, item, "generate "+state, map[string]any{"template": tmpl, "state": state, "reused": reused})
	}
	run("item_2", "decision-card-copy", "failed", false)
	run("item_2", "decision-card-copy", "succeeded", true)
	run("item_2", "one-line-summary", "succeeded", false)
	if v := e.collect(nil); len(v.ModelUnavailable) != 2 {
		t.Fatalf("failed/reused/other-template call cleared the signal: %+v", v.ModelUnavailable)
	}
	run("item_2", "decision-card-copy", "succeeded", false)
	if v := e.collect(nil); len(v.ModelUnavailable) != 1 || v.ModelUnavailable[0].Reason != "delivery-threshold draft failed" || v.Healthy() {
		t.Fatalf("after card recovered: %+v", v.ModelUnavailable)
	}
	run("item_3", "delivery-threshold", "succeeded", false)
	if v := e.collect(nil); !v.Healthy() {
		t.Fatalf("not cleared after recovery: %+v", v.ModelUnavailable)
	}
}

func TestDriftPackAndUnjudgedBacklog(t *testing.T) {
	e := setup(t)
	e.update(func(tx *store.Tx) error {
		for item, state := range map[string]string{"item_open": "open", "item_done": "merge"} {
			if _, err := tx.Exec("INSERT INTO agentsmd_drift (item, repo, file_hash, render_hash, content, state) VALUES (?, 'r', ?, 'rh', '', ?)", item, item, state); err != nil {
				return err
			}
		}
		for id, disp := range map[string]string{"ev1": "unjudged", "ev2": "pending", "ev3": "ignored"} {
			if _, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
				occurred_at, correlation_keys, sender, sender_verified, disposition) VALUES ('d', ?, ?, 'src_1', 'run', 't', 's', '', '', '', 0, '[]', '', 0, ?)`, id, id, disp); err != nil {
				return err
			}
		}
		return nil
	})
	rej := fakePacks{{Pack: "work", Source: "user", Reason: "bad schema"}}
	v := e.collect(rej)
	if len(v.AgentsMDDrift) != 1 || v.AgentsMDDrift[0].Item != "item_open" {
		t.Fatalf("drift %+v", v.AgentsMDDrift)
	}
	if len(v.Unjudged) != 1 || v.Unjudged[0].ID != "ev1" {
		t.Fatalf("unjudged %+v", v.Unjudged)
	}
	if len(v.PackRejections) != 1 || v.PackRejections[0].Pack != "work" {
		t.Fatalf("packs %+v", v.PackRejections)
	}
}

// 只报不修：汇总前后账本里的 Run 与投递都不变。
func TestCollectIsReadOnly(t *testing.T) {
	e := setup(t)
	r := e.enqueue(domain.RunTask, "item_a", t0.Add(-time.Minute))
	e.collect(nil)
	var rec proc.Record
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { rec, err = proc.Get(tx, r.ID); return }); err != nil {
		t.Fatal(err)
	}
	if rec.Run.State != domain.RunQueued {
		t.Fatalf("%+v", rec.Run)
	}
}

func TestCollectRequiresDeliveries(t *testing.T) {
	e := setup(t)
	if err := e.st.View(bg, func(tx *store.Tx) error { _, err := Collect(tx, Options{}); return err }); err == nil {
		t.Fatal("expected error")
	}
}

// sync 跑一遍健康告警的投递产出，返回全部健康类投递（按产出顺序）。
func (e *env) sync() []domain.Delivery {
	e.t.Helper()
	var out []domain.Delivery
	e.update(func(tx *store.Tx) (err error) {
		if err = NewNotifier(e.q).Sync(tx); err == nil {
			out, err = e.q.Unread(tx)
		}
		return
	})
	return out
}

// 所有健康告警经同一处产出健康类投递：链路熔断与来源失败各一条、不重复；初筛与起草失败只进健康视图；重复消费幂等。
func TestEveryAlertYieldsOneHealthDeliveryExceptScreening(t *testing.T) {
	e := setup(t)
	e.event(domain.EvHealthAlert, "item_1", chain.ExceededReason, map[string]any{"command": "delegate.create", "chain_depth": 4, "threshold": 3})
	e.event(domain.EvHealthAlert, "src_a", "source src_a disabled after permanent failure (exit 3): bad token", map[string]any{"kind": "source", "source_id": "src_a"})
	e.event(domain.EvHealthAlert, "item_2", "web_only screening did not complete: model timeout", map[string]any{"kind": ScreeningKind})
	e.event(domain.EvHealthAlert, "item_2", "decision card draft failed", map[string]any{"kind": DraftKind, "template": "decision-card-copy"})
	e.event(domain.EvHealthAlert, "rule:inbound/x", RuleFallbackReason, map[string]string{"rule": "inbound/x"})
	e.event(domain.EvHealthAlert, "item_1", chain.ExceededReason, map[string]any{"command": "item.create", "chain_depth": 5, "threshold": 3})
	e.event(domain.EvHealthAlert, "item_3", "follow-up judgement failed, will retry: model 500", map[string]any{"kind": FollowupKind})

	first := e.sync()
	if len(first) != 5 {
		t.Fatalf("deliveries = %+v", first)
	}
	byItem := map[string][]domain.Delivery{}
	for _, d := range first {
		if d.Class != domain.DeliveryHealth || d.Route != delivery.RouteWeb || !strings.HasPrefix(d.ID, "health:") {
			t.Fatalf("delivery = %+v", d)
		}
		byItem[d.Item] = append(byItem[d.Item], d)
	}
	if len(byItem["item_1"]) != 2 || len(byItem["src_a"]) != 1 || len(byItem["rule:inbound/x"]) != 1 || len(byItem["item_2"]) != 0 || len(byItem["item_3"]) != 1 {
		t.Fatalf("by item = %+v", byItem)
	}
	if d := byItem["item_1"][0]; !strings.Contains(d.Content.Summary, "超过阈值 3") || d.Content.WebURL != "/items/item_1" {
		t.Fatalf("trip delivery = %+v", d)
	}
	if d := byItem["src_a"][0]; !strings.Contains(d.Content.Summary, "bad token") || d.Content.WebURL != "/health" {
		t.Fatalf("source delivery = %+v", d)
	}
	if again := e.sync(); len(again) != 5 {
		t.Fatalf("repeat sync changed deliveries: %+v", again)
	}
}

// 没有对象的命令被拦下时 Object 是命令名，投递不带指向它的链接。
func TestObjectlessTripHasNoItemLink(t *testing.T) {
	e := setup(t)
	e.event(domain.EvHealthAlert, "item.create", chain.ExceededReason, map[string]any{"command": "item.create", "chain_depth": 9, "threshold": 8})
	if ds := e.sync(); len(ds) != 1 || ds[0].Content.WebURL != "/health" {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// ACT-PACK-04：能力包加载失败经加载器的 OnReject 记 health.alert，产出一条健康类投递；同一内容再加载不重复。
func TestPackRejectionYieldsOneHealthDelivery(t *testing.T) {
	e := setup(t)
	l := packs.New(packs.Options{Builtin: fstest.MapFS{"brief/pack.yaml": {Data: []byte("name: brief\nversion: [\n")}}, Kinds: items.NewKinds(),
		OnReject: func(r packs.Rejection) { e.update(func(tx *store.Tx) error { return PackRejected(tx, r) }) }})
	for i := 0; i < 2; i++ {
		if _, err := l.Load(); err != nil {
			t.Fatal(err)
		}
	}
	ds := e.sync()
	if len(ds) != 1 || ds[0].Item != "pack:brief" || ds[0].Class != domain.DeliveryHealth {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// 路由取策略里健康类的配置。
func TestHealthRouteFollowsPolicy(t *testing.T) {
	for in, want := range map[string]string{"": delivery.RouteWeb, "web": delivery.RouteWeb, "inject": delivery.RouteInject,
		"channel:tg": "channel:tg", "tg": delivery.ChannelRoute("tg")} {
		p := policy.Default()
		p.Disturb.Routes = map[string]string{"health": in}
		if got := route(p); got != want {
			t.Errorf("route(%q) = %q, want %q", in, got, want)
		}
	}
}

// 游标：账本回到较早状态时从头重算，投递幂等。
func TestSyncRestartsWhenLedgerRewinds(t *testing.T) {
	e := setup(t)
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO health_cursor (id, seq) VALUES (1, 9999) ON CONFLICT(id) DO UPDATE SET seq = excluded.seq`)
		return err
	})
	e.event(domain.EvHealthAlert, "src_a", "x", map[string]string{"kind": "source"})
	if ds := e.sync(); len(ds) != 1 {
		t.Fatalf("deliveries = %+v", ds)
	}
}

// 已有账本升级：健康游标接手 chain 的旧游标，历史告警不被补发。
func TestCursorInheritedFromChainOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	open := func(mods ...store.Module) *store.Store {
		reg := store.NewRegistry()
		for _, m := range mods {
			if err := reg.Register(m); err != nil {
				t.Fatal(err)
			}
		}
		st, err := store.Open(dir, reg)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	old := open(chain.Module)
	if err := old.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO chain_cursor (id, seq) VALUES (1, 7)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	old.Close()
	st := open(chain.Module, Module)
	defer st.Close()
	var seq int64
	if err := st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT seq FROM health_cursor WHERE id = 1`)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			err = r.Scan(&seq)
		}
		return err
	}); err != nil || seq != 7 {
		t.Fatalf("cursor = %d, %v", seq, err)
	}
}
