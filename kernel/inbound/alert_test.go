package inbound

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/why"
)

const alertRule = "inbound-triage/source_rules/x-alert"

func metric(id string, v float64) map[string]any {
	return ev(id, "x.metric", "磁盘", map[string]any{"correlation_keys": []string{"disk-a"}, "metrics": map[string]float64{"value": v},
		"summary": "磁盘用量", "link": "https://mon.example/disk-a"})
}

func (e *env) alertDelivery(eventID string) (d domain.Delivery, found bool) {
	e.t.Helper()
	id := attention.AlertID(e.disposition("src_m", eventID).ID)
	_ = e.st.View(bg, func(tx *store.Tx) error {
		got, err := e.queue.Get(tx, id)
		d, found = got, err == nil
		return nil
	})
	return
}

func (e *env) observedFor(eventID string) (n int) {
	e.t.Helper()
	id := e.disposition("src_m", eventID).ID
	_ = e.st.View(bg, func(tx *store.Tx) error {
		entries, err := eventlog.ReadObject(tx, id)
		for _, en := range entries {
			if en.Event.Type == rules.EvObserved && en.Event.Actor.Principal == "rule:"+alertRule {
				n++
			}
		}
		return err
	})
	return
}

// ACT-IRL-09、ACT-DGA-04/05：序列第一次出现与未上穿时不成立；observe 档只在上穿那一条记“本来会提醒”，不投递、不建事项、不调模型。
func TestAlertObserveRecordsOnlyTheCrossing(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_m", "exits", metric("m1", 95), metric("m2", 80), metric("m3", 92))
	for id, want := range map[string]int{"m1": 0, "m2": 0, "m3": 1} {
		d := e.disposition("src_m", id)
		if d.Disposition != domain.DispositionIgnored || e.observedFor(id) != want {
			t.Errorf("%s: disposition %s observed %d, want ignored/%d", id, d.Disposition, e.observedFor(id), want)
		}
		if _, found := e.alertDelivery(id); found {
			t.Errorf("%s: observe produced a delivery", id)
		}
	}
	if len(e.allItems()) != 0 || len(e.stub.Requests()) != 0 {
		t.Fatalf("items %d model requests %d", len(e.allItems()), len(e.stub.Requests()))
	}
}

// ACT-DGA-07/08/10：auto 档上穿产出没有事项、带 origin 的提醒；持续高于阈值不重复；apa why 回放规则、条件、上一次取值与投递。
func TestAlertAutoDeliversOnceAndWhyReplaysIt(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_m", "exits", metric("m0", 50)) // 先登记来源、对账规则
	e.must("rule.set_mode", map[string]string{"rule": alertRule, "mode": rules.ModeAuto})
	e.emit("src_m", "exits", metric("m1", 70), metric("m2", 96), metric("m3", 97))

	if d := e.disposition("src_m", "m1"); d.Disposition != domain.DispositionIgnored {
		t.Fatalf("70: %s", d.Disposition)
	}
	if _, found := e.alertDelivery("m3"); found || e.disposition("src_m", "m3").Disposition != domain.DispositionIgnored {
		t.Fatal("97 after 96 is not a crossing but alerted")
	}
	hit := e.disposition("src_m", "m2")
	d, found := e.alertDelivery("m2")
	if hit.Disposition != domain.DispositionAlerted || !found || hit.Item != "" || len(e.allItems()) != 0 {
		t.Fatalf("96: event %+v delivery found=%v items=%d", hit, found, len(e.allItems()))
	}
	if d.Item != "" || d.Class != domain.DeliveryReminder || d.Origin == nil || d.Origin.SourceEvent != hit.ID || d.Origin.Rule != alertRule ||
		d.Content.Title != "磁盘" || d.Content.Summary != "磁盘用量" || d.Content.WebURL != "https://mon.example/disk-a" {
		t.Fatalf("delivery %+v origin %+v", d, d.Origin)
	}
	if len(e.stub.Requests()) != 0 {
		t.Fatal("alert exit called the model")
	}

	var tl why.Timeline
	if err := e.st.View(bg, func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		tl, err = why.Replay(tx, pol, why.Request{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "obs"}, Level: authz.LevelObserver, ID: hit.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(tl.Alerts) != 1 {
		t.Fatalf("alerts %+v", tl.Alerts)
	}
	a := tl.Alerts[0]
	var cs []struct {
		Condition string
		Previous  *float64
		Current   float64
	}
	if err := json.Unmarshal(a.Conditions, &cs); err != nil || len(cs) != 1 {
		t.Fatalf("conditions %s: %v", a.Conditions, err)
	}
	if a.Rule != alertRule || cs[0].Condition != "metrics.value crosses_above 90" || cs[0].Previous == nil || *cs[0].Previous != 70 || cs[0].Current != 96 ||
		a.Delivery == nil || a.Delivery.ID != d.ID || a.Delivery.State == "" || !strings.HasPrefix(a.Delivery.Class, "reminder") {
		t.Fatalf("replay %+v conditions %+v", a, cs)
	}
}

// rules.Series 的契约：规则 off 时不提醒也不记观察。
func TestAlertOffRuleDoesNothing(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_m", "exits", metric("m0", 50))
	e.must("rule.set_mode", map[string]string{"rule": alertRule, "mode": rules.ModeOff})
	e.emit("src_m", "exits", metric("m1", 99))
	if _, found := e.alertDelivery("m1"); found || e.observedFor("m1") != 0 || e.disposition("src_m", "m1").Disposition != domain.DispositionIgnored {
		t.Fatal("off rule produced a trace")
	}
}

// ACT-IRL-12：changed 与 crosses_below 只与同一序列（来源，关联键，指标）的上一次取值比较；各序列第一次出现、没变化的都不命中。
func TestChangeConditionsComparePerSeries(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	sample := func(id, typ, key, name string, v float64) map[string]any {
		return ev(id, typ, id, map[string]any{"correlation_keys": []string{key}, "metrics": map[string]float64{name: v}})
	}
	e.emit("src_x", "exits",
		sample("c1", "x.level", "lv-a", "level", 1), sample("c2", "x.level", "lv-a", "level", 1), sample("c3", "x.level", "lv-a", "level", 2),
		sample("c4", "x.level", "lv-b", "level", 2),
		sample("t1", "x.temp", "t-a", "temp", 8), sample("t2", "x.temp", "t-a", "temp", 3))
	want := map[string]domain.Disposition{"c1": domain.DispositionIgnored, "c2": domain.DispositionIgnored, "c3": domain.DispositionProposed,
		"c4": domain.DispositionIgnored, "t1": domain.DispositionIgnored, "t2": domain.DispositionProposed}
	for id, d := range want {
		if got := e.disposition("src_x", id).Disposition; got != d {
			t.Errorf("%s: %s, want %s", id, got, d)
		}
	}
}

// ACT-DGA-10：提醒被静默期暂存时 apa why 仍能回放（投递状态写“held”）；静默结束放行后显示真实投递状态。
func TestWhyReplaysAlertHeldByQuietHours(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_m", "exits", metric("m0", 50))
	e.must("rule.set_mode", map[string]string{"rule": alertRule, "mode": rules.ModeAuto})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "quiet", func(p *policy.Policy) error {
			p.Disturb.Quiet = &policy.Quiet{Start: e.now.Add(-time.Hour).Format("15:04"), End: e.now.Add(30 * time.Minute).Format("15:04")}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.emit("src_m", "exits", metric("m1", 96)) // 50→96 上穿；emit 把时间拨快 2 分钟，仍在静默内
	hit := e.disposition("src_m", "m1")
	replay := func() why.Timeline {
		var tl why.Timeline
		if err := e.st.View(bg, func(tx *store.Tx) error {
			_, pol, err := policy.Current(tx)
			if err != nil {
				return err
			}
			tl, err = why.Replay(tx, pol, why.Request{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "obs"}, Level: authz.LevelObserver, ID: hit.ID})
			return err
		}); err != nil {
			t.Fatalf("replay failed while the alert is held: %v", err)
		}
		return tl
	}
	if tl := replay(); len(tl.Alerts) != 1 || tl.Alerts[0].Delivery == nil || tl.Alerts[0].Delivery.State != why.StateHeld ||
		tl.Alerts[0].Delivery.ID != attention.AlertID(hit.ID) || len(tl.Steps) == 0 {
		t.Fatalf("held replay %+v", tl.Alerts)
	}
	e.now = e.now.Add(time.Hour) // 静默结束
	if err := e.st.Update(bg, e.att.Tick); err != nil {
		t.Fatal(err)
	}
	if tl := replay(); tl.Alerts[0].Delivery == nil || tl.Alerts[0].Delivery.State == why.StateHeld || tl.Alerts[0].Delivery.Class != "reminder" {
		t.Fatalf("released replay %+v", tl.Alerts[0].Delivery)
	}
}
