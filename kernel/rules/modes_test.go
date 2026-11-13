package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const reminderPack = `name: cat
version: 1.0.0
description: categories
clock_rules:
  - id: follow
    type: follow_up
    schedule: 72h
  - id: stale
    type: stale
    schedule: 24h
  - id: stale-off
    type: stale
    schedule: 24h
    mode: off
  - id: stale-obs
    type: stale
    schedule: 24h
    mode: observe
  - id: sweep
    type: recur
    schedule: 24h
    do:
      - action: create_item
        kind: task
event_rules:
  - id: timer
    when:
      event: item.phase_changed
      to_phase: open
    do:
      - action: create_timer
        after: 1m
`

// ACT-RULE-08/09/10：类别由加载器推定。提醒类省略 mode 开箱 auto（可显式写 off/observe），
// 其余（含只建 Timer 的事件规则、建事项的时钟规则）省略 mode 即 observe；全新安装不产生任何 mode_changed。
func TestInitialModeByRuleClass(t *testing.T) {
	e := newEnv(t, map[string]string{"cat/pack.yaml": reminderPack}, nil)
	for k, want := range map[string]string{
		"cat/clock_rules/follow": ModeAuto, "cat/clock_rules/stale": ModeAuto,
		"cat/clock_rules/stale-off": ModeOff, "cat/clock_rules/stale-obs": ModeObserve,
		"cat/clock_rules/sweep": ModeObserve, "cat/event_rules/timer": ModeObserve,
	} {
		if got := e.mode(k); got != want {
			t.Errorf("%s initial mode = %s, want %s", k, got, want)
		}
	}
	if n := len(e.ofType(domain.EvRuleModeChanged)); n != 0 {
		t.Errorf("fresh install wrote %d rule.mode_changed", n)
	}
}

// ACT-RULE-12：提醒类规则本人关掉后，内容变化保持本人的选择，不回退 observe、不产生告警；动作类仍回 observe（见 TestContentChangeFallsBackToObserve）。
func TestReminderRuleKeepsOwnersChoiceOnContentChange(t *testing.T) {
	e := newEnv(t, map[string]string{"cat/pack.yaml": reminderPack}, nil)
	if err := e.setMode(owner, "cat/clock_rules/stale", ModeOff); err != nil {
		t.Fatal(err)
	}
	e.pack(map[string]string{"cat/pack.yaml": strings.Replace(reminderPack, "schedule: 24h\n  - id: stale-off", "schedule: 48h\n  - id: stale-off", 1)})
	e.pack(map[string]string{"cat/pack.yaml": strings.ReplaceAll(strings.Replace(reminderPack, "schedule: 72h", "schedule: 70h", 1), "schedule: 24h\n  - id: stale-off", "schedule: 48h\n  - id: stale-off")})
	if e.mode("cat/clock_rules/stale") != ModeOff || e.mode("cat/clock_rules/follow") != ModeAuto {
		t.Fatalf("stale=%s follow=%s", e.mode("cat/clock_rules/stale"), e.mode("cat/clock_rules/follow"))
	}
	if n := len(e.ofType(domain.EvHealthAlert)); n != 0 {
		t.Fatalf("reminder content change raised %d health alerts", n)
	}
	if evs := e.ofType(domain.EvRuleModeChanged); len(evs) != 1 || evs[0].Actor.Principal != "owner" {
		t.Fatalf("mode_changed = %+v", evs)
	}
}

// legacy 把一条规则改回 v0.1 账本里的样子：初始 observe、旧内容 hash、没有任何 mode_changed。
func (e *env) legacy(rule string) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE rules_state SET mode = 'observe', hash = 'v01' WHERE key = ?`, rule)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// ACT-UPG-10 / ACT-RULE-14/15/16：升级时未动过的提醒类规则切到 auto，各记一条 system 的 rule.mode_changed，
// 合并成一条健康类说明；本人切换过的、包里显式写的、动作类的不动；再次对账不重复。
func TestUpgradeSwitchesUntouchedReminderRulesToAuto(t *testing.T) {
	e := newEnv(t, map[string]string{"cat/pack.yaml": reminderPack}, nil)
	const follow, stale, off, obs, sweep = "cat/clock_rules/follow", "cat/clock_rules/stale", "cat/clock_rules/stale-off", "cat/clock_rules/stale-obs", "cat/event_rules/timer"
	if err := e.setMode(owner, off, ModeObserve); err != nil { // 本人切换过：observe 是他选的
		t.Fatal(err)
	}
	if err := e.setMode(owner, stale, ModeOff); err != nil { // 本人关掉
		t.Fatal(err)
	}
	for _, k := range []string{follow, off, stale, obs} {
		e.legacy(k)
	}
	e.legacy(sweep)
	base := len(e.events())

	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{follow: ModeAuto, off: ModeObserve, stale: ModeObserve, obs: ModeObserve, sweep: ModeObserve} {
		if got := e.mode(k); got != want {
			t.Errorf("%s after upgrade = %s, want %s", k, got, want)
		}
	}
	var changed, alerts []domain.Event
	for _, ev := range e.events()[base:] {
		switch ev.Type {
		case domain.EvRuleModeChanged:
			changed = append(changed, ev)
		case domain.EvHealthAlert:
			alerts = append(alerts, ev)
		}
	}
	// stale、off 本人动过：只更新 hash，不留新 Event。
	if len(changed) != 1 || changed[0].Object != "rule:"+follow || changed[0].Actor.Type != domain.ActorSystem {
		t.Fatalf("mode_changed after upgrade = %+v", changed)
	}
	var p map[string]string
	_ = json.Unmarshal(changed[0].Payload, &p)
	if p["from"] != ModeObserve || p["to"] != ModeAuto {
		t.Fatalf("payload = %v", p)
	}
	if len(alerts) != 1 || !strings.Contains(string(alerts[0].Payload), follow) {
		t.Fatalf("want one merged health note, got %+v", alerts)
	}
	n := len(e.events())
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	if len(e.events()) != n {
		t.Fatal("second reconcile must not switch again")
	}
}

const autoAcceptNoMode = `name: acc
version: 1.0.0
description: tri
source_rules:
  - id: tri-auto
    accept: auto
    match: {type: tri.auto}
    then: proposed
`

// ACT-TRI-04：accept: auto 的来源规则包内不写 mode 即可加载，初始 observe；Web 切 auto 后才生效。
func TestAcceptAutoSourceRuleWithoutModeStartsObserve(t *testing.T) {
	e := newEnv(t, map[string]string{"acc/pack.yaml": autoAcceptNoMode}, nil)
	k := SourceKey("acc", "tri-auto")
	if e.mode(k) != ModeObserve {
		t.Fatalf("mode = %s", e.mode(k))
	}
	if err := e.setMode(owner, k, ModeAuto); err != nil || e.mode(k) != ModeAuto {
		t.Fatalf("owner switch: %v / %s", err, e.mode(k))
	}
}

type fakePacks []*packs.Pack

func (f fakePacks) List() []*packs.Pack { return f }

// ACT-DGA-05/06：then: alert 的来源规则登记为动作类，初始 observe；delegate 切不了，Web 上的本人可以切 auto。
func TestAlertSourceRuleStartsObserveAndIsWebOnly(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.eng.o.Packs = fakePacks{{Name: "dga", SourceRules: []packs.SourceRule{{ID: "metric-over-limit", Then: "alert"}},
		RuleHashes: map[string]string{"source_rules/metric-over-limit": "h1"}}}
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	k := SourceKey("dga", "metric-over-limit")
	if e.mode(k) != ModeObserve {
		t.Fatalf("alert rule initial mode = %s", e.mode(k))
	}
	if err := e.setMode(delegate, k, ModeAuto); err == nil || e.mode(k) != ModeObserve {
		t.Fatalf("delegate switch err=%v mode=%s", err, e.mode(k))
	}
	if err := e.setMode(owner, k, ModeAuto); err != nil || e.mode(k) != ModeAuto {
		t.Fatalf("owner switch: %v / %s", err, e.mode(k))
	}
}

// ACT-UPG-10：包被拒绝（如带 v0.1 字段的用户覆盖包）时，本人写在包里的 mode 看不到，升级不替他把 observe 切成 auto；
// 同一次对账里别的包的提醒类规则照常切换。
func TestUpgradeHoldsReminderRulesOfRejectedPack(t *testing.T) {
	e := newEnv(t, map[string]string{"cat/pack.yaml": reminderPack, "oth/pack.yaml": strings.NewReplacer("name: cat", "name: oth").Replace(reminderPack)}, nil)
	const held, other = "cat/clock_rules/follow", "oth/clock_rules/follow"
	e.legacy(held)
	e.legacy(other)
	bad := strings.Replace(reminderPack, "type: follow_up", "type: no_such_type", 1)
	if err := os.WriteFile(filepath.Join(e.data, "cat", "pack.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if rej, err := e.loads.Load(); err != nil || len(rej) != 1 {
		t.Fatalf("want one rejection, got %v %v", rej, err)
	}
	base := len(e.events())
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.mode(held); got != ModeObserve {
		t.Errorf("rule of a rejected pack = %s, want observe", got)
	}
	if got := e.mode(other); got != ModeAuto {
		t.Errorf("rule of a healthy pack = %s, want auto", got)
	}
	for _, ev := range e.events()[base:] {
		if ev.Type == domain.EvRuleModeChanged && ev.Object == "rule:"+held {
			t.Errorf("unexpected mode change for held rule: %+v", ev)
		}
	}
}
