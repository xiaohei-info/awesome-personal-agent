package health

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 诊断页只显示现状：来源认证失败后最近一次拉取成功，信号消失，runner 建的 needs_input 事项被结束。
func TestSourceRecoveryClearsSignalAndEndsItem(t *testing.T) {
	e := setup(t)
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Options{Store: e.st, Exec: e.x, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.source("src_cal", true, runner.Stats{})
	code := 2
	at := t0
	if err := r.Handle(bg, proc.Result{Run: domain.Run{ID: "run_a", Category: domain.RunSource, Target: "src_cal", State: domain.RunFailed, ExitCode: &code, FinishedAt: &at}}); err != nil {
		t.Fatal(err)
	}
	v := e.collect(nil)
	if len(v.SourceFailures) != 1 || v.SourceFailures[0].NeedsInputItem == "" || v.SourceFailures[0].ConsecutiveFailures != 1 {
		t.Fatalf("auth failure: %+v", v.SourceFailures)
	}
	item := v.SourceFailures[0].NeedsInputItem
	if err := r.Handle(bg, proc.Result{Run: domain.Run{ID: "run_b", Category: domain.RunSource, Target: "src_cal", State: domain.RunSucceeded, FinishedAt: &at},
		Stdout: []byte(`{"cursor":"c1"}` + "\n")}); err != nil {
		t.Fatal(err)
	}
	if v = e.collect(nil); len(v.SourceFailures) != 0 || !v.Healthy() {
		t.Fatalf("after recovery: %+v", v.SourceFailures)
	}
	var it domain.Item
	e.update(func(tx *store.Tx) (err error) { it, err = items.Get(tx, item); return })
	if it.Phase != domain.PhaseDone {
		t.Fatalf("item not ended: %+v", it)
	}
}

// 外呼超时：同一目标之后又跑过一次即被取代；没有再跑的，过了 StaleAfter 也不再算现状。
func TestTimeoutSupersededOrStale(t *testing.T) {
	e := setup(t)
	settle := func(r domain.Run, state domain.RunState) {
		e.update(func(tx *store.Tx) error {
			if _, err := tx.Exec("UPDATE outcall_run SET state = 'running' WHERE id = ?", r.ID); err != nil {
				return err
			}
			return proc.Settle(tx, r.ID, state, nil, "x", t0)
		})
	}
	settle(e.enqueue(domain.RunSource, "src_a", t0), domain.RunTimeout)
	settle(e.enqueue(domain.RunTask, "item_b", t0), domain.RunTimeout)
	if v := e.collect(nil); len(v.Outcall.Timeouts) != 2 {
		t.Fatalf("%+v", v.Outcall.Timeouts)
	}
	settle(e.enqueue(domain.RunSource, "src_a", t0), domain.RunFailed)
	if v := e.collect(nil); len(v.Outcall.Timeouts) != 1 || v.Outcall.Timeouts[0].Target != "item_b" {
		t.Fatalf("superseded: %+v", v.Outcall.Timeouts)
	}
	if v := e.collectAt(t0.Add(StaleAfter), 0); len(v.Outcall.Timeouts) != 0 {
		t.Fatalf("stale: %+v", v.Outcall.Timeouts)
	}
}

// 链路熔断、其他告警、规则回退、回执被拒：各自的恢复条件一定会在正常使用中满足。
func TestEventAlertsClearOnCurrentEvidence(t *testing.T) {
	e := setup(t)
	var done, open string
	e.update(func(tx *store.Tx) error {
		for p, phase := range map[*string]domain.Phase{&done: domain.PhaseDropped, &open: domain.PhaseOpen} {
			it, _, err := items.Create(tx, domain.Item{Phase: phase, Kind: domain.KindTask, Title: "t", Priority: domain.PriorityNormal}, "", t0)
			if err != nil {
				return err
			}
			*p = it.ID
		}
		return nil
	})
	e.event(domain.EvHealthAlert, open, chain.ExceededReason, map[string]any{"chain_depth": 9, "threshold": 8})
	e.event(domain.EvHealthAlert, "item.create", chain.ExceededReason, map[string]any{"command": "item.create", "chain_depth": 9, "threshold": 8})
	e.event(domain.EvHealthAlert, "rules:upgrade", "升级说明", map[string]any{"kind": "rules_upgrade"})
	e.event(domain.EvHealthAlert, "rule:p/x", RuleFallbackReason, map[string]string{"rule": "p/x"})
	e.event(domain.EvHealthAlert, done, "delegate.report rejected", map[string]any{"kind": "receipt", "command": "delegate.report", "exit": 23})
	v := e.collect(nil)
	if len(v.ChainTrips) != 2 || len(v.OtherAlerts) != 1 || len(v.RuleFallbacks) != 1 || len(v.ReceiptFailures) != 0 {
		t.Fatalf("trips=%+v other=%+v rules=%+v receipts=%+v", v.ChainTrips, v.OtherAlerts, v.RuleFallbacks, v.ReceiptFailures)
	}
	// 本人在 Web 处理被熔断的事项、重新审阅规则。
	e.update(func(tx *store.Tx) error {
		for _, ev := range []domain.Event{{Type: domain.EvItemUpdated, Object: open}, {Type: domain.EvRuleModeChanged, Object: "rule:p/x"}} {
			ev.Actor, ev.Reason, ev.At, ev.Payload = owner.Actor, "t", t0, []byte(`{}`)
			if _, err := eventlog.Append(tx, ev); err != nil {
				return err
			}
		}
		return nil
	})
	v = e.collect(nil)
	if len(v.ChainTrips) != 1 || v.ChainTrips[0].Object != "item.create" || len(v.RuleFallbacks) != 0 {
		t.Fatalf("after handling: trips=%+v rules=%+v", v.ChainTrips, v.RuleFallbacks)
	}
	if v = e.collectAt(t0.Add(StaleAfter), 0); len(v.ChainTrips) != 0 || len(v.OtherAlerts) != 0 || !v.Healthy() {
		t.Fatalf("after %s: trips=%+v other=%+v", StaleAfter, v.ChainTrips, v.OtherAlerts)
	}
}

// Agent 运行故障（2026-11-20）：任务型、后台运行失败且仍是该目标最近一次运行时列为 Outcall.Failures，带类别码，计入异常；
// 来源与判断的失败有各自的信号，不在这里；之后同一目标再跑一次即被取代。
func TestAgentRunFailuresListedWithFaultCode(t *testing.T) {
	e := setup(t)
	settle := func(r domain.Run, state domain.RunState, exit *int, reason string) {
		e.update(func(tx *store.Tx) error {
			if _, err := tx.Exec("UPDATE outcall_run SET state = 'running' WHERE id = ?", r.ID); err != nil {
				return err
			}
			return proc.Settle(tx, r.ID, state, exit, reason, t0)
		})
	}
	denied := 126
	settle(e.enqueue(domain.RunTask, "item_brief", t0), domain.RunFailed, &denied, "exit status 126: agent-cli: Operation not permitted")
	settle(e.enqueue(domain.RunSource, "src_a", t0), domain.RunFailed, nil, "spawn failed")
	v := e.collect(nil)
	if len(v.Outcall.Failures) != 1 || v.Outcall.Failures[0].Target != "item_brief" || v.Healthy() {
		t.Fatalf("failures %+v", v.Outcall.Failures)
	}
	if f := v.Outcall.Failures[0].Fault; f == nil || f.Code != domain.FaultExecDenied {
		t.Fatalf("fault %+v", f)
	}
	settle(e.enqueue(domain.RunTask, "item_brief", t0), domain.RunSucceeded, nil, "")
	if v := e.collect(nil); len(v.Outcall.Failures) != 0 {
		t.Fatalf("superseded: %+v", v.Outcall.Failures)
	}
}
