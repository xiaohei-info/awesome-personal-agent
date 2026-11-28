package policy

import (
	"errors"
	"reflect"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	nextChat = Target{Agent: AgentNextChat, User: UserOff}
	off      = Target{Agent: AgentOff, User: UserOff}
	toDef    = Target{Agent: AgentOff, User: UserDefault}
)

func mainElse(e string) Target { return Target{Agent: AgentMain, User: UserOff, Else: e} }

func wantRows(reminder, decisionish Target, extra map[string]Target) map[string]Target {
	m := map[string]Target{RowDecision: decisionish, RowScheduled: mainElse("")}
	for _, r := range reminderRows {
		m[r] = reminder
	}
	for _, r := range []string{RowSourceFailing, RowRunFailed, RowNewProposals} {
		m[r] = decisionish
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// 出厂矩阵就是 §4.2 的"出厂"列：决策与各类提醒下次对话时带上，到点与简报、复盘的结果推主会话，例行任务只记录，系统告警只记录。
func TestFactoryDeliveryIsTheDesignedFactoryColumn(t *testing.T) {
	got := FactoryDelivery()
	want := Delivery{DefaultChannel: "", Rows: wantRows(nextChat, nextChat, map[string]Target{
		RowTaskResult + "brief": mainElse(AgentNextChat), RowTaskResult + "review": mainElse(AgentNextChat),
		RowTaskResult + "routine": off, RowTaskResult + TaskOther: nextChat,
		RowTaskFailed + "brief": mainElse(AgentNextChat), RowTaskFailed + "review": mainElse(AgentNextChat),
		RowTaskFailed + "routine": nextChat, RowTaskFailed + TaskOther: nextChat,
		RowHealth: off, RowSourceResult: mainElse(AgentNextChat),
		RowDelegationResult: {Agent: AgentMain, User: UserDefault, Else: AgentNextChat},
	})}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("factory =\n%+v\nwant\n%+v", got, want)
	}
	if len(got.Rows) != len(FixedRows) {
		t.Fatalf("factory has %d rows, FixedRows %d", len(got.Rows), len(FixedRows))
	}
}

// onlineDisturb 是线上策略的形状：决策与提醒推同一条渠道，简报下次对话时带上，健康只记录，没有 push，到点提醒开着。
func onlineDisturb() Disturb {
	d := Default().Disturb
	d.Routes = map[string]string{"decision": "channel:ch-1", "reminder": "channel:ch-1", "brief": "inject", "health": "web", "wake": "web"}
	pr := DefaultProactive()
	pr.On = true
	d.Proactive = &pr
	return d
}

// 线上形状换算出 §4.2 的"线上迁移"列，逐格精确比对。
func TestDeriveOnlineShapeIsTheMigrationColumn(t *testing.T) {
	got := DeriveDelivery(onlineDisturb(), nil)
	want := Delivery{DefaultChannel: "channel:ch-1", Rows: wantRows(toDef, toDef, map[string]Target{
		RowTaskResult + "brief": mainElse(AgentNextChat), RowTaskResult + "review": mainElse(AgentNextChat),
		RowTaskResult + "routine": off, RowTaskResult + TaskOther: nextChat,
		RowTaskFailed + "brief": mainElse(UserDefault), RowTaskFailed + "review": mainElse(UserDefault),
		RowTaskFailed + "routine": toDef, RowTaskFailed + TaskOther: toDef,
		RowHealth: off, RowSourceResult: mainElse(AgentNextChat),
		RowDelegationResult: {Agent: AgentMain, User: UserDefault, Else: AgentNextChat},
	})}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("online =\n%+v\nwant\n%+v", got, want)
	}
}

// 「类别:优先级」覆盖进 by_priority；推送类单设的渠道、按实例覆盖、未带前缀的旧渠道名都按旧版本的选路由规整换算；
// 默认渠道取出现最多的渠道。
func TestDeriveOverridesAndLegacyForms(t *testing.T) {
	d := Default().Disturb
	d.Routes = map[string]string{"decision": "tg", "decision:urgent": "channel:bark", "reminder": "channel:tg", "brief": "web", "brief:high": "channel:bark", "health": "inject"}
	d.Push = map[string]PushRule{PushTemplateRun: {On: true, Route: "channel:bark"}, PushRunFailed: {On: true}}
	pr := DefaultProactive()
	pr.TaskPush = map[string]bool{"brief": false, "routine": true}
	pr.InstancePush = map[string]bool{"in_r": true, "in_b": false, "in_gone": true}
	d.Proactive = &pr
	m := DeriveDelivery(d, map[string]string{"in_r": "routine", "in_b": "brief"})
	tg, bark := Target{Agent: AgentOff, User: "channel:tg"}, Target{Agent: AgentOff, User: UserDefault} // bark 出现 3 次、tg 2 次
	if m.DefaultChannel != "channel:bark" {
		t.Fatalf("default = %q", m.DefaultChannel)
	}
	for k, want := range map[string]Target{
		RowDecision:               {Agent: AgentOff, User: "channel:tg", ByPriority: map[string]Target{"urgent": bark}},
		RowOverdue:                tg,
		RowTaskResult + "brief":   {Agent: AgentOff, User: UserOff, ByPriority: map[string]Target{"high": bark}},
		RowTaskResult + "review":  {Agent: AgentMain, User: UserOff, ByPriority: map[string]Target{"high": mainElse(UserDefault)}},
		RowTaskResult + "routine": mainElse(""),
		RowTaskFailed + "brief":   bark, // 单设了渠道就用它，不管推不推主会话
		RowTaskFailed + TaskOther: bark,
		RowRunFailed:              tg,
		RowHealth:                 nextChat,
		RowInstance + "in_r":      mainElse(""),
		RowInstance + "in_b":      {Agent: AgentOff, User: UserOff, ByPriority: map[string]Target{"high": bark}},
		RowInstanceFail + "in_b":  bark,
	} {
		if got := m.Rows[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %+v, want %+v", k, got, want)
		}
	}
	if _, ok := m.Rows[RowInstance+"in_gone"]; ok {
		t.Error("an instance that no longer exists must not get a row")
	}
	if got, _ := m.Lookup(domain.PriorityUrgent, RowDecision); !reflect.DeepEqual(got, bark) {
		t.Errorf("urgent decision = %+v", got)
	}
	if got, _ := m.Lookup(domain.PriorityLow, RowDecision); !reflect.DeepEqual(got, tg) {
		t.Errorf("low decision = %+v", got)
	}
}

// 存过的矩阵缺行时取出厂值；实例行没有时跟类型；什么都没有时 ok 为假。
func TestLookupFallsBackToFactoryThenType(t *testing.T) {
	m := Delivery{Rows: map[string]Target{RowTaskResult + "brief": toDef}}
	if got, ok := m.Lookup("", RowInstance+"x", RowTaskResult+"brief"); !ok || !reflect.DeepEqual(got, toDef) {
		t.Fatalf("type row = %+v %v", got, ok)
	}
	if got, ok := m.Lookup("", RowStale); !ok || !reflect.DeepEqual(got, nextChat) {
		t.Fatalf("missing row = %+v %v, want factory", got, ok)
	}
	if _, ok := m.Lookup("", RowInstance+"x"); ok {
		t.Fatal("no row anywhere must report !ok")
	}
}

func TestDeliveryValidation(t *testing.T) {
	bad := []Delivery{
		{DefaultChannel: "tg"},
		{Rows: map[string]Target{"nope": off}},
		{Rows: map[string]Target{RowInstance: off}},
		{Rows: map[string]Target{RowDecision: {Agent: "wake", User: UserOff}}},
		{Rows: map[string]Target{RowDecision: {Agent: AgentOff, User: "inject"}}},
		{Rows: map[string]Target{RowDecision: {Agent: AgentNextChat, User: UserOff, Else: UserDefault}}},
		{Rows: map[string]Target{RowDecision: {Agent: AgentMain, User: UserOff, Else: UserOff}}},
		{Rows: map[string]Target{RowDecision: {Agent: AgentOff, User: UserOff, ByPriority: map[string]Target{"x": off}}}},
		{Rows: map[string]Target{RowDecision: {Agent: AgentOff, User: UserOff, ByPriority: map[string]Target{"low": {Agent: AgentOff, User: UserOff, ByPriority: map[string]Target{"low": off}}}}}},
	}
	for i, m := range bad {
		p := Default()
		p.Disturb.Delivery = &m
		if _, err := normalize(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: %+v accepted", i, m)
		}
	}
	good := Delivery{DefaultChannel: "channel:a", Rows: map[string]Target{RowDecision: {Agent: AgentMain, User: "channel:b", Else: AgentNextChat},
		RowInstanceFail + "i1": toDef, RowStale: {Agent: AgentOff, User: "wake", ByPriority: map[string]Target{"low": nextChat}}}}
	p := Default()
	p.Disturb.Delivery = &good
	if _, err := normalize(p); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenEqual(t *testing.T) {
	a := onlineDisturb()
	b := onlineDisturb()
	b.Push = map[string]PushRule{PushSourceFailing: {On: false}} // 开关不是冻结键
	b.Quiet = &Quiet{Start: "23:00", End: "08:00"}
	if !FrozenEqual(a, b) {
		t.Fatal("non-frozen changes must compare equal")
	}
	for name, f := range map[string]func(*Disturb){
		"routes":     func(d *Disturb) { d.Routes["brief"] = "web" },
		"push route": func(d *Disturb) { d.Push = map[string]PushRule{PushRunFailed: {On: true, Route: "channel:x"}} },
		"task_push": func(d *Disturb) {
			pr := d.ProactiveSettings()
			pr.TaskPush = map[string]bool{"brief": false}
			d.Proactive = &pr
		},
		"instance": func(d *Disturb) {
			pr := d.ProactiveSettings()
			pr.InstancePush = map[string]bool{"i": true}
			d.Proactive = &pr
		},
	} {
		c := onlineDisturb()
		f(&c)
		if FrozenEqual(a, c) {
			t.Errorf("%s change must break FrozenEqual", name)
		}
	}
}

// 采用建议值：线上形状只改两处（14 天没动静、简报与复盘的没出来），旧键原样；只写一次；出厂设置换算值与建议值相同，不写、不冻结。
func TestSeedDeliveryAdoptsSuggestedOnceAndOnlyWhenItDiffers(t *testing.T) {
	s := open(t)
	sys := domain.Actor{Type: domain.ActorSystem, Principal: "kernel:defaults", Via: domain.ViaClock}
	seed := func() (wrote bool) {
		t.Helper()
		if err := s.Update(ctx, func(tx *store.Tx) (err error) { _, wrote, err = SeedDelivery(tx, sys, nil); return }); err != nil {
			t.Fatal(err)
		}
		return
	}
	if seed() {
		t.Fatal("a fresh install must not get a matrix")
	}
	must(t, s, func(p *Policy) error { p.BackgroundRunTimeoutSec = 1000; return nil })
	if seed() {
		t.Fatal("factory routes: suggested equals derived, nothing to write")
	}
	must(t, s, func(p *Policy) error { p.Disturb = onlineDisturb(); return nil })
	if !seed() {
		t.Fatal("online shape must be seeded")
	}
	var p Policy
	if err := s.View(ctx, func(tx *store.Tx) (err error) { _, p, err = Current(tx); return }); err != nil {
		t.Fatal(err)
	}
	if p.Disturb.Delivery == nil || !reflect.DeepEqual(p.Disturb.Routes, onlineDisturb().Routes) {
		t.Fatalf("after seed: %+v", p.Disturb)
	}
	derived := DeriveDelivery(onlineDisturb(), nil)
	var diff []string
	for k, v := range p.Disturb.Delivery.Rows {
		if !reflect.DeepEqual(v, derived.Rows[k]) {
			diff = append(diff, k)
		}
	}
	got := map[string]Target{}
	for _, k := range diff {
		got[k] = p.Disturb.Delivery.Rows[k]
	}
	want := map[string]Target{RowStale: nextChat, RowTaskFailed + "brief": toDef, RowTaskFailed + "review": toDef}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggested differs from derived in %v, want exactly %v", got, want)
	}
	if seed() {
		t.Fatal("seed must run once")
	}
}
