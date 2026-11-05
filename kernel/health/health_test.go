package health

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
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
		registry.Module, runner.Module, agentsmd.Module} {
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

// 全新账本：八类信号都正常，两个队列都是零。
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
	e.event(domain.EvHealthAlert, "src_1", "something else", map[string]string{"kind": "other"})
	v := e.collect(nil)
	if len(v.ChainTrips) != 1 || v.ChainTrips[0].Object != "item_1" || v.ChainTrips[0].Depth != 4 {
		t.Fatalf("trips %+v", v.ChainTrips)
	}
	if len(v.ModelUnavailable) != 1 || v.ModelUnavailable[0].Object != "item_2" {
		t.Fatalf("model %+v", v.ModelUnavailable)
	}
	if len(v.RuleFallbacks) != 1 || v.RuleFallbacks[0].Rule != "inbound/x" {
		t.Fatalf("rules %+v", v.RuleFallbacks)
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
