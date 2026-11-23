package attention

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 到点提醒（§7.4 主动提醒）与定时任务产出注入主会话。时间都按本机时区；2026-11-23 是周一。

func local(day string, hm string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

// mainSession 登记一个声明支持唤醒的 harness 并选它为主 Agent：主会话的唤醒路由是 wake（旧设置迁移来的卡没有 Agent id）。
func (e *env) mainSession() {
	e.t.Helper()
	if err := executors.Register(e.x, executors.Options{Owner: "owner"}); err != nil {
		e.t.Fatal(err)
	}
	e.do(owner, "executor.register", map[string]any{"harness": "hx", "main_session": map[string]any{"wake": true},
		"headless": map[string]any{"command": []string{"true"}, "session": map[string]any{"extract": "$.session_id"}, "access": map[string]any{"edit": []string{}}}})
	e.policy(func(p *policy.Policy) { main := "legacy:hx"; p.MainAgentID = &main })
}

// proactive 改主动提醒设置（从默认值或已存的值起改）。
func (e *env) proactive(f func(*policy.Proactive)) {
	e.t.Helper()
	e.policy(func(p *policy.Policy) {
		s := p.Disturb.ProactiveSettings()
		f(&s)
		p.Disturb.Proactive = &s
	})
}

// enable 在此刻打开到点提醒（Since = 现在，与设置接口一致）。
func (e *env) enable(f ...func(*policy.Proactive)) {
	e.t.Helper()
	e.proactive(func(s *policy.Proactive) {
		now := e.now
		s.On, s.Since = true, &now
		for _, g := range f {
			g(s)
		}
	})
}

// to 把时间拨到 t 并跑一次时钟扫描。
func (e *env) to(t time.Time) {
	e.t.Helper()
	e.advance(t.Sub(e.now))
}

// wakes 领走主会话路由上现在可领的全部投递。
func (e *env) wakes() []domain.Delivery { return e.drain(delivery.RouteWake) }

func (e *env) at(t time.Time) { e.now = t }

// lines 是一条合并提醒的各行。
func lines(d domain.Delivery) []string { return strings.Split(d.Content.Summary, "\n") }

func onlyBatch(t *testing.T, ds []domain.Delivery, n int) domain.Delivery {
	t.Helper()
	if len(ds) != 1 || ds[0].Class != domain.DeliveryReminder || ds[0].Item != ScheduleItem || ds[0].Run != "" ||
		!strings.HasPrefix(ds[0].ID, schedBatchPrefix) || len(lines(ds[0])) != n {
		t.Fatalf("want one merged reminder with %d lines on the main session, got %+v", n, ds)
	}
	return ds[0]
}

func none(t *testing.T, ds []domain.Delivery, when string) {
	t.Helper()
	if len(ds) != 0 {
		t.Fatalf("%s: want nothing, got %+v", when, ds)
	}
}

// 默认关：带日期时段的事项到点也不提醒；关着时不产出任何投递。
func TestScheduleIsOffByDefault(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	e.item(map[string]any{"planned_for": "2026-11-23", "planned_slot": "09:00 - 10:00", "daily_at": nil})
	e.to(local("2026-11-23", "10:30"))
	none(t, e.wakes(), "off")
	none(t, e.unread(), "off (web)")
}

// 只有日期：当天工作开始时间（默认 09:00）提醒一次，一行写明计划日期与事项 id；送到主会话的唤醒路由，不是运行。
func TestScheduleDateOnlyAtWorkStart(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	id := e.item(map[string]any{"title": "写周报", "planned_for": "2026-11-23"})
	e.enable()
	e.to(local("2026-11-23", "08:59"))
	none(t, e.wakes(), "before work start")
	e.to(local("2026-11-23", "09:00"))
	d := onlyBatch(t, e.wakes(), 1)
	if want := "· [09:00] 开始｜计划 2026-11-23 做 写周报（" + id + "）"; d.Content.Summary != want || d.Content.Title != "到点提醒：1 件事" {
		t.Fatalf("content = %q / %q, want %q", d.Content.Title, d.Content.Summary, want)
	}
	e.to(local("2026-11-23", "12:00"))
	none(t, e.wakes(), "same day later")
}

// 只有钟点（daily_at，无日期）：每天这个钟点各一次。
func TestScheduleDailyTimeEveryDay(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	id := e.item(map[string]any{"title": "喝水", "daily_at": "10:30"})
	e.enable()
	e.to(local("2026-11-23", "10:30"))
	if d := onlyBatch(t, e.wakes(), 1); d.Content.Summary != "· [10:30] 开始｜每天 10:30 喝水（"+id+"）" {
		t.Fatalf("summary = %q", d.Content.Summary)
	}
	e.to(local("2026-11-24", "10:29"))
	none(t, e.wakes(), "next day before")
	e.to(local("2026-11-24", "10:30"))
	onlyBatch(t, e.wakes(), 1)
	e.to(local("2026-11-29", "10:31")) // 周日也提醒（每天），只补最近 24 小时内的一个点
	onlyBatch(t, e.wakes(), 1)
}

// 日期 + 钟点：精准到时段开始那一分钟；截止提醒关着时时段结束不提醒。
func TestScheduleDateAndTimeExact(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	id := e.item(map[string]any{"title": "评审", "planned_for": "2026-11-23", "planned_slot": "14:00 - 15:00"})
	e.enable(func(s *policy.Proactive) { s.End = false })
	e.to(local("2026-11-23", "13:59"))
	none(t, e.wakes(), "13:59")
	e.to(local("2026-11-23", "14:00"))
	if d := onlyBatch(t, e.wakes(), 1); d.Content.Summary != "· [14:00] 开始｜14:00 - 15:00 评审（"+id+"）" {
		t.Fatalf("summary = %q", d.Content.Summary)
	}
	e.to(local("2026-11-23", "15:00"))
	none(t, e.wakes(), "end switch off")
}

// 时段：开始与截止各一条，两个开关各管各的；截止提醒带提前量。
func TestScheduleSlotStartAndEnd(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	id := e.item(map[string]any{"title": "评审", "planned_for": "2026-11-23", "planned_slot": "14:00 - 15:00"})
	e.enable(func(s *policy.Proactive) { s.LeadMin = 10 })
	e.to(local("2026-11-23", "14:00"))
	onlyBatch(t, e.wakes(), 1)
	e.to(local("2026-11-23", "14:49"))
	none(t, e.wakes(), "before end-lead")
	e.to(local("2026-11-23", "14:50"))
	if d := onlyBatch(t, e.wakes(), 1); d.Content.Summary != "· [14:50] 截止｜时段 14:00 - 15:00 结束 评审（"+id+"）" {
		t.Fatalf("summary = %q", d.Content.Summary)
	}

	f := setup(t)
	f.mainSession()
	f.at(local("2026-11-23", "08:00"))
	f.item(map[string]any{"planned_for": "2026-11-23", "planned_slot": "14:00 - 15:00"})
	f.enable(func(s *policy.Proactive) { s.Start = false })
	f.to(local("2026-11-23", "14:30"))
	none(t, f.wakes(), "start switch off")
	f.to(local("2026-11-23", "15:00"))
	if d := onlyBatch(t, f.wakes(), 1); !strings.Contains(d.Content.Summary, "截止｜") {
		t.Fatalf("summary = %q", d.Content.Summary)
	}
}

// 落在非工作日的只有日期的计划：开了顺延（默认）就到下一个工作日 09:00；关掉顺延就在当天 09:00。
func TestScheduleNonWorkdayShifts(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-27", "20:00")) // 周五
	id := e.item(map[string]any{"title": "整理照片", "planned_for": "2026-11-28"})
	e.enable()
	e.to(local("2026-11-28", "09:01")) // 周六
	none(t, e.wakes(), "saturday")
	e.to(local("2026-11-30", "09:00")) // 周一
	if d := onlyBatch(t, e.wakes(), 1); d.Content.Summary != "· [09:00] 开始｜计划 2026-11-28 做（非工作日，顺延到今天） 整理照片（"+id+"）" {
		t.Fatalf("summary = %q", d.Content.Summary)
	}

	f := setup(t)
	f.mainSession()
	f.at(local("2026-11-27", "20:00"))
	f.item(map[string]any{"planned_for": "2026-11-28"})
	f.enable(func(s *policy.Proactive) { s.Shift = false })
	f.to(local("2026-11-28", "09:00"))
	onlyBatch(t, f.wakes(), 1)
}

// 一次 Tick 只产出一条合并消息；同一提醒点全局只推一次：重复 Tick、事项无关字段的修改、再次领取都不重推。
func TestScheduleBatchAndIdempotentPerPoint(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	a := e.item(map[string]any{"title": "甲", "planned_for": "2026-11-23"})
	b := e.item(map[string]any{"title": "乙", "planned_for": "2026-11-23"})
	e.item(map[string]any{"title": "建议", "phase": "proposed", "planned_for": "2026-11-23"}) // 建议不提醒
	e.enable()
	e.to(local("2026-11-23", "09:00"))
	d := onlyBatch(t, e.wakes(), 2)
	if l := lines(d); !strings.Contains(l[0]+l[1], a) || !strings.Contains(l[0]+l[1], b) {
		t.Fatalf("lines = %q", l)
	}
	e.do(owner, "item.update", map[string]any{"id": a, "title": "甲（改名）"})
	for _, m := range []time.Duration{time.Second, 30 * time.Second, time.Hour} {
		e.advance(m)
	}
	none(t, e.wakes(), "after repeated ticks and an unrelated edit")
	var n int64
	e.update(func(tx *store.Tx) error {
		return scan(tx, func(r *store.Rows) error { return r.Scan(&n) }, `SELECT COUNT(*) FROM delivery_delivery WHERE id LIKE 'schedbatch:%'`)
	})
	if n != 1 {
		t.Fatalf("merged deliveries = %d, want 1", n)
	}
}

// 同一事项同一原因 60 分钟内不再推：推过 14:00 之后改到 14:30 不推；再改到距 14:00 超过 60 分钟的 15:10 就推。
func TestScheduleDedupWithinSixtyMinutes(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	id := e.item(map[string]any{"planned_for": "2026-11-23", "planned_slot": "14:00 - 14:20"})
	e.enable(func(s *policy.Proactive) { s.End = false })
	e.to(local("2026-11-23", "14:00"))
	onlyBatch(t, e.wakes(), 1)
	e.do(owner, "item.update", map[string]any{"id": id, "planned_slot": "14:30 - 14:50"})
	e.to(local("2026-11-23", "14:30"))
	none(t, e.wakes(), "rescheduled 30 minutes later")
	e.do(owner, "item.update", map[string]any{"id": id, "planned_slot": "15:10 - 15:30"})
	e.to(local("2026-11-23", "15:10"))
	if d := onlyBatch(t, e.wakes(), 1); !strings.Contains(d.Content.Summary, "15:10 - 15:30") {
		t.Fatalf("summary = %q", d.Content.Summary)
	}
}

// 静默时段内到点的提醒拦着；静默结束合并成一条补发。
func TestScheduleQuietHoldsThenMerges(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.policy(func(p *policy.Policy) { p.Disturb.Quiet = &policy.Quiet{Start: "22:00", End: "08:00", Zone: "Local"} })
	e.at(local("2026-11-23", "06:00"))
	e.item(map[string]any{"title": "跑步", "daily_at": "07:00"})
	e.item(map[string]any{"title": "早会", "planned_for": "2026-11-23", "planned_slot": "07:30 - 07:45"})
	e.enable(func(s *policy.Proactive) { s.End = false })
	e.to(local("2026-11-23", "07:00"))
	e.to(local("2026-11-23", "07:30"))
	none(t, e.wakes(), "quiet hours")
	e.to(local("2026-11-23", "08:00"))
	d := onlyBatch(t, e.wakes(), 2)
	if l := lines(d); !strings.HasPrefix(l[0], "· [07:00] 开始｜每天 07:00 跑步") || !strings.HasPrefix(l[1], "· [07:30] 开始｜07:30 - 07:45 早会") {
		t.Fatalf("lines = %q", l)
	}
}

// 每日上限：条数到了的攒到下一个工作日的工作开始时间合并推一条；消息条数到了同样攒着。
func TestScheduleDailyCapDefersToNextWorkStart(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-27", "08:00")) // 周五
	for _, n := range []string{"一", "二", "三"} {
		e.item(map[string]any{"title": n, "planned_for": "2026-11-27", "planned_slot": "10:00 - 10:30"})
	}
	late := e.item(map[string]any{"title": "四", "planned_for": "2026-11-27", "planned_slot": "11:00 - 11:30"})
	e.enable(func(s *policy.Proactive) { s.End, s.MaxItems, s.MaxBatches = false, 2, 5 })
	e.to(local("2026-11-27", "10:00"))
	onlyBatch(t, e.wakes(), 2)
	e.to(local("2026-11-27", "11:00"))
	none(t, e.wakes(), "item cap reached")
	e.to(local("2026-11-29", "09:00")) // 周日不是工作日
	none(t, e.wakes(), "sunday")
	e.to(local("2026-11-30", "09:00"))
	if d := onlyBatch(t, e.wakes(), 2); !strings.Contains(d.Content.Summary, late) || !strings.Contains(d.Content.Summary, "11-27 10:00") {
		t.Fatalf("deferred batch = %q", d.Content.Summary)
	}

	f := setup(t)
	f.mainSession()
	f.at(local("2026-11-23", "08:00"))
	f.item(map[string]any{"planned_for": "2026-11-23", "planned_slot": "10:00 - 10:30"})
	f.item(map[string]any{"planned_for": "2026-11-23", "planned_slot": "11:00 - 11:30"})
	f.enable(func(s *policy.Proactive) { s.End, s.MaxItems, s.MaxBatches = false, 0, 1 })
	f.to(local("2026-11-23", "10:00"))
	onlyBatch(t, f.wakes(), 1)
	f.to(local("2026-11-23", "11:00"))
	none(t, f.wakes(), "message cap reached")
	f.to(local("2026-11-24", "09:00"))
	onlyBatch(t, f.wakes(), 1)
}

// 改期后旧提醒作废：静默中拦着的旧提醒点随改期丢掉，按新时间提醒；还没被领取的合并消息随成员完成作废，
// 其余成员重新合成一条，完成的那件不再出现。
func TestScheduleRescheduleVoidsOldReminder(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.policy(func(p *policy.Policy) { p.Disturb.Quiet = &policy.Quiet{Start: "22:00", End: "08:00", Zone: "Local"} })
	e.at(local("2026-11-23", "06:00"))
	id := e.item(map[string]any{"title": "早会", "planned_for": "2026-11-23", "planned_slot": "07:30 - 07:45"})
	e.enable(func(s *policy.Proactive) { s.End = false })
	e.to(local("2026-11-23", "07:30")) // 拦在静默里
	e.do(owner, "item.update", map[string]any{"id": id, "planned_slot": "09:30 - 09:45"})
	e.to(local("2026-11-23", "08:00"))
	none(t, e.wakes(), "old point after reschedule")
	e.to(local("2026-11-23", "09:30"))
	if d := onlyBatch(t, e.wakes(), 1); !strings.Contains(d.Content.Summary, "09:30 - 09:45") {
		t.Fatalf("summary = %q", d.Content.Summary)
	}

	f := setup(t)
	f.mainSession()
	f.at(local("2026-11-23", "08:00"))
	a := f.item(map[string]any{"title": "甲", "planned_for": "2026-11-23"})
	b := f.item(map[string]any{"title": "乙", "planned_for": "2026-11-23"})
	f.enable()
	f.to(local("2026-11-23", "09:00")) // 合并消息已入队、还没被领取
	f.do(owner, "item.transition", map[string]any{"id": a, "to": "done", "reason": "做完了"})
	f.advance(30 * time.Second)
	d := onlyBatch(t, f.wakes(), 1)
	if strings.Contains(d.Content.Summary, a) || !strings.Contains(d.Content.Summary, b) {
		t.Fatalf("after completing one member: %q", d.Content.Summary)
	}
}

// 注入失败不丢：失败回报后按退避重新可领，同一条再次领取到（不重新生成，不静默丢）。
func TestScheduleFailedInjectionIsRetried(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.at(local("2026-11-23", "08:00"))
	e.item(map[string]any{"planned_for": "2026-11-23"})
	e.enable()
	e.to(local("2026-11-23", "09:00"))
	d := onlyBatch(t, e.wakes(), 1)
	e.update(func(tx *store.Tx) error { _, _, err := e.q.Fail(tx, d.ID, d.Version, "injection refused"); return err })
	if got := e.get(d.ID); got.State != domain.DeliveryPending || got.NextAttemptAt == nil {
		t.Fatalf("after fail: %+v", got)
	}
	e.advance(time.Minute)
	if again := e.wakes(); len(again) != 1 || again[0].ID != d.ID {
		t.Fatalf("retry = %+v", again)
	}
}

// 没有能唤醒的主会话时，到点提醒落 Web 未读，不选无头运行。
func TestScheduleWithoutMainSessionGoesToWeb(t *testing.T) {
	e := setup(t)
	e.at(local("2026-11-23", "08:00"))
	e.item(map[string]any{"planned_for": "2026-11-23"})
	e.enable()
	e.to(local("2026-11-23", "09:00"))
	none(t, e.wakes(), "no main session")
	if u := e.unread(); len(u) != 1 || u[0].Item != ScheduleItem || u[0].Route != delivery.RouteWeb {
		t.Fatalf("unread = %+v", u)
	}
}

// 预览：按当前数据假设现在开启，接下来 24 小时的条数、批数、下一批时刻与条数；静默内的点并到静默结束。
func TestSchedulePreviewCountsNextDay(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.policy(func(p *policy.Policy) { p.Disturb.Quiet = &policy.Quiet{Start: "22:00", End: "08:00", Zone: "Local"} })
	e.at(local("2026-11-23", "17:00"))
	e.item(map[string]any{"planned_for": "2026-11-24"})                                  // 明天 09:00
	e.item(map[string]any{"planned_for": "2026-11-24", "planned_slot": "14:00 - 15:00"}) // 明天 14:00 与 15:00（都在 24 小时内）
	e.item(map[string]any{"planned_for": "2026-11-24", "planned_slot": "18:00 - 19:00"}) // 超出 24 小时的不算
	e.item(map[string]any{"daily_at": "07:00"})                                          // 明早 07:00 在静默里，并到 08:00
	e.item(map[string]any{"planned_for": "2026-11-20"})                                  // 过去的点不预览、不补推
	var pv Preview
	e.update(func(tx *store.Tx) (err error) {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		pv, err = SchedulePreview(tx, pol, pol.Disturb.ProactiveSettings(), e.now)
		return err
	})
	if pv.Total != 4 || pv.Batches != 4 || pv.NextAt == nil || !pv.NextAt.Equal(local("2026-11-24", "08:00")) || pv.NextCount != 1 ||
		pv.Route != delivery.RouteWake || pv.Blocked != "" {
		t.Fatalf("preview = %+v", pv)
	}
}

// ---- 定时任务产出注入主会话 ----

// task 建一条实例（类型 pack），返回它的 origin.source。
func (e *env) task(id, pack string) map[string]any {
	e.t.Helper()
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO instances_instance (id, pack, project, params, enabled) VALUES (?, ?, '', '{}', 1)`, id, pack)
		return err
	})
	return map[string]any{"source": domain.OriginInstancePrefix + id}
}

// 简报默认注入主会话、只走这一条（原路由上没有）；内容标明简报、正文按长度给全文；同一份只注入一次。
func TestBriefGoesToMainSessionOnlyOnce(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.policy(func(p *policy.Policy) { p.Disturb.Routes["brief"] = "chan" })
	id := e.briefDone(map[string]any{"origin": e.task("ins_b", "brief"),
		"spec": map[string]any{"period": "p", "sections": []map[string]any{{"title": "今天", "body": "- 写周报"}}}})
	d := e.wakes()
	if len(d) != 1 || d[0].ID != delivery.BriefID(id) || d[0].Class != domain.DeliveryBrief || d[0].Run != "" ||
		d[0].Content.Title != "【简报】每日简报" || d[0].Content.Summary != "## 今天\n- 写周报" {
		t.Fatalf("main session = %+v", d)
	}
	if c := e.claim("channel:chan"); c != nil {
		t.Fatalf("brief must not also go to the channel: %+v", c)
	}
	e.do(owner, "item.update", map[string]any{"id": id, "title": "改名"})
	e.advance(time.Minute)
	none(t, e.wakes(), "same brief again")
}

// 复盘标明复盘；长正文给每节摘要与全文位置。
func TestReviewLabelAndLongBodySummary(t *testing.T) {
	e := setup(t)
	e.mainSession()
	long := strings.Repeat("- 一行很长的复盘内容，说明本周完成的事情与原因。\n", 80)
	id := e.briefDone(map[string]any{"title": "review 2026-11-27", "origin": e.task("ins_r", "review"),
		"spec": map[string]any{"period": "p", "sections": []map[string]any{{"title": "完成了什么", "body": long}}}})
	d := e.wakes()
	if len(d) != 1 || d[0].Content.Title != "【复盘】review 2026-11-27" {
		t.Fatalf("review = %+v", d)
	}
	if want := "## 完成了什么\n- 一行很长的复盘内容，说明本周完成的事情与原因。\n- 一行很长的复盘内容，说明本周完成的事情与原因。\n……（本节共 80 行）\n\n（正文较长，以上是摘要；全文在 APA 的这件事项里：/items/" + id + "）"; d[0].Content.Summary != want {
		t.Fatalf("summary = %q", d[0].Content.Summary)
	}
}

// 一键关掉：简报不再注入，照旧按 routes.brief 走（渠道），内容是原来的标题与摘要；按实例的设置优先于按类型。
func TestTaskPushSwitchOffKeepsOldRoute(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.policy(func(p *policy.Policy) { p.Disturb.Routes["brief"] = "chan" })
	e.proactive(func(s *policy.Proactive) { s.TaskPush = map[string]bool{"brief": false} })
	id := e.briefDone(map[string]any{"origin": e.task("ins_b", "brief")})
	none(t, e.wakes(), "push off")
	if c := e.claim("channel:chan"); c == nil || c.ID != delivery.BriefID(id) || c.Content.Title != "每日简报" {
		t.Fatalf("channel = %+v", c)
	}
	e.proactive(func(s *policy.Proactive) { s.InstancePush = map[string]bool{"ins_c": true} })
	id2 := e.briefDone(map[string]any{"origin": e.task("ins_c", "brief")})
	if d := e.wakes(); len(d) != 1 || d[0].ID != delivery.BriefID(id2) {
		t.Fatalf("instance override = %+v", d)
	}
}

// 例行任务默认只记在 APA，不推；打开后完成时注入一次。
func TestRoutineOutputPushIsOptIn(t *testing.T) {
	e := setup(t)
	e.mainSession()
	src := e.task("ins_x", "routine")
	a := e.item(map[string]any{"kind": "task", "title": "例行一", "origin": src, "spec": map[string]any{"result": "已备份"}})
	e.do(owner, "item.transition", map[string]any{"id": a, "to": "done", "reason": "ran"})
	e.advance(time.Minute)
	none(t, e.wakes(), "routine default off")
	none(t, e.unread(), "routine default off (web)")
	e.proactive(func(s *policy.Proactive) { s.TaskPush = map[string]bool{"routine": true} })
	b := e.item(map[string]any{"kind": "task", "title": "例行二", "origin": src, "spec": map[string]any{"result": "已备份"}})
	e.do(owner, "item.transition", map[string]any{"id": b, "to": "done", "reason": "ran"})
	d := e.wakes()
	if len(d) != 1 || d[0].ID != TaskID(b) || d[0].Content.Title != "【例行任务】例行二" || d[0].Content.Summary != "已备份" {
		t.Fatalf("routine = %+v", d)
	}
}

// 主会话不能唤醒时简报退回原路由（不丢、不走无头运行）。
func TestBriefFallsBackWhenMainCannotWake(t *testing.T) {
	e := setup(t)
	e.policy(func(p *policy.Policy) { p.Disturb.Routes["brief"] = "chan" })
	id := e.briefDone(map[string]any{"origin": e.task("ins_b", "brief")})
	if c := e.claim("channel:chan"); c == nil || c.ID != delivery.BriefID(id) {
		t.Fatalf("channel = %+v", c)
	}
}

// 长正文摘要末尾的全文位置是传入的绝对链接，不是事项 id 也不是运行目录。
func TestTaskContentLongBodyPointsToItemURL(t *testing.T) {
	long := strings.Repeat("- 一行很长的内容，说明事情与原因，凑够长度。\n", 80)
	spec, _ := json.Marshal(map[string]any{"sections": []map[string]any{{"title": "今天", "body": long}}})
	c := TaskContent(domain.Item{ID: "itm_x", Title: "t", Spec: spec}, "brief", "https://apa.example/items/itm_x")
	if want := "\n\n（正文较长，以上是摘要；全文在 APA 的这件事项里：https://apa.example/items/itm_x）"; !strings.HasSuffix(c.Summary, want) {
		t.Fatalf("summary tail = %q", c.Summary[len(c.Summary)-80:])
	}
}
