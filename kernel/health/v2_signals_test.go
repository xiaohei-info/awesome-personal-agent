package health

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) emit(typ, obj string, at time.Time, payload json.RawMessage) {
	e.t.Helper()
	e.update(func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: typ, Object: obj, Reason: "t", Payload: payload, At: at,
			Actor: domain.Actor{Type: domain.ActorSystem, Principal: "system", Via: domain.ViaCLI}})
		return err
	})
}

// delegate 建一个委派子事项并记 delegation.created（与 delegate.create 同一份快照）。
func (e *env) delegate(assignee string, at time.Time) string {
	e.t.Helper()
	var id string
	e.update(func(tx *store.Tx) error {
		it, snap, err := items.Create(tx, domain.Item{Phase: domain.PhaseOpen, Kind: domain.KindDelegation, Title: "d", Assignee: assignee,
			Priority: domain.PriorityNormal}, "", at)
		if err != nil {
			return err
		}
		id = it.ID
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvDelegationCreated, Object: id, Reason: "t", Payload: snap, At: at,
			Actor: domain.Actor{Type: domain.ActorSystem, Principal: "system", Via: domain.ViaCLI}})
		return err
	})
	return id
}

func (e *env) collectAt(now time.Time, after time.Duration) View {
	e.t.Helper()
	var v View
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		v, err = Collect(tx, Options{Deliveries: e.q, Now: func() time.Time { return now }, DelegationStallAfter: after})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// ACT-HLT-04：超过 delegation_stall_after 没有回执的委派进 StalledDelegations，不影响 Healthy；有回执（进展）后清除。
func TestStalledDelegationReportedThenClearedByReceipt(t *testing.T) {
	e := setup(t)
	id := e.delegate("work-host.cli", t0)
	if v := e.collectAt(t0.Add(119*time.Second), 2*time.Minute); len(v.StalledDelegations) != 0 {
		t.Fatalf("not yet stalled: %+v", v.StalledDelegations)
	}
	v := e.collectAt(t0.Add(3*time.Minute), 2*time.Minute)
	if len(v.StalledDelegations) != 1 || v.StalledDelegations[0].Item != id || v.StalledDelegations[0].Assignee != "work-host.cli" || !v.Healthy() {
		t.Fatalf("stalled %+v healthy=%v", v.StalledDelegations, v.Healthy())
	}
	e.emit(domain.EvDelegationReported, id, t0.Add(3*time.Minute), json.RawMessage(`{}`))
	if v := e.collectAt(t0.Add(4*time.Minute), 2*time.Minute); len(v.StalledDelegations) != 0 {
		t.Fatalf("receipt must clear: %+v", v.StalledDelegations)
	}
	if v := e.collectAt(t0.Add(6*time.Minute), 2*time.Minute); len(v.StalledDelegations) != 1 {
		t.Fatalf("silent again after the receipt: %+v", v.StalledDelegations)
	}
	// 默认 24 小时
	if v := e.collectAt(t0.Add(23*time.Hour), 0); len(v.StalledDelegations) != 0 {
		t.Fatalf("default is 24h: %+v", v.StalledDelegations)
	}
	if v := e.collectAt(t0.Add(25*time.Hour), 0); len(v.StalledDelegations) != 1 {
		t.Fatalf("default 24h elapsed: %+v", v.StalledDelegations)
	}
}

// 终结（离开 open）或被改派后清除。
func TestStalledDelegationClearsOnFinishOrReassign(t *testing.T) {
	e := setup(t)
	done, moved := e.delegate("a", t0), e.delegate("a", t0)
	e.update(func(tx *store.Tx) error {
		if _, _, _, err := items.Revise(tx, done, "transition", t0, func(it *domain.Item) error { it.Phase = domain.PhaseDropped; return nil }); err != nil {
			return err
		}
		_, _, _, err := items.Revise(tx, moved, "update", t0, func(it *domain.Item) error { it.Assignee = "b"; return nil })
		return err
	})
	if v := e.collectAt(t0.Add(time.Hour), time.Minute); len(v.StalledDelegations) != 0 {
		t.Fatalf("dropped and reassigned must not be reported: %+v", v.StalledDelegations)
	}
}

// ACT-HLT-05/06：迟到 5 分钟以上的定时记 health.alert 进 ClockLags；准时的不记；之后连续一小时没有迟到即清除。
func TestClockLagRecordedAndClearedAfterAnHour(t *testing.T) {
	e := setup(t)
	fire := func(late time.Duration, at time.Time) {
		e.update(func(tx *store.Tx) error {
			return RecordClockLag(tx, clock.Command{TimerID: "tm_1", Target: "item_hl2", Kind: domain.TimerDue, Late: late}, at)
		})
	}
	fire(4*time.Minute+59*time.Second, t0)
	if v := e.collectAt(t0, 0); len(v.ClockLags) != 0 || len(v.OtherAlerts) != 0 {
		t.Fatalf("under the threshold records nothing: %+v %+v", v.ClockLags, v.OtherAlerts)
	}
	fire(8*time.Minute, t0)
	v := e.collectAt(t0.Add(time.Minute), 0)
	if len(v.ClockLags) != 1 || v.ClockLags[0].Timer != "item_hl2" || !v.Healthy() || len(v.OtherAlerts) != 0 {
		t.Fatalf("lag %+v healthy=%v other=%+v", v.ClockLags, v.Healthy(), v.OtherAlerts)
	}
	if v := e.collectAt(t0.Add(59*time.Minute), 0); len(v.ClockLags) != 1 {
		t.Fatalf("still within the hour: %+v", v.ClockLags)
	}
	if v := e.collectAt(t0.Add(61*time.Minute), 0); len(v.ClockLags) != 0 {
		t.Fatalf("an hour without lateness clears: %+v", v.ClockLags)
	}
	fire(6*time.Minute, t0.Add(70*time.Minute)) // 新的迟到重新出现
	if v := e.collectAt(t0.Add(71*time.Minute), 0); len(v.ClockLags) != 1 {
		t.Fatalf("new lag: %+v", v.ClockLags)
	}
}
