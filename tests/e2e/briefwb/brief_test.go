//go:build e2e

package briefwb

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/srcstub"
)

const importedNote = "IMPORTED-NOTE-不进当天活动"

// accPack 是验收用能力包：日历事件只入账（ignore）；digest-src 的事件进知会。prop-src 没有规则，走初筛与起草，
// 这样建议才带推荐、理由与把握低标记。
const accPack = `name: acc-wb
version: 1.0.0
description: 简报资料验收夹具的来源规则
source_rules:
  - {id: cal-ignore, match: {source_type: calendar}, then: ignore}
  - {id: dg, match: {source_type: digest-src}, then: digest}
`

const (
	defaultInstruction = "default-instruction-marker"
	amInstruction      = "只写日程与待决策"
)

// ---- 日历夹具：两个来源，C1 改过期、C3 最新版取消、C4 在窗口外 ----

type cal struct {
	id, typ, subject, key string
	start                 time.Time
	end, location         string
}

func (c cal) line() map[string]any {
	return map[string]any{"id": c.id, "type": c.typ, "subject": c.subject, "occurred_at": c.start.Format(time.RFC3339),
		"correlation_keys": []string{c.key}, "link": "https://example.invalid/cal/" + c.id,
		"headers": map[string]any{"end": c.end, "location": c.location}}
}

type calFixture struct{ c1v1, c1v2, c3v1, c3v2, c4, c2 cal }

func (f *fx) calendars() calFixture {
	h := func(days, hour int) string { return f.at(days, hour).Format(time.RFC3339) }
	return calFixture{
		c1v1: cal{"c1-v1", "event.created", "周会 C1", "k-c1", f.at(1, 10), h(1, 11), "会议室甲"},
		c1v2: cal{"c1-v2", "event.updated", "周会 C1", "k-c1", f.at(2, 15), h(2, 16), "会议室乙"}, // 改期后的最新版
		c3v1: cal{"c3-v1", "event.created", "取消的会 C3", "k-c3", f.at(1, 9), h(1, 10), "会议室丙"},
		c3v2: cal{"c3-v2", "event.cancelled", "取消的会 C3", "k-c3", f.at(1, 9), h(1, 10), "会议室丙"},
		c4:   cal{"c4-v1", "event.created", "第五天的会 C4", "k-c4", f.at(5, 10), h(5, 11), "会议室丁"},
		c2:   cal{"c2-v1", "event.created", "家庭聚餐 C2", "k-c2", f.at(1, 18), h(1, 20), "餐厅"},
	}
}

const (
	calWork = "src_acc_cal_work"
	calHome = "src_acc_cal_home"
)

// ---- 知会与建议的来源事件 ----

func mailLine(id, subject string, occurred time.Time, body string) map[string]any {
	return map[string]any{"id": id, "type": "mail.received", "subject": subject, "summary": "摘要 " + subject, "detail": body,
		"sender": "news@example.com", "occurred_at": occurred.UTC().Format(time.RFC3339)}
}

var digestSources = []struct{ id, external string }{
	{"src_acc_dg_full", "full"}, {"src_acc_dg_ex", "excerpt"}, {"src_acc_dg_none", "none"}, {"src_acc_dg_unset", ""},
}

// dgBody 是知会事件的正文：30000 个字符，各事件开头不同。
func dgBody(id string) string { return srcstub.Body(id, 30000) }

// ---- 解出各段 ----

type rawRef struct {
	Event    string `json:"event"`
	File     string `json:"file"`
	Material struct {
		Scope    string `json:"scope"`
		Mode     string `json:"mode"`
		Sent     int    `json:"sent_chars"`
		Original int    `json:"original_chars"`
	} `json:"material"`
}

type digestEntry struct {
	Event  string   `json:"event"`
	Source string   `json:"source"`
	Title  string   `json:"title"`
	Raw    []rawRef `json:"raw"`
}

func digestOf(t *testing.T, r runResult) []digestEntry {
	t.Helper()
	return decode[struct {
		Digest []digestEntry `json:"digest"`
	}](t, r.Mats["digest"]).Digest
}

type scheduleEntry struct {
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      string    `json:"end"`
	Location string    `json:"location"`
	Source   string    `json:"source"`
}

func scheduleOf(t *testing.T, r runResult) []scheduleEntry {
	t.Helper()
	if !strings.Contains(r.Mats["schedule"], `"schedule":[`) {
		t.Fatalf("schedule 段必须是数组: %s", r.Mats["schedule"])
	}
	return decode[struct {
		Schedule []scheduleEntry `json:"schedule"`
	}](t, r.Mats["schedule"]).Schedule
}

func titles[T any](xs []T, title func(T) string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, title(x))
	}
	return out
}

func sameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	g, w := map[string]int{}, map[string]int{}
	for _, s := range got {
		g[s]++
	}
	for _, s := range want {
		w[s]++
	}
	if fmt.Sprint(g) != fmt.Sprint(w) {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

type window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

func windowOf(t *testing.T, r runResult) window {
	t.Helper()
	return decode[window](t, string(r.Item.Spec))
}

// fixtureIDs 是账本夹具里要在断言里点名的事项。
type fixtureIDs struct {
	workDone, openDone, importedDone string
	staleItem, followUp, echoItem    string
	decNormal, decSecond             string
}

// 建议：三条来自 prop-src（外发范围全文）的事件，经初筛与起草成为建议；B 是把握低的。
var proposalFixture = []struct {
	mark, title, recommend, reason string
	p                              float64 // 分类端点的初筛 p_yes
	low                            bool
}{
	{"prop-a", "建议 A", "accept", "对方明确提出请求", 0.90, false},
	{"prop-b", "建议 B", "dismiss", "可能无关", 0.35, true}, // 分类端点不确定、对话端点资料不足 → 保守默认 → 把握低
	{"prop-c", "建议 C", "accept", "本周内要用", 0.90, false},
}

type proposalOut struct {
	Title         string   `json:"title"`
	Sources       string   `json:"sources"`
	Recommend     string   `json:"recommend"`
	Reason        string   `json:"reason"`
	LowConfidence bool     `json:"low_confidence"`
	Raw           []rawRef `json:"raw"`
}

func (f *fx) clsPYes(mark string, p float64, delayMS int) {
	f.t.Helper()
	f.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: mark}, PYes: &p, DelayMS: delayMS})
}

// reminded 是事项上 followup.reminded 事件的条数（跟进提醒已记）。
func (f *fx) reminded(id string) int {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/audit/events?type=followup.reminded&object="+id, "", f.OBS)
	if r.Code != 200 {
		f.t.Fatalf("GET /v1/audit/events: HTTP %d %s", r.Code, r.Body)
	}
	return len(decode[struct {
		Events []json.RawMessage `json:"events"`
	}](f.t, r.Body).Events)
}

// askItem 建一件 open 事项并以执行者身份写入决策请求；初筛按 Cls 上的配置作答。
func (f *fx) askItem(mark string) string {
	f.t.Helper()
	id := f.addItem("--title", "待决策 "+mark, "--phase", "open", "--assignee", "work-host.cli", "--project", f.Projects["acc-work"])
	f.apa(f.WRK, 0, "ask", id, "--question", "选哪个方案 "+mark, "--option", "A=方案A", "--option", "B=方案B")
	return id
}

// buildLedger 造账本夹具（测试时钟推进 15 天，之后的"今天"都以内核时间为准）：
// 建议流 3 条（一条把握低）、待决策 2 条、今天与明天到期与已过期各一、
// 回音转回 open、跟进到期仍在等、久未推进各一、当天完成的事项与进展、一批导入的事件。
func (f *fx) buildLedger() fixtureIDs {
	f.t.Helper()
	var ids fixtureIDs
	work, open := f.Projects["acc-work"], f.Projects["acc-open"]
	f.WireEndpoints() // 分类端点 CLS_STUB 与生成端点 MODEL_STUB，初筛、起草与 web_only 初筛都走它们
	f.writePack("acc-wb", accPack)
	f.web("PUT", "/v1/settings/policy", `{"default_harness":"`+briefHarness+`"}`, 200)

	// 15 天前就在的两件事：一件没有任何进展（久未推进），一件在等外部回复（跟进提醒到期）。
	ids.staleItem = f.addItem("--title", "久未推进的事项", "--phase", "open", "--project", work)
	ids.followUp = f.addItem("--title", "跟进到期仍在等的事项", "--phase", "open", "--assignee", "work-host.cli", "--project", work)
	f.apa(f.WRK, 0, "item", "transition", ids.followUp, "--to", "waiting", "--waiting-on", "external:装修公司报价", "--reason", "等报价")
	f.advance(15 * 86400)
	f.until("跟进提醒已记且仍在等", func() bool { return f.reminded(ids.followUp) >= 1 })
	if it := f.show(ids.followUp); it.Phase != "waiting" {
		f.t.Fatalf("跟进到期的事项应仍在 waiting: %s", it.Phase)
	}

	// 知会：四个外发范围不同的来源各两条正文 30000 字符的事件。
	for _, s := range digestSources {
		var lines []map[string]any
		for i := 1; i <= 2; i++ {
			id := fmt.Sprintf("%s-%d", strings.ReplaceAll(s.id, "_", "-"), i)
			lines = append(lines, mailLine(id, "知会 "+id, f.now().Add(-time.Duration(10-i)*time.Minute), dgBody(id)))
		}
		f.addSource(s.id, strings.ReplaceAll(s.id, "_", "-"), "digest-src", s.external, lines)
		f.waitEvents(s.id, 2, "digest")
	}

	// 今天、明天到期与已过期各一。
	today, tomorrow, yesterday := f.at(0, 0).Format("2006-01-02"), f.at(1, 0).Format("2006-01-02"), f.at(-1, 0).Format("2006-01-02")
	f.addItem("--title", "今天到期", "--phase", "open", "--project", work, "--deadline", today)
	f.addItem("--title", "明天到期", "--phase", "open", "--project", open, "--deadline", tomorrow)
	f.addItem("--title", "已过期", "--phase", "open", "--project", work, "--deadline", yesterday)
	// 当天完成的事项与写下的进展（两个项目）。
	ids.workDone = f.addItem("--title", "今天完成 W", "--phase", "open", "--project", work)
	f.apa(f.DEL, 0, "item", "progress", ids.workDone, "--text", "进展 W1")
	f.apa(f.DEL, 0, "item", "transition", ids.workDone, "--to", "done", "--reason", "完成")
	ids.openDone = f.addItem("--title", "今天完成 O", "--phase", "open", "--project", open)
	f.apa(f.DEL, 0, "item", "progress", ids.openDone, "--text", "进展 O1")
	f.apa(f.DEL, 0, "item", "transition", ids.openDone, "--to", "done", "--reason", "完成")
	// 一批导入的历史事件：给已完成事项的一条进展，与把另一件事记为完成。
	ids.importedDone = f.addItem("--title", "导入时记为完成的事项", "--phase", "open", "--project", open)
	f.seedImported(ids.workDone, ids.importedDone)

	// 等外部回音的事项收到回音，回到 open（关联键登记在事项上，回音事件带同一个键）。
	ids.echoItem = f.addItem("--title", "等房东确认的事项", "--phase", "open", "--project", open)
	f.apa(f.DEL, 0, "item", "transition", ids.echoItem, "--to", "waiting", "--waiting-on", "external:房东", "--reason", "等房东回复")
	f.apa(f.DEL, 0, "item", "relate", ids.echoItem, "--key", "acc-echo-thread")
	echo := map[string]any{"id": "acc-echo-1", "type": "acc.reply", "subject": "周三上午可以", "occurred_at": f.now().UTC().Format(time.RFC3339),
		"correlation_keys": []string{"acc-echo-thread"}, "sender": "landlord@example.com"}
	f.addSource("src_acc_echo", "acc-echo", "echo-src", "full", []map[string]any{echo})
	f.until("回音使事项回到 open", func() bool { return f.show(ids.echoItem).Phase == "open" })

	// 建议：先配好分类端点与生成端点的作答，再让来源出事件。
	var pl []map[string]any
	for _, p := range proposalFixture {
		f.clsPYes(p.mark, p.p, 0)
		if p.low {
			f.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: p.mark}, Answer: "no", Sufficient: new(bool)})
		}
		b, _ := json.Marshal(map[string]any{"title": p.title, "kind": "task", "recommend": p.recommend, "reason": p.reason})
		f.ModelStub.When(p.mark, modelstub.Reply{Content: string(b)})
		pl = append(pl, mailLine(p.mark+"-1", p.mark+" 来信", f.now().Add(-20*time.Minute), "建议正文 "+p.mark))
	}
	f.addSource("src_acc_prop", "acc-prop", "prop-src", "full", pl)
	f.until("三条建议进入建议流", func() bool { return len(f.proposedTitles()) == 3 })

	// 待决策两条（2026-11-20 起高风险类别与初筛取消，问题一个口径，简报全部列出）。
	ids.decNormal, ids.decSecond = f.askItem("DEC-NORMAL"), f.askItem("DEC-SECOND")
	return ids
}

func (f *fx) proposedTitles() []string {
	f.t.Helper()
	var out []string
	for _, it := range decode[struct {
		Items []item `json:"items"`
	}](f.t, f.apa(f.OBS, 0, "item", "list", "--phase", "proposed").Stdout).Items {
		out = append(out, it.Title)
	}
	return out
}

// TestFlowBriefMaterial 对应验收 flow-brief-material（OMAC-NOTES §3 B3）：固定的账本夹具 + STUB_HARNESS 读资料文件并写回。
func TestFlowBriefMaterial(t *testing.T) {
	f := setup(t)
	var ids fixtureIDs
	var am, pm1 runResult
	var amInst, pmInst string
	var wantProposals []string
	for _, p := range proposalFixture {
		wantProposals = append(wantProposals, p.title)
	}

	f.step("ACT-BRF-01 准备账本夹具与两个简报实例", func(t *testing.T) {
		ids = f.buildLedger()
		if n := len(f.briefs()); n != 0 {
			t.Fatalf("夹具里不应已有简报: %d", n)
		}
		sameSet(t, "建议流", f.proposedTitles(), wantProposals)
		amInst = f.addInstance(0, "instruction="+amInstruction)
		pmInst = f.addInstance(0, "schedule_days=3")
		if packs := f.web("GET", "/v1/packs", "", 200); !strings.Contains(packs, "default_instruction") {
			t.Fatalf("简报包应带 default_instruction: %.300s", packs)
		}
	})

	f.step("ACT-BRF-02、03 brief-am：各段与夹具一致；没有日历来源时日程为空；知会进先跑的这一份", func(t *testing.T) {
		am = f.runBrief(amInst, "done")
		if got := scheduleOf(t, am); len(got) != 0 {
			t.Errorf("没有日历来源时日程应为空数组: %+v", got)
		}
		// 新增建议：3 条、pending_total 3，各带推荐、理由与把握低标记（只有 B 为真）、来源名。
		props := decode[struct {
			Proposals []proposalOut `json:"proposals"`
			Total     int           `json:"pending_total"`
		}](t, am.Mats["proposals"])
		if props.Total != 3 || len(props.Proposals) != 3 {
			t.Fatalf("proposals = %+v", props)
		}
		got := map[string]proposalOut{}
		for _, p := range props.Proposals {
			got[p.Title] = p
		}
		for _, w := range proposalFixture {
			p, ok := got[w.title]
			if !ok || p.Recommend != w.recommend || p.Reason != w.reason || p.LowConfidence != w.low || p.Sources != "acc-prop" {
				t.Errorf("建议 %s = %+v，期望 recommend=%s reason=%s low=%v", w.title, p, w.recommend, w.reason, w.low)
			}
		}
		// 待决策：两条都列出（高风险类别取消，2026-11-20）。
		dec := decode[struct {
			Decisions []struct {
				ID       string `json:"id"`
				Question string `json:"question"`
			} `json:"decisions"`
		}](t, am.Mats["decisions"]).Decisions
		gotDec := map[string]bool{}
		for _, d := range dec {
			gotDec[d.ID] = true
		}
		if len(dec) != 2 || !gotDec[ids.decNormal] || !gotDec[ids.decSecond] {
			t.Errorf("decisions = %+v，期望恰好两条待决策", dec)
		}
		// 到期与等待：今天、明天、已过期、回音、跟进到期、久未推进。
		due := decode[struct {
			Due []struct{ Title, Reason string } `json:"due_waiting"`
		}](t, am.Mats["due_waiting"]).Due
		var gotDue []string
		for _, d := range due {
			gotDue = append(gotDue, d.Title+"/"+d.Reason)
		}
		sameSet(t, "due_waiting", gotDue, []string{"今天到期/due_today", "明天到期/due_tomorrow", "已过期/overdue",
			"等房东确认的事项/echo", "跟进到期仍在等的事项/follow_up_due", "久未推进的事项/stale"})
		// 当天活动：按项目分组（组名是项目名，给 Agent 直接用），导入事件不计。
		act := decode[struct {
			Activity []struct {
				Project string `json:"project"`
				Entries []struct{ Title, Kind, Text string }
			} `json:"activity"`
		}](t, am.Mats["activity"]).Activity
		byProject := map[string][]string{}
		for _, g := range act {
			for _, e := range g.Entries {
				byProject[g.Project] = append(byProject[g.Project], e.Kind+":"+e.Title+":"+e.Text)
			}
		}
		sameSet(t, "acc-work 的活动", byProject["acc-work"], []string{"progress:今天完成 W:进展 W1", "done:今天完成 W:"})
		sameSet(t, "acc-open 的活动", byProject["acc-open"], []string{"progress:今天完成 O:进展 O1", "done:今天完成 O:"})
		if len(byProject) != 2 {
			t.Errorf("活动应只分两个项目（导入的事项不计）: %v", byProject)
		}
		if strings.Contains(am.Mats["activity"], importedNote) || strings.Contains(am.Mats["activity"], "导入时记为完成的事项") {
			t.Errorf("导入的事件不应进当天活动: %s", am.Mats["activity"])
		}
		// 知会：四个来源各两条，进先跑的这一份。
		if d := digestOf(t, am); len(d) != 8 {
			t.Errorf("brief-am 的知会应有 8 条: %d", len(d))
		}
	})

	f.step("ACT-BRF-02 brief-pm：两个日历来源，改期只取最新版、取消的与窗口外的不列", func(t *testing.T) {
		c := f.calendars()
		f.addSource(calWork, "工作日历", "calendar", "", []map[string]any{c.c1v1.line(), c.c1v2.line(), c.c3v1.line(), c.c3v2.line(), c.c4.line()})
		f.addSource(calHome, "家庭日历", "calendar", "", []map[string]any{c.c2.line()})
		f.waitEvents(calWork, 5, "ignored")
		f.waitEvents(calHome, 1, "ignored")
		pm1 = f.runBrief(pmInst, "done")
		got := scheduleOf(t, pm1)
		if len(got) != 2 {
			t.Fatalf("schedule = %+v，期望 C2 与改期后的 C1", got)
		}
		// 按开始时间排：C2（明天 18:00）在前，C1 取改期后的后天 15:00。
		c2e, c1e := got[0], got[1]
		if c2e.Title != c.c2.subject || c2e.Source != "家庭日历" || !c2e.Start.Equal(c.c2.start) || c2e.Location != "餐厅" || c2e.End != c.c2.end {
			t.Errorf("C2 = %+v", c2e)
		}
		if c1e.Title != c.c1v2.subject || c1e.Source != "工作日历" || !c1e.Start.Equal(c.c1v2.start) || c1e.Location != "会议室乙" || c1e.End != c.c1v2.end {
			t.Errorf("C1 应取改期后的最新版: %+v", c1e)
		}
		for _, e := range got {
			if strings.Contains(e.Title, "C3") || strings.Contains(e.Title, "C4") {
				t.Errorf("已取消的与窗口外的不应出现: %+v", e)
			}
		}
		if d := digestOf(t, pm1); len(d) != 0 || !strings.Contains(pm1.Mats["digest"], `"digest":[]`) {
			t.Errorf("同一条知会只进先跑的一份，brief-pm 的知会应为空数组: %s", pm1.Mats["digest"])
		}
	})

	f.step("ACT-BRF-04 两个实例各有自己的窗口、内容要求与所属实例", func(t *testing.T) {
		amW, pmW := windowOf(t, am), windowOf(t, pm1)
		// 实例的第一份从本次到期之前的上一个调度节拍起（验收实例的定时是每年 1 月 1 日 03:00，主机时区）。
		prevTick := func(until time.Time) time.Time {
			at := time.Date(until.In(time.Local).Year(), 1, 1, 3, 0, 0, 0, time.Local)
			if !at.Before(until) {
				at = at.AddDate(-1, 0, 0)
			}
			return at
		}
		if want := prevTick(amW.Until); !amW.Since.Equal(want) {
			t.Errorf("brief-am 是该实例第一份，窗口应从上一个调度节拍 %v 起: %v", want, amW.Since)
		}
		if want := prevTick(pmW.Until); !pmW.Since.Equal(want) {
			t.Errorf("brief-pm 的第一份同样从自己定时的上一个节拍起（不借用 brief-am 的）: %v ≠ %v", pmW.Since, want)
		}
		pm2 := f.runBrief(pmInst, "done")
		if w := windowOf(t, pm2); !w.Since.Equal(pmW.Until) {
			t.Errorf("brief-pm 第二份的 since 应等于它上一份的 until：%v ≠ %v", w.Since, pmW.Until)
		}
		if am.Item.Origin == nil || am.Item.Origin.Source != "instance:"+amInst || pm1.Item.Origin == nil || pm1.Item.Origin.Source != "instance:"+pmInst {
			t.Errorf("origin 各记所属实例: am=%+v pm=%+v", am.Item.Origin, pm1.Item.Origin)
		}
		if got := strings.TrimSpace(am.Files["instruction.txt"]); got != amInstruction {
			t.Errorf("brief-am 的 instruction 文件应是本人写的要求: %q", got)
		}
		packs := decode[struct {
			Packs []struct {
				Name               string `json:"name"`
				DefaultInstruction string `json:"default_instruction"`
			} `json:"packs"`
		}](t, f.web("GET", "/v1/packs", "", 200)).Packs
		var want string
		for _, p := range packs {
			if p.Name == "brief" {
				want = p.DefaultInstruction
			}
		}
		if want == "" || strings.TrimSpace(pm1.Files["instruction.txt"]) != strings.TrimSpace(want) {
			t.Errorf("brief-pm 的 instruction 文件应是包的 default_instruction：%q ≠ %q", pm1.Files["instruction.txt"], want)
		}
	})

	f.step("ACT-BRF-03 窗口天数：schedule_days=7 时 C4 出现；15 被加载校验拒绝", func(t *testing.T) {
		seven := f.addInstance(0, "schedule_days=7")
		got := scheduleOf(t, f.runBrief(seven, "done"))
		if len(got) != 3 || got[2].Title != f.calendars().c4.subject {
			t.Errorf("7 天窗口应多出 C4: %+v", got)
		}
		r := f.Apa(f.DEL, "", "instance", "add", "brief", "--param", "schedule=0 3 1 1 *", "--param", "schedule_days=15")
		if r.Code != 2 || !strings.Contains(r.Stderr, "schedule_days") {
			t.Errorf("schedule_days=15 应以退出码 2 被拒并指出 schedule_days: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
	})

	f.step("ACT-BRF-05 来源原文文件不超过外发范围与字符预算，且只读", func(t *testing.T) {
		checkRaw(t, f, am)
	})

	f.step("ACT-BRF-06 合计超过 400000 字符时从最旧的条目起只留摘要", func(t *testing.T) {
		var lines []map[string]any
		base := f.now().Add(-time.Hour)
		for i := 1; i <= 25; i++ {
			id := fmt.Sprintf("acc-big-%02d", i)
			lines = append(lines, mailLine(id, "大知会 "+id, base.Add(time.Duration(i)*time.Minute), dgBody(id)))
		}
		f.addSource("src_acc_dg_big", "acc-dg-big", "digest-src", "full", lines)
		f.waitEvents("src_acc_dg_big", 25, "digest")
		big := f.runBrief(f.addInstance(0), "done")
		checkBig(t, big)
	})

	f.step("ACT-BRF-07 简报只写 period 时转 needs_input 并写明缺 sections[].body", func(t *testing.T) {
		f.setMode("brief-period")
		defer f.setMode("brief")
		r := f.runBrief(f.addInstance(0), "needs_input")
		if r.Item.Ask == nil || !strings.Contains(r.Item.Ask.Question, "sections[].body") {
			t.Errorf("应写明缺 sections[].body: %+v", r.Item.Ask)
		}
	})
}

// TestBriefEmptySegments：账本里什么都没有时，各段都是空数组（不是 null、不缺键），pending_total 为 0，不报错。
func TestBriefEmptySegments(t *testing.T) {
	f := setup(t)
	r := f.runBrief(f.addInstance(0), "done")
	for _, seg := range []struct{ name, key string }{{"schedule", "schedule"}, {"proposals", "proposals"}, {"decisions", "decisions"},
		{"due_waiting", "due_waiting"}, {"activity", "activity"}, {"digest", "digest"}} {
		m := decode[map[string]json.RawMessage](t, r.Mats[seg.name])
		if got := strings.TrimSpace(string(m[seg.key])); got != "[]" {
			t.Errorf("空账本里 %s 段应是 []，实际 %q（%s）", seg.name, got, r.Mats[seg.name])
		}
	}
	if total := decode[map[string]json.RawMessage](t, r.Mats["proposals"])["pending_total"]; string(total) != "0" {
		t.Errorf("pending_total 应为 0: %s", total)
	}
}

// checkRaw 核对 brief-am 的原文文件：full 的写原文且不超过单文件上限、条目的 material 与文件头一致；excerpt 只有正文前 500 个字符；
// none 与未选择的没有文件；全部文件合计不超过预算；文件只读。
func checkRaw(t *testing.T, f *fx, am runResult) {
	t.Helper()
	total := 0
	for name, text := range am.Files {
		if strings.HasPrefix(name, "source-event-") {
			total += utf8.RuneCountInString(text)
		}
	}
	if total > 400000 {
		t.Errorf("原文文件合计 %d 个字符，超过预算 400000", total)
	}
	bySource := map[string][]digestEntry{}
	for _, d := range digestOf(t, am) {
		bySource[d.Source] = append(bySource[d.Source], d)
	}
	for _, s := range digestSources {
		ents := bySource[s.id]
		if len(ents) != 2 {
			t.Fatalf("%s 应有 2 条知会: %+v", s.id, ents)
		}
		for _, d := range ents {
			if len(d.Raw) != 1 {
				t.Fatalf("%s 应带一个原文引用: %+v", d.Event, d)
			}
			r := d.Raw[0]
			body := dgBody(strings.TrimPrefix(d.Title, "知会 "))
			switch s.external {
			case "full":
				text, ok := am.Files[r.File]
				if !ok || r.File == "" {
					t.Fatalf("%s 应有原文文件: %+v", d.Event, r)
				}
				n := utf8.RuneCountInString(text)
				if n > 20000 || r.Material.Original != 30000 || r.Material.Sent <= 0 || r.Material.Sent > n || r.Material.Scope != "full" {
					t.Errorf("%s 文件 %d 个字符，material = %+v", d.Event, n, r.Material)
				}
				if !strings.Contains(text, fmt.Sprintf("原文 %d 个字符，写入 %d 个字符", r.Material.Original, r.Material.Sent)) {
					t.Errorf("%s 的文件头应写明与条目同样的裁剪量 %+v:\n%.300s", d.Event, r.Material, text)
				}
				if !strings.Contains(text, body[:200]) {
					t.Errorf("%s 的文件应是原文", d.Event)
				}
			case "excerpt":
				text, ok := am.Files[r.File]
				if !ok || r.File == "" || r.Material.Scope != "excerpt" {
					t.Fatalf("%s 应有摘录文件: %+v", d.Event, r)
				}
				if !strings.Contains(text, body[:500]) || strings.Contains(text, body[:700]) {
					t.Errorf("%s 的文件应是正文前 500 个字符的摘录", d.Event)
				}
			default: // none 与未选择
				if r.File != "" || r.Material.Mode != "not_written" {
					t.Errorf("%s（外发范围 %q）不应有原文文件: %+v", d.Event, s.external, r)
				}
			}
		}
	}
	// 建议引用的来源事件（prop-src，全文）也各有文件。
	props := decode[struct {
		Proposals []proposalOut `json:"proposals"`
	}](t, am.Mats["proposals"]).Proposals
	for _, p := range props {
		if len(p.Raw) != 1 || p.Raw[0].File == "" || am.Files[p.Raw[0].File] == "" {
			t.Errorf("建议应带来源原文文件: %+v", p.Raw)
		}
	}
}

func checkBig(t *testing.T, big runResult) {
	t.Helper()
	d := digestOf(t, big)
	if len(d) != 25 {
		t.Fatalf("知会应有 25 条: %d %+v", len(d), d)
	}
	total := 0
	for name, text := range big.Files {
		if strings.HasPrefix(name, "source-event-") {
			total += utf8.RuneCountInString(text)
		}
	}
	// 建议引用的 3 个来源事件的原文文件也计入合计，所以被跳过的不止 5 条；跳过得恰到好处：再放回一个满上限的文件就超预算。
	if total > 400000 || total <= 400000-20000 {
		t.Errorf("原文文件合计 %d 个字符，应不超过 400000 且只跳过必要的几条", total)
	}
	// 条目按落盘顺序（最旧在前）：被跳过的是最旧的那些，只留摘要；之后的都有文件。
	skipped := 0
	for i, e := range d {
		r := e.Raw[0]
		if r.Material.Mode == "summary_only" {
			skipped++
			if r.File != "" || skipped != i+1 {
				t.Errorf("被跳过的应是最旧的连续几条：第 %d 条 %+v", i+1, r)
			}
			continue
		}
		if r.File == "" || big.Files[r.File] == "" {
			t.Errorf("第 %d 条应有原文文件: %+v", i+1, r)
		}
	}
	if skipped < 5 {
		t.Errorf("25 条各约 20000 字符，至少要跳过最旧的 5 条，实际 %d", skipped)
	}
}
