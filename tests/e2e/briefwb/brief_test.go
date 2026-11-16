//go:build e2e

package briefwb

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/srcstub"
)

const importedNote = "IMPORTED-NOTE-不进当天活动"

// accPack 是验收用能力包：日历事件只入账（ignore）；digest-src 的事件进知会；prop-src 的事件直接成为建议。
const accPack = `name: acc-wb
version: 1.0.0
description: 简报资料验收夹具的来源规则
source_rules:
  - {id: cal-ignore, match: {source_type: calendar}, then: ignore}
  - {id: dg, match: {source_type: digest-src}, then: digest}
  - {id: prop, match: {source_type: prop-src}, kind: task, then: proposed}
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

func at(days, hour int) time.Time {
	y, m, d := time.Now().Date()
	return time.Date(y, m, d+days, hour, 0, 0, 0, time.Local)
}

var (
	c1v1 = cal{"c1-v1", "event.created", "周会 C1", "k-c1", at(1, 10), at(1, 11).Format(time.RFC3339), "会议室甲"}
	c1v2 = cal{"c1-v2", "event.updated", "周会 C1", "k-c1", at(2, 15), at(2, 16).Format(time.RFC3339), "会议室乙"} // 改期后的最新版
	c3v1 = cal{"c3-v1", "event.created", "取消的会 C3", "k-c3", at(1, 9), at(1, 10).Format(time.RFC3339), "会议室丙"}
	c3v2 = cal{"c3-v2", "event.cancelled", "取消的会 C3", "k-c3", at(1, 9), at(1, 10).Format(time.RFC3339), "会议室丙"}
	c4   = cal{"c4-v1", "event.created", "第五天的会 C4", "k-c4", at(5, 10), at(5, 11).Format(time.RFC3339), "会议室丁"}
	c2   = cal{"c2-v1", "event.created", "家庭聚餐 C2", "k-c2", at(1, 18), at(1, 20).Format(time.RFC3339), "餐厅"}
)

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
	proposals                        []string // 建议的标题
	decision                         string
}

// buildLedger 造账本夹具：建议流 3 条、待决策 1 条、今天与明天到期与已过期各一、当天完成的事项与进展、一批导入的事件。
func (f *fx) buildLedger() fixtureIDs {
	f.t.Helper()
	var ids fixtureIDs
	work, open := f.Projects["acc-work"], f.Projects["acc-open"]

	f.writePack("acc-wb", accPack)
	f.web("PUT", "/v1/settings/policy", `{"default_harness":"`+briefHarness+`"}`, 200)

	// 知会：四个外发范围不同的来源各两条正文 30000 字符的事件。
	for _, s := range digestSources {
		var lines []map[string]any
		for i := 1; i <= 2; i++ {
			id := fmt.Sprintf("%s-%d", strings.ReplaceAll(s.id, "_", "-"), i)
			lines = append(lines, mailLine(id, "知会 "+id, time.Now().Add(-time.Duration(10-i)*time.Minute), dgBody(id)))
		}
		f.addSource(s.id, strings.ReplaceAll(s.id, "_", "-"), "digest-src", s.external, lines)
		f.waitEvents(s.id, 2, "digest")
	}
	// 建议：三条来自 prop-src（外发范围全文）的事件，规则直接成为建议。
	var pl []map[string]any
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("acc-prop-%d", i)
		ids.proposals = append(ids.proposals, "建议 "+id)
		pl = append(pl, mailLine(id, "建议 "+id, time.Now().Add(-time.Duration(20-i)*time.Minute), "建议正文 "+id))
	}
	f.addSource("src_acc_prop", "acc-prop", "prop-src", "full", pl)
	f.until("三条建议进入建议流", func() bool { return len(f.proposedTitles()) == 3 })

	// 待本人决定的事。
	ids.decision = f.addItem("--title", "待决策：要不要买？", "--phase", "needs_input", "--project", work, "--ask", `{"question":"要不要买？"}`)
	// 今天、明天到期与已过期各一。
	today, tomorrow, yesterday := at(0, 0).Format("2006-01-02"), at(1, 0).Format("2006-01-02"), at(-1, 0).Format("2006-01-02")
	f.addItem("--title", "今天到期", "--phase", "open", "--project", work, "--deadline", today)
	f.addItem("--title", "明天到期", "--phase", "open", "--project", open, "--deadline", tomorrow)
	f.addItem("--title", "已过期", "--phase", "open", "--project", work, "--deadline", yesterday)
	// 当天完成的事项与写下的进展（两个项目）。
	ids.workDone = f.addItem("--title", "今天完成 W", "--phase", "open", "--project", work)
	f.apa(f.DEL, 0, "item", "progress", ids.workDone, "--text", "进展 W1", "--evidence", "验收夹具")
	f.apa(f.DEL, 0, "item", "transition", ids.workDone, "--to", "done", "--reason", "完成", "--evidence", "验收夹具")
	ids.openDone = f.addItem("--title", "今天完成 O", "--phase", "open", "--project", open)
	f.apa(f.DEL, 0, "item", "progress", ids.openDone, "--text", "进展 O1", "--evidence", "验收夹具")
	f.apa(f.DEL, 0, "item", "transition", ids.openDone, "--to", "done", "--reason", "完成", "--evidence", "验收夹具")
	// 一批导入的历史事件：给已完成事项的一条进展，与把另一件事记为完成。
	ids.importedDone = f.addItem("--title", "导入时记为完成的事项", "--phase", "open", "--project", open)
	f.seedImported(ids.workDone, ids.importedDone)
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

	f.step("ACT-BRF-01 准备账本夹具与两个简报实例", func(t *testing.T) {
		ids = f.buildLedger()
		if n := len(f.briefs()); n != 0 {
			t.Fatalf("夹具里不应已有简报: %d", n)
		}
		sameSet(t, "建议流", f.proposedTitles(), ids.proposals)
		amInst = f.addInstance(0, "instruction="+amInstruction)
		pmInst = f.addInstance(0, "schedule_days=3")
		if packs := f.web("GET", "/v1/packs", "", 200); !strings.Contains(packs, "default_instruction") {
			t.Fatalf("简报包应带 default_instruction: %.300s", packs)
		}
	})

	f.step("ACT-BRF-02 没有日历来源时日程为空；其余各段与夹具一致；知会进先跑的 brief-am", func(t *testing.T) {
		am = f.runBrief(amInst, "done")
		if got := scheduleOf(t, am); len(got) != 0 {
			t.Errorf("没有日历来源时日程应为空数组: %+v", got)
		}
		// 新增建议：3 条、pending_total 3、每条带来源名与把握低标记。
		props := decode[struct {
			Proposals []struct {
				Title         string          `json:"title"`
				Sources       string          `json:"sources"`
				LowConfidence json.RawMessage `json:"low_confidence"`
			} `json:"proposals"`
			Total int `json:"pending_total"`
		}](t, am.Mats["proposals"])
		sameSet(t, "proposals 标题", titles(props.Proposals, func(p struct {
			Title         string          `json:"title"`
			Sources       string          `json:"sources"`
			LowConfidence json.RawMessage `json:"low_confidence"`
		}) string {
			return p.Title
		}), ids.proposals)
		if props.Total != 3 {
			t.Errorf("pending_total = %d，期望 3", props.Total)
		}
		for _, p := range props.Proposals {
			if p.Sources != "acc-prop" || len(p.LowConfidence) == 0 {
				t.Errorf("建议条目应带来源名与把握低标记: %+v", p)
			}
		}
		// 待决策：夹具里的一条。
		dec := decode[struct {
			Decisions []struct{ ID, Title, Question string } `json:"decisions"`
		}](t, am.Mats["decisions"]).Decisions
		if len(dec) != 1 || dec[0].ID != ids.decision || dec[0].Question != "要不要买？" {
			t.Errorf("decisions = %+v，期望只有 %s", dec, ids.decision)
		}
		// 到期与等待：今天、明天、已过期各一。
		due := decode[struct {
			Due []struct{ Title, Reason string } `json:"due_waiting"`
		}](t, am.Mats["due_waiting"]).Due
		var got []string
		for _, d := range due {
			got = append(got, d.Title+"/"+d.Reason)
		}
		sameSet(t, "due_waiting", got, []string{"今天到期/due_today", "明天到期/due_tomorrow", "已过期/overdue"})
		// 当天活动：按项目分组，导入事件不计。
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
		sameSet(t, "acc-work 的活动", byProject[f.Projects["acc-work"]], []string{"progress:今天完成 W:进展 W1", "done:今天完成 W:"})
		sameSet(t, "acc-open 的活动", byProject[f.Projects["acc-open"]], []string{"progress:今天完成 O:进展 O1", "done:今天完成 O:"})
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

	f.step("ACT-BRF-03 两个日历来源：改期只取最新版、取消的与窗口外的不列", func(t *testing.T) {
		f.addSource(calWork, "工作日历", "calendar", "", []map[string]any{c1v1.line(), c1v2.line(), c3v1.line(), c3v2.line(), c4.line()})
		f.addSource(calHome, "家庭日历", "calendar", "", []map[string]any{c2.line()})
		f.waitEvents(calWork, 5, "ignored")
		f.waitEvents(calHome, 1, "ignored")
		pm1 = f.runBrief(pmInst, "done")
		got := scheduleOf(t, pm1)
		if len(got) != 2 {
			t.Fatalf("schedule = %+v，期望 C2 与改期后的 C1", got)
		}
		// 按开始时间排：C2（明天 18:00）在前，C1 取改期后的后天 15:00。
		c2e, c1e := got[0], got[1]
		if c2e.Title != c2.subject || c2e.Source != "家庭日历" || !c2e.Start.Equal(c2.start) || c2e.Location != "餐厅" || c2e.End != c2.end {
			t.Errorf("C2 = %+v", c2e)
		}
		if c1e.Title != c1v2.subject || c1e.Source != "工作日历" || !c1e.Start.Equal(c1v2.start) || c1e.Location != "会议室乙" || c1e.End != c1v2.end {
			t.Errorf("C1 应取改期后的最新版: %+v", c1e)
		}
		for _, e := range got {
			if strings.Contains(e.Title, "C3") || strings.Contains(e.Title, "C4") {
				t.Errorf("已取消的与窗口外的不应出现: %+v", e)
			}
		}
		if d := digestOf(t, pm1); len(d) != 0 {
			t.Errorf("同一条知会只进先跑的一份，brief-pm 的知会应为空: %+v", d)
		}
	})

	f.step("ACT-BRF-04 两个实例各有自己的窗口、内容要求与所属实例", func(t *testing.T) {
		amW, pmW := windowOf(t, am), windowOf(t, pm1)
		if d := amW.Until.Sub(amW.Since); d != 24*time.Hour {
			t.Errorf("brief-am 是该实例第一份，窗口应为 24 小时: %v", d)
		}
		if d := pmW.Until.Sub(pmW.Since); d != 24*time.Hour {
			t.Errorf("brief-pm 的第一份窗口也是 24 小时（不借用 brief-am 的）: %v", d)
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

	f.step("ACT-BRF-05 窗口天数：schedule_days=7 时 C4 出现；15 被拒", func(t *testing.T) {
		seven := f.addInstance(0, "schedule_days=7")
		got := scheduleOf(t, f.runBrief(seven, "done"))
		if len(got) != 3 || got[2].Title != c4.subject {
			t.Errorf("7 天窗口应多出 C4: %+v", got)
		}
		f.addInstance(2, "schedule_days=15")
	})

	f.step("ACT-BRF-06 来源原文文件不超过外发范围与字符预算", func(t *testing.T) {
		checkRaw(t, f, am)
	})

	f.step("ACT-BRF-07 合计超过 400000 字符时从最旧的条目起只留摘要", func(t *testing.T) {
		var lines []map[string]any
		base := time.Now().Add(-time.Hour)
		for i := 1; i <= 25; i++ {
			id := fmt.Sprintf("acc-big-%02d", i)
			lines = append(lines, mailLine(id, "大知会 "+id, base.Add(time.Duration(i)*time.Minute), dgBody(id)))
		}
		f.addSource("src_acc_dg_big", "acc-dg-big", "digest-src", "full", lines)
		f.waitEvents("src_acc_dg_big", 25, "digest")
		big := f.runBrief(f.addInstance(0), "done")
		checkBig(t, big)
	})

	f.step("简报只写 period 时转 needs_input 并写明缺 sections[].body", func(t *testing.T) {
		f.setMode("brief-period")
		defer f.setMode("brief")
		r := f.runBrief(f.addInstance(0), "needs_input")
		if r.Item.Ask == nil || !strings.Contains(r.Item.Ask.Question, "sections[].body") {
			t.Errorf("应写明缺 sections[].body: %+v", r.Item.Ask)
		}
	})
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
		Proposals []struct {
			Raw []rawRef `json:"raw"`
		} `json:"proposals"`
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
