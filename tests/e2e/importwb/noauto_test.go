//go:build e2e

package importwb

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// subscribers 是 OMAC-NOTES §5 / GT-390 的"现场"读法清单：凡是读事件日志的自动化订阅方，各自落在哪些表里。
// 导入之后它们的行数都不得因导入事件而增加；新增订阅方时同步加一行（设计 §6.14 第 17 条）。
var subscribers = []struct {
	name   string
	tables []string
}{
	{"触达（注入记录；投递表按行断言，见 noAutomation）", []string{"delivery_injection"}},
	{"规则（触发记录）", []string{"rules_fire"}},
	{"摘要起草（已摘要的事件与起草）", []string{"summaries_digested", "attention_draft", "attention_held", "attention_done"}},
	{"跟进与久未推进（待跟进与轮次状态）", []string{"followup_pending", "followup_state", "followup_resume"}},
	{"判断与外调", []string{"judgments_j", "outcall_judgment", "outcall_run"}},
	{"委派与后台运行", []string{"delegation_dispatch", "delegation_link", "background_run", "agenttask_result", "agenttask_session", "resume_run"}},
	{"Agent 自检与健康统计", []string{"direct_result", "direct_health", "direct_judge_health"}},
	{"链式触发", []string{"chain_cursor"}},
}

// 导入之后允许新增的事件：import.completed（对外可见的收尾）、本测试自己的 Web 登录，
// 以及 T-0002 过期截止的真实触发（见 doc.go：item.deadline_passed 与它晚触发一天带来的 health.alert）。
func allowedLive(typ string) bool {
	return slices.Contains([]string{"import.completed", "auth.login", "item.deadline_passed", "health.alert"}, typ)
}

// noAutomation 断言导入没有触发任何订阅方：与导入前的行数基线比较。
func (f *fx) noAutomation(base map[string]int, seq0 int64, t2 string) {
	f.t.Helper()
	after := f.tableCounts()
	for _, s := range subscribers {
		for _, tb := range s.tables {
			if after[tb] != base[tb] {
				f.t.Errorf("订阅方「%s」的表 %s 因导入新增了行: %d -> %d", s.name, tb, base[tb], after[tb])
			}
		}
	}
	// 事件日志：导入之后的所有普通事件只能是放行名单里的类型；不许有规则观察与规则触发。
	for _, row := range f.queryStrings(`SELECT type || '|' || object FROM eventlog_event WHERE seq > ? AND import_batch = ''`, seq0) {
		typ, obj, _ := strings.Cut(row, "|")
		if !allowedLive(typ) {
			f.t.Errorf("导入之后出现了不该有的普通事件 %s（对象 %s）", typ, obj)
		}
	}
	for _, typ := range []string{"rule.observed", "rule.skipped"} {
		if n := f.eventCount(`seq > ? AND type = ?`, seq0, typ); n != 0 {
			f.t.Errorf("导入新增了 %d 条 %s", n, typ)
		}
	}
	// 触达：只许有 T-0002 过期截止那次真实触发带来的健康告警兜底投递。
	for _, row := range f.queryStrings(`SELECT item || '|' || class FROM delivery_delivery`) {
		if row != t2+"|health" {
			f.t.Errorf("触达里出现了与导入无关的投递 %q（只许 T-0002 的健康告警）", row)
		}
	}
	if pushed := decodeToday(f).Pushed; pushed != 0 {
		f.t.Errorf("今天已推送 %d 条，导入不应推送任何东西", pushed)
	}
}

type todayView struct {
	Pushed int `json:"pushed"`
}

func decodeToday(f *fx) todayView {
	f.t.Helper()
	return decode[todayView](f.t, f.web("GET", "/v1/deliveries/today", "", 200))
}

// TestFlowImportNoAutomation 是 flow-import-no-automation（验收文档 ACT-INA-01..05，OMAC-NOTES §3 I5）：
// 导入写下的历史事件只进账本与折叠，不触发规则、提醒、跟进、摘要、注入、统计、简报与对外事件流；导入的未完成事项从导入时刻起算。
func TestFlowImportNoAutomation(t *testing.T) {
	f := setup(t)
	f.declareBriefHarness()
	const src = "acc-imp"
	ctx := func() string { return f.apa(f.DEL, 0, "context", "--session", "ina").Stdout }
	var base map[string]int
	var seq0 int64
	var batch, t1, t2, t3 string
	var importedAt time.Time
	var timers0 []string

	// ACT-IMP-01 的基线：导入前各订阅方的行数、最大序号与 Timer。
	ctx() // 先取一次注入增量，游标落在导入之前
	base, seq0 = f.tableCounts(), f.maxSeq()
	timers0 = f.queryStrings(`SELECT id FROM clock_timer ORDER BY id`)
	open3 := fmt.Sprintf(`{"type":"item","external_id":"T-0003","item":{"kind":"task","title":"导入的未完成事项"},"history":[{"at":%q,"event":"created","phase":"open"},{"at":%q,"event":"progress","text":"旧进展","on":%q}]}`+"\n",
		rfc(f.now().Add(-30*24*time.Hour).In(cst).Truncate(time.Second)), rfc(f.now().Add(-29*24*time.Hour).In(cst).Truncate(time.Second)),
		f.now().Add(-29*24*time.Hour).In(cst).Format("2006-01-02"))

	f.step("ACT-INA-01 导入前后各订阅方没有新增", func(t *testing.T) {
		out := f.mustImport(src, "", f.plan()+open3)
		batch, t1, t2, t3 = out.Batch, out.Results[2].Object, out.Results[3].Object, out.Results[4].Object
		if out.Created != 4 || out.Existing != 1 {
			t.Fatalf("导入结果 = %+v", out)
		}
		importedAt = f.show(t2).Item.Imported.At
		// 已过期的截止由时钟扫描照常触发一次；等它出现之后再比较，放行名单才是确定的。
		f.until("T-0002 的 item.deadline_passed", func() bool { return len(f.audit("type=item.deadline_passed&object="+t2)) == 1 })
		f.noAutomation(base, seq0, t2)

		// 注入增量：同一会话的下一次读取不带导入写入的变更；对照：普通事件照常带出。
		if got := ctx(); strings.Contains(got, t1) || strings.Contains(got, t2) || strings.Contains(got, t3) || !strings.Contains(got, `"items":[]`) {
			t.Errorf("注入增量带出了导入的事项: %s", got)
		}
		live := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "现场对照", "--evidence", "对照").Stdout).ID
		if got := ctx(); !strings.Contains(got, live) {
			t.Errorf("对照失败：普通事件应出现在注入增量里: %s", got)
		}
		// 对外事件流：除 import.completed 与现场事件外没有导入事件。
		feed := f.api(f.OBS, "GET", fmt.Sprintf("/v1/events?cursor=%d&limit=500", seq0), 200)
		for _, typ := range []string{"item.created", "item.progress_added", "item.phase_changed"} {
			if strings.Contains(feed, `"type":"`+typ+`"`) && !strings.Contains(feed, live) {
				t.Errorf("对外事件流出现了 %s", typ)
			}
		}
		if strings.Contains(feed, t1) || strings.Contains(feed, t3) || strings.Count(feed, `"type":"import.completed"`) != 1 {
			t.Errorf("对外事件流应只有一条 import.completed、不含导入事项: %s", feed)
		}
		// Timer：只多出 T-0002 的截止 Timer（已触发即移除）与第一轮跟进 Timer。
		var added []string
		for _, id := range f.queryStrings(`SELECT id FROM clock_timer ORDER BY id`) {
			if !slices.Contains(timers0, id) {
				added = append(added, id)
			}
		}
		if !reflect.DeepEqual(added, []string{"followup_" + t2}) {
			t.Errorf("新增的 Timer = %v，期望只有 followup_%s（截止 Timer 已触发移除）", added, t2)
		}
	})

	f.step("ACT-INA-02 导入事件在账本、审计与回放里都读得到", func(t *testing.T) {
		batchEvs := f.audit("imported=" + batch)
		if len(batchEvs) != 1+9+2+2 { // Alpha 项目一条（acc/home 绑定不写事件）、T-0001 九条、T-0002 两条、T-0003 两条
			t.Fatalf("批次事件 %d 条，期望 14", len(batchEvs))
		}
		for _, e := range batchEvs {
			if e.Imported != batch || e.RecordedAt == "" {
				t.Errorf("事件缺导入标记或写入时刻: %+v", e)
			}
		}
		all := f.audit(fmt.Sprintf("after=%d", seq0))
		seen := map[int64]bool{}
		for _, e := range all {
			seen[e.Seq] = true
		}
		for _, e := range batchEvs {
			if !seen[e.Seq] {
				t.Errorf("不带筛选的审计缺少导入事件 seq=%d", e.Seq)
			}
		}
		why := decodeWhy(t, f.apa(f.OBS, 0, "why", t1).Stdout)
		if len(why) != 9 {
			t.Errorf("apa why 回放出 %d 步，期望 9（完整时间线）", len(why))
		}
		for _, s := range why {
			if s.Imported != batch {
				t.Errorf("回放里的步骤缺导入批次: %+v", s)
			}
		}
	})

	f.step("ACT-INA-03 已过期截止照常触发一次，跟进从导入时刻起算", func(t *testing.T) {
		dl := f.audit("type=item.deadline_passed&object=" + t2)
		if len(dl) != 1 || dl[0].Imported != "" {
			t.Errorf("item.deadline_passed 应恰好一条且是普通事件: %+v", dl)
		}
		var at int64
		if err := f.ledger().QueryRow(`SELECT at FROM clock_timer WHERE id = ?`, "followup_"+t2).Scan(&at); err != nil {
			t.Fatal(err)
		}
		const round = 72 * time.Hour // 能力包 follow-up 的第一轮间隔
		if got := time.Unix(0, at); got.Sub(importedAt.Add(round)).Abs() > 2*time.Second {
			t.Errorf("跟进 Timer 在 %s，期望导入时刻 %s 加 %v", got, importedAt, round)
		}
		reminded := func() int { return len(f.audit("type=followup.reminded&object=" + t2)) }
		time.Sleep(3 * time.Second) // 从 10-18 起算的话早就该提醒了
		if reminded() != 0 {
			t.Fatalf("导入后立刻就提醒了：跟进按原始时刻起算")
		}
		f.advance(int64((round - time.Hour).Seconds())) // 第一轮间隔差一小时：还不该提醒
		time.Sleep(3 * time.Second)
		if reminded() != 0 {
			t.Fatalf("第一轮间隔未到就提醒了")
		}
		f.advance(int64((2 * time.Hour).Seconds()))
		f.until("第一轮跟进提醒", func() bool { return reminded() == 1 })
		if n := len(f.audit("type=item.deadline_passed&object=" + t2)); n != 1 {
			t.Errorf("截止触发了 %d 次", n)
		}
	})

	f.step("ACT-INA-04 未完成的导入事项不立刻进久未推进", func(t *testing.T) {
		// T-0003 是 open 的任务，最后一次变更在 29 天前（早已超过 14 天的阈值），但从导入时刻起算，所以导入后三天仍不在久未推进里。
		staleIDs := func() []string {
			var ids []string
			for _, it := range decode[struct {
				Stale []itemView `json:"stale"`
			}](t, f.api(f.OBS, "GET", "/v1/today", 200)).Stale {
				ids = append(ids, it.ID)
			}
			return ids
		}
		if got := staleIDs(); slices.Contains(got, t3) || slices.Contains(got, t2) || slices.Contains(got, t1) {
			t.Errorf("导入的事项不该进久未推进: %v", got)
		}
		since := time.Since(importedAt) + time.Duration(f.offset)*time.Second
		f.advance(int64((14*24*time.Hour + time.Hour - since).Seconds())) // 导入时刻起满阈值（14 天）
		if got := staleIDs(); !slices.Contains(got, t3) {
			t.Errorf("满阈值之后 T-0003 应进久未推进: %v", got)
		}
		if got := staleIDs(); slices.Contains(got, t1) {
			t.Errorf("已完成的 T-0001 不进久未推进: %v", got)
		}
	})

	f.step("ACT-INA-05 当天活动与简报取材不计导入事件", func(t *testing.T) {
		// 一批 at 落在简报窗口（过去 24 小时）内的导入事件：不过滤的话它们一定会进当天活动。
		now := f.now().In(cst).Truncate(time.Second)
		const note = "IMPORTED-NOTE-不进当天活动"
		recent := fmt.Sprintf(`{"type":"item","external_id":"R-1","item":{"kind":"task","title":"刚导入的完成事项"},"history":[{"at":%q,"event":"created","phase":"open"},{"at":%q,"event":"progress","text":%q,"on":%q},{"at":%q,"event":"transition","to":"done"}]}`+"\n",
			rfc(now.Add(-3*time.Hour)), rfc(now.Add(-2*time.Hour)), note, now.Add(-2*time.Hour).Format("2006-01-02"), rfc(now.Add(-time.Hour)))
		f.mustImport("acc-imp-recent", "", recent)
		inst := f.addBriefInstance()
		// 对照：现场写的进展必须出现在当天活动里，否则"导入的没进去"可能只是简报取材坏了。
		live := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "现场事项", "--phase", "open", "--evidence", "对照").Stdout).ID
		f.apa(f.DEL, 0, "item", "progress", live, "--text", "LIVE-NOTE-现场进展", "--evidence", "对照")
		mats := f.runBrief(inst)
		act := mats["activity"]
		if !strings.Contains(act, "LIVE-NOTE-现场进展") {
			t.Fatalf("对照失败：现场进展应出现在 activity 段: %s", act)
		}
		for _, bad := range []string{note, "刚导入的完成事项", "旧进展", "进展一", "导入的已完成事项"} {
			if strings.Contains(act, bad) {
				t.Errorf("导入的历史事件进了 activity（含 %q）: %s", bad, act)
			}
		}
		if n := f.tableCounts()["rules_fire"]; n != base["rules_fire"] {
			t.Errorf("这批导入触发了规则: rules_fire %d -> %d", base["rules_fire"], n)
		}
	})
}

type whyStep struct {
	Seq      int64  `json:"seq"`
	Type     string `json:"type"`
	Imported string `json:"imported"`
}

func decodeWhy(t *testing.T, s string) []whyStep {
	t.Helper()
	return decode[struct {
		Steps []whyStep `json:"steps"`
	}](t, s).Steps
}
