package delivery

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func capPolicy(cap int, mutes ...policy.Mute) policy.Policy {
	p := policy.Default()
	p.Disturb.DailyCap = cap
	p.Disturb.Mutes = mutes
	return p
}

func (e *env) gated(pol policy.Policy, id, item string, class domain.DeliveryClass, route string, t Target) domain.Delivery {
	e.t.Helper()
	var d domain.Delivery
	e.update(func(tx *store.Tx) (err error) {
		d, _, err = e.q.CreateGated(tx, pol, domain.Delivery{ID: id, Item: item, Class: class, Route: route, Version: 1,
			Content: domain.DeliveryContent{Title: id}}, t)
		return
	})
	return d
}

// 日上限：前 N 条经渠道，之后只留 Web 未读；当天第一次超限补发恰好一条说明，之后不重复。
func TestDailyCapSuppressesAndNoticesOnce(t *testing.T) {
	e := newEnv(t, Options{})
	pol := capPolicy(2)
	var states []domain.DeliveryState
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		d := e.gated(pol, id, "i-"+id, domain.DeliveryReminder, "channel:x", Target{})
		states = append(states, d.State)
		if id == "r3" || id == "r4" {
			if d.Route != RouteWeb || d.Suppressed != SuppressedDailyCap || d.State != domain.DeliveryFallbackWeb {
				t.Fatalf("%s: route=%s suppressed=%q state=%s", id, d.Route, d.Suppressed, d.State)
			}
		}
	}
	if states[0] != domain.DeliveryPending || states[1] != domain.DeliveryPending {
		t.Fatalf("first two must be pushed: %v", states)
	}
	var pushed []string
	for c := e.claim("channel:x"); c != nil; c = e.claim("channel:x") {
		pushed = append(pushed, c.ID)
	}
	if len(pushed) != 3 || pushed[2] != "capnotice:2026-01-01" {
		t.Fatalf("channel gets 2 reminders + exactly one notice, got %v", pushed)
	}
	var unread []domain.Delivery
	e.view(func(tx *store.Tx) (err error) { unread, err = e.q.Unread(tx); return })
	if len(unread) != 2 {
		t.Fatalf("suppressed reminders must stay in Web unread, got %d", len(unread))
	}
}

// 不受限：健康类、web_only 决策、豁免的临期决策、注入、web 路由；cap=0 不限。
func TestDailyCapExemptions(t *testing.T) {
	e := newEnv(t, Options{})
	pol := capPolicy(1)
	e.gated(pol, "use", "i0", domain.DeliveryReminder, "channel:x", Target{})
	if d := e.gated(pol, "h", "i1", domain.DeliveryHealth, "channel:x", Target{}); d.Suppressed != "" {
		t.Fatal("health must not be capped")
	}
	if d := e.gated(pol, "inj", "i2", domain.DeliveryReminder, RouteInject, Target{}); d.Route != RouteInject {
		t.Fatal("injection must not be capped")
	}
	if d := e.gated(pol, "ex", "i3", domain.DeliveryDecision, "channel:x", Target{Exempt: true}); d.Suppressed != "" {
		t.Fatal("exempt near-deadline decision must not be capped")
	}
	var wo domain.Delivery
	e.update(func(tx *store.Tx) (err error) {
		wo, _, err = e.q.CreateGated(tx, pol, domain.Delivery{ID: "wo", Item: "i4", Class: domain.DeliveryDecision, Route: "channel:x", Version: 1,
			Content: domain.DeliveryContent{Title: "t", WebOnly: true}}, Target{})
		return
	})
	if wo.Suppressed != "" {
		t.Fatal("web_only decision must not be capped")
	}
	if d := e.gated(pol, "over", "i5", domain.DeliveryDecision, "channel:x", Target{}); d.Suppressed != SuppressedDailyCap {
		t.Fatal("ordinary decision over the cap must be suppressed")
	}
	unlimited := capPolicy(0)
	for _, id := range []string{"u1", "u2", "u3", "u4"} {
		if d := e.gated(unlimited, id, "j"+id, domain.DeliveryReminder, "channel:x", Target{}); d.Suppressed != "" {
			t.Fatal("cap 0 means unlimited")
		}
	}
}

// 自然日重置：次日额度恢复，前一天超出的不补推。
func TestDailyCapResetsNextDay(t *testing.T) {
	e := newEnv(t, Options{})
	pol := capPolicy(1)
	e.gated(pol, "a", "i1", domain.DeliveryReminder, "channel:x", Target{})
	if d := e.gated(pol, "b", "i2", domain.DeliveryReminder, "channel:x", Target{}); d.Suppressed != SuppressedDailyCap {
		t.Fatal("second must be suppressed")
	}
	e.now = time.Date(2026, 1, 2, 0, 1, 0, 0, time.UTC)
	if d := e.gated(pol, "c", "i3", domain.DeliveryReminder, "channel:x", Target{}); d.Suppressed != "" || d.State != domain.DeliveryPending {
		t.Fatalf("next day quota must reset: %+v", d)
	}
	if b := e.get("b"); b.State != domain.DeliveryFallbackWeb {
		t.Fatalf("yesterday's overflow must stay in Web: %s", b.State)
	}
}

// 自然日按静默时段的时区算。
func TestDailyCapDayUsesQuietZone(t *testing.T) {
	e := newEnv(t, Options{})
	pol := capPolicy(1)
	pol.Disturb.Quiet = &policy.Quiet{Start: "01:00", End: "02:00", Zone: "Asia/Shanghai"}
	e.now = time.Date(2026, 1, 1, 15, 0, 0, 0, time.UTC) // 上海 23:00
	e.gated(pol, "a", "i1", domain.DeliveryReminder, "channel:x", Target{})
	e.now = time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC) // 上海次日 00:30
	if d := e.gated(pol, "b", "i2", domain.DeliveryReminder, "channel:x", Target{}); d.Suppressed != "" {
		t.Fatal("Shanghai new day must reset the cap")
	}
}

// 静音：按来源、项目、事项；到期恢复；决策也不推；健康/唤醒与其他对象不受影响。
func TestMutes(t *testing.T) {
	e := newEnv(t, Options{})
	soon := e.now.Add(2 * time.Minute)
	pol := capPolicy(0,
		policy.Mute{Scope: policy.MuteProject, ID: "home"},
		policy.Mute{Scope: policy.MuteItem, ID: "A"},
		policy.Mute{Scope: policy.MuteSource, ID: "src1"},
		policy.Mute{Scope: policy.MuteItem, ID: "T", Until: &soon})
	cases := []struct {
		id, item string
		t        Target
		class    domain.DeliveryClass
		muted    bool
	}{
		{"p-home", "x1", Target{Project: "home"}, domain.DeliveryReminder, true},
		{"p-work", "x2", Target{Project: "work"}, domain.DeliveryReminder, false},
		{"item-A", "A", Target{Project: "work"}, domain.DeliveryReminder, true},
		{"item-B", "B", Target{Project: "work"}, domain.DeliveryReminder, false},
		{"src", "", Target{Source: "src1"}, domain.DeliveryReminder, true},
		{"dec", "x3", Target{Project: "home"}, domain.DeliveryDecision, true},
		{"health", "x4", Target{Project: "home"}, domain.DeliveryHealth, false},
		{"temp", "T", Target{}, domain.DeliveryReminder, true},
	}
	for _, c := range cases {
		item := c.item
		if item == "" {
			item = "attention:digest"
		}
		d := e.gated(pol, c.id, item, c.class, "channel:x", c.t)
		if (d.Suppressed == SuppressedMuted) != c.muted || (d.Route == RouteWeb) != c.muted {
			t.Fatalf("%s: suppressed=%q route=%s want muted=%v", c.id, d.Suppressed, d.Route, c.muted)
		}
	}
	e.now = soon.Add(time.Second)
	if d := e.gated(pol, "temp2", "T", domain.DeliveryReminder, "channel:x", Target{}); d.Suppressed != "" || d.State != domain.DeliveryPending {
		t.Fatalf("expired mute must recover: %+v", d)
	}
}

// 静音的提醒不占日上限额度；被静音压的也不触发超限说明。
func TestMutedDoesNotConsumeCap(t *testing.T) {
	e := newEnv(t, Options{})
	pol := capPolicy(1, policy.Mute{Scope: policy.MuteItem, ID: "A"})
	e.gated(pol, "m1", "A", domain.DeliveryReminder, "channel:x", Target{})
	e.gated(pol, "m2", "A", domain.DeliveryReminder, "channel:x", Target{})
	if d := e.gated(pol, "ok", "B", domain.DeliveryReminder, "channel:x", Target{}); d.Suppressed != "" {
		t.Fatal("muted deliveries must not consume the cap")
	}
	if c := e.claim("channel:x"); c == nil || c.ID != "ok" {
		t.Fatalf("only ok should be pushed, got %+v", c)
	}
	if c := e.claim("channel:x"); c != nil {
		t.Fatalf("no notice expected, got %s", c.ID)
	}
}

// 幂等：同 id 重复创建返回首次结果，不重复计数也不重复说明。
func TestCreateGatedIdempotent(t *testing.T) {
	e := newEnv(t, Options{})
	pol := capPolicy(1)
	e.gated(pol, "a", "i1", domain.DeliveryReminder, "channel:x", Target{})
	first := e.gated(pol, "b", "i2", domain.DeliveryReminder, "channel:x", Target{})
	again := e.gated(pol, "b", "i2", domain.DeliveryReminder, "channel:x", Target{})
	if first.Suppressed != again.Suppressed || again.State != domain.DeliveryFallbackWeb {
		t.Fatalf("%+v vs %+v", first, again)
	}
}

func (e *env) alertGated(pol policy.Policy, eventID, route, source string) domain.Delivery {
	e.t.Helper()
	var d domain.Delivery
	e.update(func(tx *store.Tx) (err error) {
		d, _, err = e.q.CreateGated(tx, pol, domain.Delivery{ID: "alert:" + eventID, Class: domain.DeliveryReminder, Route: route, Version: 1,
			Origin:  &domain.DeliveryOrigin{SourceEvent: eventID, Rule: "p/source_rules/r", Source: source},
			Content: domain.DeliveryContent{Title: eventID}}, Target{Source: source})
		return
	})
	return d
}

// ACT-DGA-07/12（§6.9）：没有事项的提醒带 origin 入队、可被渠道领取与回报；同样受日上限与来源静音约束，被压的只留 Web 未读。
func TestDeliveryWithoutItemKeepsOriginAndIsGated(t *testing.T) {
	e := newEnv(t, Options{})
	pol := capPolicy(1)
	d := e.alertGated(pol, "sev_1", "channel:x", "src_m")
	if d.Item != "" || d.Origin == nil || d.Origin.SourceEvent != "sev_1" || d.Origin.Rule != "p/source_rules/r" || d.State != domain.DeliveryPending {
		t.Fatalf("delivery %+v origin %+v", d, d.Origin)
	}
	c := e.claim("channel:x")
	if c == nil || c.ID != d.ID || c.Item != "" || c.Origin == nil || c.Origin.SourceEvent != "sev_1" {
		t.Fatalf("claimed %+v", c)
	}
	if got, r, err := e.ack(d.ID, 1, "msg-1"); err != nil || r != Applied || got.State != domain.DeliveryDelivered {
		t.Fatalf("ack %+v %v %v", got, r, err)
	}
	if over := e.alertGated(pol, "sev_2", "channel:x", "src_m"); over.Route != RouteWeb || over.Suppressed != SuppressedDailyCap {
		t.Fatalf("over the daily cap: %+v", over)
	}
	muted := capPolicy(0, policy.Mute{Scope: policy.MuteSource, ID: "src_noisy"})
	if m := e.alertGated(muted, "sev_3", "channel:x", "src_noisy"); m.Route != RouteWeb || m.Suppressed != SuppressedMuted {
		t.Fatalf("muted source: %+v", m)
	}
}

// 没有事项又没有 origin 的投递无处可追溯，拒绝；有事项的投递不带 origin。
func TestDeliveryWithoutItemNeedsOrigin(t *testing.T) {
	e := newEnv(t, Options{})
	content := domain.DeliveryContent{Title: "t"}
	for _, bad := range []domain.Delivery{
		{ID: "x", Class: domain.DeliveryReminder, Route: ch, Version: 1, Content: content},
		{ID: "x", Class: domain.DeliveryReminder, Route: ch, Version: 1, Content: content, Origin: &domain.DeliveryOrigin{SourceEvent: "e"}},
		{ID: "x", Item: "i", Class: domain.DeliveryReminder, Route: ch, Version: 1, Content: content, Origin: &domain.DeliveryOrigin{SourceEvent: "e", Rule: "r"}},
	} {
		e.view(func(tx *store.Tx) error {
			_, _, err := e.q.Create(tx, bad)
			wantCode(t, err, domain.CodeInvalidRequest)
			return nil
		})
	}
}
