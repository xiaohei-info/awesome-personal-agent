//go:build e2e

// Package judgment 按验收文档 flow-judgment-record（ACT-JRC-01..21）用真实 apa 二进制（-tags apa_testclock 测试构建）
// 独立执行"判断记录、计数、校准与抽检"全流程。环境来自 flow-env-baseline 的共用脚本（tests/e2e/baseline）：
// 隔离的 APA_HOME、五个凭据、CLS_STUB / MODEL_STUB 两个端点替身、STUB_HARNESS、测试时钟、合成账本夹具。
// 不使用任何真实模型端点、宿主或账号；这些由本机升级验证 L1–L12 承接。
//
// Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。ego-browser 不在本主机，页面渲染本身不在此验证。
package judgment

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func TestFlowJudgmentRecord(t *testing.T) {
	f := setup(t)

	// flow-step 失败后续不得开始；business-action 失败记录后继续，让一次运行报出全部缺陷。
	// 每个动作用自己的 *testing.T：等待超时只让该动作失败，其余动作照常执行。
	step := func(name string, fn func(t *testing.T)) bool {
		t.Helper()
		return t.Run(name, func(t *testing.T) {
			parent, parentEnv := f.t, f.Env.T
			f.t, f.Env.T = t, t
			defer func() { f.t, f.Env.T = parent, parentEnv }()
			fn(t)
		})
	}
	// flow-step 失败后续不得开始；business-action 失败记录后继续，让一次运行报出全部缺陷。
	stepFatal := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !step(name, fn) {
			t.Fatalf("%s 未通过，后续动作不得开始", name)
		}
	}

	ev := map[string]sourceEvent{} // 关键字 → 来源事件
	rc := map[string]record{}      // 关键字 → 定案的判断记录
	var highItem, midItem, undItem, lowRec, p20Rec string
	var cursorBefore int64

	// ---- ACT-JRC-01 ----
	stepFatal("ACT-JRC-01 准备判断用来源与替身用例", func(t *testing.T) {
		f.registerSource()
		f.cases()

		out := httpPost(t, f.Cls.URL(), map[string]any{"protocol": "probability-judgment/1", "questions": []map[string]any{
			{"id": "q1", "question": "q", "criteria": "c", "input": map[string]string{"s": "JRC-HIGH"}}}})
		if !strings.Contains(out, `"p_yes":0.9`) {
			t.Fatalf("CLS_STUB 没有返回配置的概率: %s", out)
		}
		chat := httpPost(t, f.ModelStub.URL()+"/chat/completions", map[string]any{"model": "acc-model",
			"messages": []map[string]string{{"role": "user", "content": "JRC-MID"}},
			"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "judge",
				"schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}, "sufficient": map[string]any{"type": "boolean"}}}}}})
		if !strings.Contains(chat, `answer`) || !strings.Contains(chat, `yes`) {
			t.Fatalf("MODEL_STUB 没有返回配置的答案: %s", chat)
		}
		// 清空两个替身日志（Reset 同时清空用例，所以重配一次）。
		f.Cls.Reset()
		f.ModelStub.Reset()
		f.cases()
		cursorBefore = f.lastSeq()
	})

	// ---- ACT-JRC-02 ----
	step("ACT-JRC-02 每道题写一条判断记录，含逐级结果与定案者", func(t *testing.T) {
		f.emit("JRC-HIGH", "JRC-LOW", "JRC-MID", "JRC-UND")
		for _, k := range []string{"JRC-HIGH", "JRC-LOW", "JRC-MID", "JRC-UND"} {
			ev[k] = f.eventOf(k)
		}
		recs := f.records("source=" + f.srcID + "&use=inbound_screen")
		if len(recs) != 4 {
			t.Fatalf("应恰好四条判断记录，实际 %d", len(recs))
		}
		raw := f.get(f.OBS, "/v1/judgments?source="+f.srcID)
		if strings.Contains(raw, "JRC-BODY") {
			t.Errorf("判断记录里不得出现资料正文 JRC-BODY")
		}
		for k, e := range ev {
			r := f.final(e.ID)
			rc[k] = r
			if strings.Contains(f.get(f.OBS, "/v1/judgments/"+r.ID), "JRC-BODY") {
				t.Errorf("%s: 记录含资料正文", k)
			}
			if r.Use != "inbound_screen" || r.Subject != e.ID || r.Source != f.srcID {
				t.Errorf("%s: use/subject/source 不对: %+v", k, r)
			}
			if !strings.Contains(r.Template, "inbound-triage") || !strings.Contains(r.Template, "triage-screen") || !strings.Contains(r.Template, "@") {
				t.Errorf("%s: template 应含包名、triage-screen 与内容 hash: %q", k, r.Template)
			}
			if r.Band.Low != 0.15 || r.Band.High != 0.60 {
				t.Errorf("%s: band 应为 0.15/0.60: %+v", k, r.Band)
			}
			for i, l := range r.Levels {
				if l.Endpoint == "" || l.Locality == "" || l.Model == "" || l.DurationMs == nil {
					t.Errorf("%s: 第 %d 级缺端点位置、模型标识或耗时: %+v", k, i, l)
				}
			}
		}
		check := func(k, decidedBy, answer string, outcomes ...string) {
			r := rc[k]
			if r.DecidedBy != decidedBy || r.Answer != answer || len(r.Levels) != len(outcomes) {
				t.Errorf("%s: decided_by/answer/级数 = %s/%s/%d，期望 %s/%s/%d", k, r.DecidedBy, r.Answer, len(r.Levels), decidedBy, answer, len(outcomes))
				return
			}
			for i, o := range outcomes {
				if r.Levels[i].Outcome != o {
					t.Errorf("%s: 第 %d 级 outcome=%s，期望 %s", k, i, r.Levels[i].Outcome, o)
				}
			}
		}
		check("JRC-HIGH", "classifier", "yes", "decided")
		check("JRC-LOW", "classifier", "no", "decided")
		check("JRC-MID", "chat", "yes", "uncertain", "decided")
		check("JRC-UND", "default", rc["JRC-UND"].Answer, "uncertain", "uncertain") // 两级都未定：定案者是 default，答案由缺省路径给出（留为建议）
		if p := rc["JRC-HIGH"].Levels[0].PYes; p == nil || *p != 0.90 || rc["JRC-HIGH"].Levels[0].ProbabilitySource != "native" {
			t.Errorf("JRC-HIGH 一级应为 p_yes=0.90、probability_source=native: %+v", rc["JRC-HIGH"].Levels[0])
		}
		if !rc["JRC-UND"].LowConf {
			t.Errorf("JRC-UND 应 low_confidence=true")
		}
		highItem, midItem, undItem, lowRec = ev["JRC-HIGH"].Item, ev["JRC-MID"].Item, ev["JRC-UND"].Item, rc["JRC-LOW"].ID
	})

	// ---- ACT-JRC-03 ----
	step("ACT-JRC-03 判断不进 Event 日志，处置命令的 Event 引用判断记录 id", func(t *testing.T) {
		mine := map[string]string{} // 来源事件 id → 关键字
		for k, e := range ev {
			mine[e.ID] = k
		}
		disposed := map[string]string{}
		for _, s := range f.audit("") {
			if s.Seq <= cursorBefore {
				continue
			}
			if (s.Type == "run.started" || s.Type == "run.finished") && payloadField(s.Payload, "category") == "judge" {
				t.Errorf("判断写进了 Event 日志: seq %d %s %s", s.Seq, s.Type, s.Payload)
			}
			if strings.Contains(string(s.Payload), "p_yes") || strings.Contains(string(s.Payload), "probability") {
				t.Errorf("Event 里不应有逐级的概率或答案: seq %d %s", s.Seq, s.Payload)
			}
			if _, ok := mine[s.Object]; ok && s.Type == "inbound.disposed" {
				disposed[s.Object] = payloadField(s.Payload, "judgment")
			}
		}
		for k, e := range ev {
			if disposed[e.ID] != rc[k].ID {
				t.Errorf("%s: inbound.disposed 的 judgment=%q，期望判断记录 %s", k, disposed[e.ID], rc[k].ID)
			}
		}
	})

	// ---- ACT-JRC-04 ----
	step("ACT-JRC-04 apa why 与 apa judgment show 回放每一级", func(t *testing.T) {
		why := func(token, id string) []auditStep {
			var tl struct {
				Steps []auditStep `json:"steps"`
			}
			if err := json.Unmarshal([]byte(f.apa(token, 0, "why", id).Stdout), &tl); err != nil {
				t.Fatalf("apa why %s: %v", id, err)
			}
			return tl.Steps
		}
		judged := func(steps []auditStep) *record {
			for _, s := range steps {
				if s.Judgment != nil && s.Judgment.Record != nil {
					return s.Judgment.Record
				}
			}
			return nil
		}
		hi := judged(why(f.OBS, highItem))
		if hi == nil || hi.ID != rc["JRC-HIGH"].ID || len(hi.Levels) != 1 || hi.Levels[0].PYes == nil || *hi.Levels[0].PYes != 0.90 ||
			hi.Levels[0].Outcome != "decided" || hi.Band.High != 0.60 || hi.Answer != "yes" {
			t.Errorf("apa why <JRC-HIGH 事项> 应有一行判断（p=0.90 高于上限 0.60 → 是、定案）: %+v", hi)
		}
		mid := judged(why(f.OBS, midItem))
		if mid == nil || mid.ID != rc["JRC-MID"].ID || len(mid.Levels) != 2 || mid.Levels[0].Outcome != "uncertain" || mid.Levels[1].Answer != "yes" || mid.Levels[1].Outcome != "decided" {
			t.Errorf("apa why <JRC-MID 事项> 应列出两级（分类端点落入不确定带、对话端点答是并定案）: %+v", mid)
		}
		// Web 事项详情的时间线是同一份数据。
		var web struct {
			Steps []auditStep `json:"steps"`
		}
		_ = json.Unmarshal([]byte(f.webOK("GET", "/why/"+midItem, "", 200)), &web)
		if w := judged(web.Steps); w == nil || w.ID != rc["JRC-MID"].ID {
			t.Errorf("Web 时间线应与 apa why 一致: %+v", w)
		}
		var shown record
		if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "judgment", "show", rc["JRC-MID"].ID).Stdout), &shown); err != nil ||
			shown.ID != rc["JRC-MID"].ID || len(shown.Levels) != 2 || shown.Levels[0].Backend != "classifier" || shown.Levels[1].Backend != "chat" || shown.DecidedBy != "chat" {
			t.Errorf("apa judgment show 应逐级列出后端、结果与定案者: %v %+v", err, shown)
		}
	})

	// ---- ACT-JRC-05（含 ACT-JRC-09 里"接受 JRC-HIGH"这一步：接受使事项进入 open，触发任务型运行）----
	step("ACT-JRC-05 运行凭据只能读目标事项的判断记录", func(t *testing.T) {
		f.enableJudgmentRun(rc["JRC-HIGH"].ID, lowRec)
		res := f.webOK("POST", "/proposals/batch", fmt.Sprintf(`{"mode":"explicit","actions":[{"id":%q,"do":"accept"}]}`, highItem), 200)
		if !strings.Contains(res, `"ok":true`) {
			t.Fatalf("接受 JRC-HIGH 的建议失败: %s", res)
		}
		dir := f.waitRunFile("jrc-cli.code")
		read := func(name string) string {
			b, _ := os.ReadFile(filepath.Join(dir, name))
			return strings.TrimSpace(string(b))
		}
		if read("jrc-high.code") != "200" {
			t.Errorf("读目标事项的记录应返回 200，实际 %s", read("jrc-high.code"))
		}
		if c := read("jrc-low.code"); c != "403" && c != "404" {
			t.Errorf("读 JRC-LOW 的记录应被拒（403 或 404），实际 %s", c)
		}
		if read("jrc-cli.code") != "5" {
			t.Errorf("运行内 apa judgment show <JRC-LOW> 退出码应为 5，实际 %s", read("jrc-cli.code"))
		}
	})

	// ---- ACT-JRC-07 ----
	step("ACT-JRC-07 v0.1 的判断 Event 原样保留可回放，不进判断记录与校准统计", func(t *testing.T) { f.legacyJudgeEvents(t) })

	// ---- ACT-JRC-08 ----
	var apiStats stats
	step("ACT-JRC-08 按用途、模板、来源、后端计数并给出升级率与未定率", func(t *testing.T) {
		var body string
		apiStats, body = f.statsAPI("")
		if len(apiStats.Rows) == 0 {
			// 按验收文档的写法（模板名 triage-screen）筛不到记录：记录里的 template 是“包名/模板名@hash”。
			t.Errorf("--template triage-screen 应能筛到 ACT-JRC-02 的四条记录（记录的 template 为 %q），实际 rows 为空", rc["JRC-HIGH"].Template)
			f.statTemplate = rc["JRC-HIGH"].Template // 用完整标识继续核对后续数值
			apiStats, body = f.statsAPI("")
		}
		cli := f.apa(f.OBS, 0, "judgment", "stats", "--use", "inbound_screen", "--template", f.statTemplate, "--source", f.srcID).Stdout
		var viaCLI stats
		if err := json.Unmarshal([]byte(cli), &viaCLI); err != nil {
			t.Fatalf("CLI 输出不是 JSON: %q", cli)
		}
		if c := apiStats.row("classifier"); c.Calls != 4 || c.Decided != 2 || c.Uncertain != 2 {
			t.Errorf("classifier 应 calls=4 decided=2 uncertain=2: %+v", c)
		}
		if c := apiStats.row("chat"); c.Calls != 2 || c.Decided != 1 || c.Uncertain != 1 {
			t.Errorf("chat 应 calls=2 decided=1 uncertain=1: %+v", c)
		}
		if len(apiStats.Rates) != 1 || !near(apiStats.Rates[0].Escalation, 0.5) || !near(apiStats.Rates[0].Undecided, 0.25) {
			t.Errorf("rates 应 escalation=0.5 undecided=0.25: %+v", apiStats.Rates)
		}
		if !reflect.DeepEqual(apiStats, viaCLI) {
			t.Errorf("CLI 与 API 数值应一致:\ncli=%s\napi=%s", cli, body)
		}
		for _, w := range []string{"cost", "price", "amount", "quota", "currency", "usd", "金额", "费用", "额度"} {
			if strings.Contains(strings.ToLower(body), w) {
				t.Errorf("统计输出不应有费用换算类字段 %q", w)
			}
		}
	})

	// ---- ACT-JRC-09 ----
	step("ACT-JRC-09 接受率把接受与补到已有事项都算作接受", func(t *testing.T) {
		// JRC-HIGH 已在 ACT-JRC-05 接受。丢弃 JRC-MID；把 JRC-UND 补到 acc-home 下一个已有的 open 事项。
		target := f.openItem("已有事项", "acc-home")
		res := f.webOK("POST", "/proposals/batch", fmt.Sprintf(`{"mode":"explicit","actions":[{"id":%q,"do":"dismiss","reason":"不需要"},{"id":%q,"do":"attach","to":%q}]}`, midItem, undItem, target), 200)
		if strings.Contains(res, `"ok":false`) {
			t.Fatalf("丢弃/补到已有事项失败: %s", res)
		}
		s, _ := f.statsAPI("")
		if len(s.Rates) != 1 || !near(s.Rates[0].Acceptance, 2.0/3) {
			t.Errorf("acceptance 应约为 0.67: %+v", s.Rates)
		}
		if r := f.apa(f.OBS, 0, "judgment", "stats", "--use", "inbound_screen", "--template", f.statTemplate, "--source", f.srcID).Stdout; !strings.Contains(r, `"acceptance"`) {
			t.Errorf("CLI 应带 acceptance: %s", r)
		}
	})

	// ---- ACT-JRC-10 ----
	step("ACT-JRC-10 在判断与校准页查看指标与分数分布", func(t *testing.T) {
		base, _ := f.statsAPI("")
		for _, window := range []string{"7d", "30d", "90d"} {
			s, _ := f.statsAPI("&since=" + window)
			if !reflect.DeepEqual(s, base) {
				t.Errorf("窗口 %s 的数值应与 ACT-JRC-08/09 一致: %+v vs %+v", window, s, base)
			}
		}
		if len(base.Rates) != 1 {
			t.Fatalf("应有一组指标: %+v", base.Rates)
		}
		r := base.Rates[0]
		if !near(r.Escalation, 0.5) || !near(r.Undecided, 0.25) || !near(r.Acceptance, 2.0/3) || r.FalseDrop != 0 {
			t.Errorf("升级率、未定率、接受率、误丢率与 ACT-JRC-08/09 不一致: %+v", r)
		}
		total, at := 0, map[float64]int{}
		for _, b := range base.Histogram {
			total += b.Count
			at[b.Low] += b.Count
		}
		if total != 4 || at[0.9] != 1 || at[0.3] != 2 || at[0] != 1 {
			t.Errorf("分数分布应覆盖分类端点的四次打分（0.9、0.3、0.3、0.05）: %+v", base.Histogram)
		}
		if other := f.get(f.OBS, "/v1/judgments/stats?use=inbound_screen"); !strings.Contains(other, `"rows"`) {
			t.Errorf("应可按用途分组: %s", other)
		}
		if other := f.get(f.OBS, "/v1/judgments/stats?template="+url.QueryEscape(f.statTemplate)+"&source="+f.srcID); !strings.Contains(other, `"backend":"chat"`) {
			t.Errorf("应可按来源与后端分组: %s", other)
		}
	})

	// ---- ACT-JRC-11 ----
	policyChanged := func() int { return len(f.audit("policy.changed")) }
	step("ACT-JRC-11 本人在 Web 改不确定带，按来源的覆盖优先，并记 Event", func(t *testing.T) {
		before := policyChanged()
		f.webOK("PUT", "/settings/judgment", `{"bands":[{"template":"triage-screen","low":0.10,"high":0.90},{"template":"triage-screen","source":"`+f.srcID+`","low":0.25,"high":0.50}]}`, 200)
		f.cls("JRC-P20", 0.20)
		f.emit("JRC-P20")
		e := f.eventOf("JRC-P20")
		r := f.final(e.ID)
		p20Rec = r.ID
		var got struct {
			Bands []map[string]any `json:"bands"`
		}
		_ = json.Unmarshal([]byte(f.webOK("GET", "/settings/judgment", "", 200)), &got)
		if len(got.Bands) != 2 {
			t.Errorf("GET /settings/judgment 应返回两条 bands: %+v", got.Bands)
		}
		steps := f.audit("policy.changed")
		if len(steps) != before+1 || steps[len(steps)-1].Actor.Principal != "owner" || steps[len(steps)-1].Actor.Via != "web" {
			t.Errorf("应新增一条本人（Web）的 policy.changed: before=%d %+v", before, steps)
		}
		if r.Band.Low != 0.25 || r.Band.High != 0.50 || len(r.Levels) != 1 || r.Levels[0].Backend != "classifier" || r.Answer != "no" || e.Disposition != "ignored" {
			t.Errorf("JRC-P20 应采用来源的 0.25/0.50、只有分类端点一级、答否、事件被忽略: %+v disposition=%s", r, e.Disposition)
		}
	})

	// ---- ACT-JRC-12 ----
	step("ACT-JRC-12 下限大于上限的不确定带被拒绝", func(t *testing.T) {
		before := policyChanged()
		r := f.Web("PUT", "/v1/settings/judgment", `{"bands":[{"template":"triage-screen","source":"`+f.srcID+`","low":0.70,"high":0.40}]}`)
		if r.Code != 400 {
			t.Errorf("非法带应返回 400，实际 %d %s", r.Code, r.Body)
		}
		if after := policyChanged(); after != before {
			t.Errorf("不应有新的 policy.changed: %d → %d", before, after)
		}
		var got struct {
			Bands []struct{ Low, High float64 } `json:"bands"`
		}
		_ = json.Unmarshal([]byte(f.webOK("GET", "/settings/judgment", "", 200)), &got)
		found := false
		for _, b := range got.Bands {
			found = found || (b.Low == 0.25 && b.High == 0.50)
		}
		if !found {
			t.Errorf("已有的 0.25/0.50 应不变: %+v", got.Bands)
		}
	})

	// ---- ACT-JRC-13 ----
	step("ACT-JRC-13 不确定带、影子模式与抽检结论不能经凭据写入", func(t *testing.T) {
		before := policyChanged()
		var bandsBefore string
		bandsBefore = f.webOK("GET", "/settings/judgment", "", 200)
		for _, tok := range []string{f.DEL, f.OBS} {
			if r := f.APIRaw("PUT", "/v1/settings/judgment", `{"bands":[],"shadow":["triage-screen"]}`, tok); r.Code != 403 {
				t.Errorf("PUT /settings/judgment 凭据写入应 403，实际 %d %s", r.Code, r.Body)
			}
			if r := f.APIRaw("POST", "/v1/judgments/"+p20Rec+"/review", `{"result":"should_see"}`, tok); r.Code != 403 {
				t.Errorf("POST review 凭据写入应 403，实际 %d %s", r.Code, r.Body)
			}
		}
		if got := f.webOK("GET", "/settings/judgment", "", 200); got != bandsBefore {
			t.Errorf("不确定带与影子列表不应变化:\n%s\n%s", bandsBefore, got)
		}
		if f.record(p20Rec).Review != nil {
			t.Errorf("该记录的 review 不应变化")
		}
		if after := policyChanged(); after != before {
			t.Errorf("不应有新的 policy.changed: %d → %d", before, after)
		}
		help := f.Apa(f.OBS, "", "judgment", "--help")
		out := help.Stdout + help.Stderr
		for _, want := range []string{"show", "spot-check", "stats"} {
			if !strings.Contains(out, want) {
				t.Errorf("apa judgment 应有只读子命令 %s: %s", want, out)
			}
		}
		for _, bad := range []string{"review", "band", "shadow", "set", "update"} {
			if strings.Contains(out, bad) {
				t.Errorf("apa judgment 不应有写入类子命令 %s: %s", bad, out)
			}
		}
	})

	// ---- ACT-JRC-14 ----
	var shdEvent sourceEvent
	step("ACT-JRC-14 影子模式只记分不定案", func(t *testing.T) {
		before := policyChanged()
		f.webOK("PUT", "/settings/judgment", `{"shadow":["triage-screen"]}`, 200)
		if policyChanged() != before+1 {
			t.Errorf("保存影子开关后事件流应有 policy.changed")
		}
		sufficient := true
		f.cls("JRC-SHD", 0.05)
		f.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "JRC-SHD"}, Answer: "yes", Sufficient: &sufficient})
		f.emit("JRC-SHD")
		shdEvent = f.eventOf("JRC-SHD")
		var shadow *record
		recs := f.records("subject=" + shdEvent.ID)
		for i := range recs {
			if recs[i].Shadow {
				shadow = &recs[i]
			}
		}
		if shadow == nil || len(shadow.Levels) == 0 || shadow.Levels[0].PYes == nil || *shadow.Levels[0].PYes != 0.05 {
			t.Fatalf("应有一条 shadow=true、含分类端点 p_yes=0.05 的判断记录: %+v", recs)
		}
		var fin *record
		for i := range recs {
			if !recs[i].Shadow {
				fin = &recs[i]
			}
		}
		if shdEvent.Disposition != "proposed" || shdEvent.Item == "" {
			t.Errorf("事件应由对话端点定案成为建议，而不是按 0.05 被忽略: %+v", shdEvent)
		}
		disposed := ""
		for _, s := range f.audit("inbound.disposed") {
			if s.Object == shdEvent.ID {
				disposed = payloadField(s.Payload, "judgment")
			}
		}
		if fin == nil {
			t.Errorf("处置 Event 的 judgment 应指向非影子的那条记录，但该事件只有一条 shadow=true 的记录 %s（处置 Event 指向 %s，即影子记录被用作处置依据）", shadow.ID, disposed)
			return
		}
		if fin.Answer != "yes" || fin.DecidedBy != "chat" {
			t.Errorf("处置应按没有分类端点的路径由对话端点定案（answer=yes）: %+v", fin)
		}
		if disposed != fin.ID {
			t.Errorf("处置 Event 的 judgment 应指向非影子的那条 %s，实际 %s（影子 %s）", fin.ID, disposed, shadow.ID)
		}
	})

	// ---- ACT-JRC-15 ----
	step("ACT-JRC-15 来源页可按最近一次判断的分数排序", func(t *testing.T) {
		evs := f.sourceEvents("&sort=score")
		pos := map[string]int{}
		last, seenNil := 2.0, false
		for i, e := range evs {
			pos[e.Key.EventID] = i
			switch {
			case e.Score == nil:
				seenNil = true
			case seenNil:
				t.Errorf("没有分数的应排在最后: %s 在无分数事件之后", e.Key.EventID)
			case *e.Score > last:
				t.Errorf("事件应按 score 从高到低排列: %s=%v 排在 %v 之后", e.Key.EventID, *e.Score, last)
			default:
				last = *e.Score
			}
		}
		hi, okH := pos["ev-JRC-HIGH"]
		p20, okP := pos["ev-JRC-P20"]
		if !okH || !okP || hi > p20 {
			t.Errorf("JRC-HIGH 0.90 应在 JRC-P20 0.20 之前: %v", pos)
		}
		if r := f.APIRaw("GET", "/v1/sources/"+f.srcID+"/events?sort=bogus", "", f.OBS); r.Code != 400 {
			t.Errorf("不支持的排序应 400，实际 %d", r.Code)
		}
	})

	// ---- ACT-JRC-06 ----
	// 放在 ACT-JRC-15 之后执行：保留期清理会删掉全部超过 1 天的记录，ACT-JRC-08..15 依赖 ACT-JRC-02 的记录与分数，
	// 验收文档按编号把 06 排在它们前面，两者互相矛盾，这里按依赖顺序执行。
	step("ACT-JRC-06 判断记录过了保留期被清理，apa why 只显示命令 Event 里的结论", func(t *testing.T) { f.retention(t, ev["JRC-HIGH"], rc["JRC-HIGH"], highItem) })

	// ---- ACT-JRC-16 ----
	stepFatal("ACT-JRC-16 关闭影子模式并造出一批被初筛忽略的事件", func(t *testing.T) {
		f.webOK("PUT", "/settings/judgment", `{"shadow":[],"bands":[]}`, 200)
		if body := f.webOK("GET", "/instances", "", 200); strings.Contains(body, `"pack":"brief"`) {
			t.Fatalf("不应有简报实例: %s", body)
		}
		keys := f.lows(1, 6)
		f.emit(keys...)
		for _, k := range keys {
			e := f.eventOf(k)
			r := f.final(e.ID)
			if e.Disposition != "ignored" || r.DecidedBy != "classifier" || r.Answer != "no" {
				t.Fatalf("%s 应被初筛忽略（classifier / no）: %+v %+v", k, e, r)
			}
		}
	})

	// ---- ACT-JRC-17 ----
	var pool []poolEntry
	step("ACT-JRC-17 抽检池只从被初筛忽略的事件里取样，CLI 只读列出", func(t *testing.T) {
		pool = f.spotPool(f.get(f.OBS, "/v1/judgments/spot-check"))
		viaCLI := f.spotPool(f.apa(f.OBS, 0, "judgment", "spot-check").Stdout)
		if len(pool) != 5 {
			t.Fatalf("应列出 5 条待抽检条目，实际 %d", len(pool))
		}
		if !reflect.DeepEqual(pool, viaCLI) {
			t.Errorf("CLI 与 API 应列出同一批:\n%v\n%v", viaCLI, pool)
		}
		proposed := map[string]bool{}
		for _, k := range []string{"JRC-HIGH", "JRC-MID", "JRC-UND", "JRC-SHD"} {
			proposed[f.eventID(k)] = true
		}
		for _, p := range pool {
			r := f.record(p.Judgment)
			var disp string
			for _, e := range f.sourceEvents("") {
				if e.ID == p.Event {
					disp = e.Disposition
				}
			}
			if r.Use != "inbound_screen" || r.Answer != "no" || disp != "ignored" || proposed[p.Event] || p.WebPath == "" {
				t.Errorf("条目应对应 use=inbound_screen、answer=no 的记录与 ignored 的来源事件，且带 Web 入口: %+v %+v disposition=%s", p, r, disp)
			}
		}
		// 页面的抽检区域读同一个接口（GET /judgments/spot-check）：连续两次读取得到同一批。
		if again := f.spotPool(f.get(f.OBS, "/v1/judgments/spot-check")); !reflect.DeepEqual(again, pool) {
			t.Errorf("结论填入前多次读取应得到同一批")
		}
	})

	// ---- ACT-JRC-18 ----
	step("ACT-JRC-18 本人点“该看”把事件转成建议并计入误丢", func(t *testing.T) {
		if len(pool) < 2 {
			t.Skip("抽检池不足")
		}
		first := pool[0]
		f.webOK("POST", "/judgments/"+first.Judgment+"/review", `{"result":"should_see"}`, 200)
		var e sourceEvent
		f.until("事件转成建议", func() bool {
			for _, x := range f.sourceEvents("") {
				if x.ID == first.Event {
					e = x
				}
			}
			return e.Disposition == "proposed" && e.Item != ""
		})
		r := f.record(first.Judgment)
		if r.Review == nil || r.Review.Result != "should_see" || r.Review.At == "" {
			t.Errorf("判断记录应 review=should_see 并带抽检时间: %+v", r.Review)
		}
		s, _ := f.statsAPI("")
		if len(s.Rates) != 1 || s.Rates[0].Reviewed != 1 || !near(s.Rates[0].FalseDrop, 1) {
			t.Errorf("stats 应 reviewed=1、false_drop=1: %+v", s.Rates)
		}
		if body := f.webOK("GET", "/items/"+e.Item, "", 200); !strings.Contains(body, `"proposed"`) {
			t.Errorf("建议流应出现对应的 proposed 事项: %s", body)
		}
	})

	// ---- ACT-JRC-19 ----
	step("ACT-JRC-19 本人点“确实不用”只记结论", func(t *testing.T) {
		if len(pool) < 2 {
			t.Skip("抽检池不足")
		}
		second := pool[1]
		f.webOK("POST", "/judgments/"+second.Judgment+"/review", `{"result":"correct"}`, 200)
		var e sourceEvent
		for _, x := range f.sourceEvents("") {
			if x.ID == second.Event {
				e = x
			}
		}
		r := f.record(second.Judgment)
		if e.Disposition != "ignored" || e.Item != "" {
			t.Errorf("事件仍应为 ignored、不产生建议: %+v", e)
		}
		if r.Review == nil || r.Review.Result != "correct" {
			t.Errorf("判断记录应 review=correct: %+v", r.Review)
		}
		s, _ := f.statsAPI("")
		if len(s.Rates) != 1 || s.Rates[0].Reviewed != 2 || !near(s.Rates[0].FalseDrop, 0.5) {
			t.Errorf("stats 应 reviewed=2、false_drop=0.5: %+v", s.Rates)
		}
	})

	// ---- ACT-JRC-20 ----
	step("ACT-JRC-20 有简报实例时每份简报带 5 条抽检条目", func(t *testing.T) { f.briefSpotCheck(t) })

	// ---- ACT-JRC-21 ----
	step("ACT-JRC-21 清理", func(t *testing.T) {
		f.cleanup(t)
		var got struct {
			Bands  []any `json:"bands"`
			Shadow []any `json:"shadow"`
		}
		if err := json.Unmarshal([]byte(f.webOK("GET", "/settings/judgment", "", 200)), &got); err != nil || len(got.Bands) != 0 || len(got.Shadow) != 0 {
			t.Errorf("bands 与 shadow 应为空: %v %+v", err, got)
		}
		if b, _ := os.ReadFile(filepath.Join(f.Home, "config.yaml")); strings.Contains(string(b), "judgment_retention_days") {
			t.Errorf("config.yaml 不应残留 judgment_retention_days")
		}
	})
}
