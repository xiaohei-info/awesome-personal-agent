//go:build e2e

package inbound

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const inpRules = `name: acc-inp-rules
version: 1.0.0
description: 验收用来源规则：inp.notice 直接忽略
source_rules:
  - id: inp-notice
    match: {source_type: acc, event_type: [inp.notice]}
    then: ignore
`

// weekday 返回 d 当天或其后最近的 wd。
func weekday(d time.Time, wd time.Weekday) time.Time {
	for d.Weekday() != wd {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// newOpenItem 用 DEL 代本人在 acc-open 建事项并转 open（验收文档 ACT-INP-03、10 的前置）。
func (k *kit) newOpenItem(title string) string {
	k.t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	k.must(json.Unmarshal([]byte(k.apa(k.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--project", k.Projects["acc-open"], "--evidence", "记一下").Stdout), &m))
	k.apa(k.DEL, 0, "item", "transition", m.ID, "--to", "open", "--reason", "go", "--evidence", "记一下")
	return m.ID
}

func TestFlowInboundPipeline(t *testing.T) {
	k := newKit(t)

	k.step("ACT-INP-01 登记脚本来源 INP_SRC", func(t *testing.T) {
		k.registerInp()
		raw := k.apa(k.OBS, 0, "source", "show", inpSrc).Stdout
		var m map[string]any
		k.must(json.Unmarshal([]byte(raw), &m))
		eq(t, "type", m["type"], "script")
		eq(t, "source_type", m["source_type"], "acc")
		if dp, _ := m["default_project"].(string); dp != "" {
			t.Errorf("default_project 应为空: %v", m["default_project"])
		}
		// 空文件一个周期后，来源的最近成功时间更新（脚本被拉起过）。
		if !k.WaitFor(30*time.Second, func() bool {
			_, err := readFile(k.queue + "/last-stdin.json")
			return err == nil
		}) {
			t.Fatalf("来源没有被拉取\n%s", k.ServiceLog())
		}
		k.clear()
	})

	k.step("ACT-INP-02 第 1 步去重：同一事件重复到达只处理一次", func(t *testing.T) {
		k.screenCls("inp-dup", 0.90)
		k.draft("inp-dup 请审阅文档", map[string]any{"title": "inp-dup 起草标题", "kind": "task"})
		k.clear()
		k.emit("inp-dup-1", "inp-dup 请审阅文档")
		k.waitEvent(inpSrc, "inp-dup-1", wait)
		k.emit("inp-dup-1", "inp-dup 请审阅文档")
		quiet()
		if n := len(k.byEventID(inpSrc, "inp-dup-1")); n != 1 {
			t.Errorf("来源事件列表里 id=inp-dup-1 应只有一条，实际 %d", n)
		}
		eq(t, "建议流里\"inp-dup 起草标题\"条数", k.countTitled("inp-dup 起草标题"), 1)
		eq(t, "CLS_LOG 含 inp-dup 的题数", len(k.clsAbout("inp-dup")), 1)
		eq(t, "MODEL_LOG 含 inp-dup 的起草请求数", len(k.modelAbout("inp-dup", false)), 1)
	})

	var t1 string
	k.step("ACT-INP-03 第 2 步归属：命中关联键的事件挂靠到原事项，不进初筛与起草", func(t *testing.T) {
		t1 = k.newOpenItem("inp-thread 合同修订")
		k.apa(k.DEL, 0, "item", "relate", t1, "--key", "inp-thread-1", "--evidence", "这个线程归到合同修订")
		before := len(k.proposed())
		k.clear()
		k.emit("inp-att-1", "inp-att 回复：合同第二版", func(e *Ev) { e.Keys = []string{"inp-thread-1"} })
		ev := k.waitEvent(inpSrc, "inp-att-1", wait)
		eq(t, "disposition", ev.Disposition, "attached")
		eq(t, "item", ev.Item, t1)
		var msgs struct {
			Messages []json.RawMessage `json:"messages"`
		}
		k.must(json.Unmarshal([]byte(k.APIRaw("GET", "/v1/items/"+t1+"/messages", "", k.OBS).Body), &msgs))
		if len(msgs.Messages) != 1 || !strings.Contains(string(msgs.Messages[0]), "inp-att") {
			t.Errorf("T1 的消息列表里应有这条消息: %v", msgs.Messages)
		}
		eq(t, "建议流条数", len(k.proposed()), before)
		quiet()
		eq(t, "CLS_LOG 含 inp-att 的批次", len(k.clsAbout("inp-att")), 0)
		eq(t, "MODEL_LOG 含 inp-att 的请求", k.modelAny("inp-att"), 0)
	})

	k.step("ACT-INP-04 第 3 步规则先于判断：规则给出明确出口时不调模型", func(t *testing.T) {
		k.writePack("acc-inp-rules", inpRules)
		k.clear()
		k.emit("inp-rule-1", "inp-rule 系统通知", func(e *Ev) { e.Type = "inp.notice" })
		ev := k.waitEvent(inpSrc, "inp-rule-1", wait)
		eq(t, "disposition", ev.Disposition, "ignored")
		eq(t, "judgment", ev.Judgment, "")
		quiet()
		eq(t, "CLS_LOG 含 inp-rule 的批次", len(k.clsAbout("inp-rule")), 0)
		eq(t, "MODEL_LOG 含 inp-rule 的请求", k.modelAny("inp-rule"), 0)
		k.removePack("acc-inp-rules")
	})

	k.step("ACT-INP-05 第 4 步初筛低于下限：忽略并留痕，不起草", func(t *testing.T) {
		k.screenCls("inp-low", 0.05)
		before := len(k.proposed())
		k.clear()
		k.emit("inp-low-1", "inp-low 促销邮件")
		ev := k.waitEvent(inpSrc, "inp-low-1", wait)
		eq(t, "disposition", ev.Disposition, "ignored")
		j := k.judgment(ev.Judgment)
		if len(j.Levels) != 1 || j.Levels[0].Backend != "classifier" || j.Levels[0].PYes == nil || *j.Levels[0].PYes != 0.05 || j.DecidedBy != "classifier" {
			t.Errorf("判断记录应只有分类端点一级、p_yes 0.05、由分类端点定案: %+v", j)
		}
		if n := len(k.clsAbout("inp-low")); n != 1 {
			t.Errorf("CLS_LOG 应有这道题，实际 %d", n)
		}
		quiet()
		eq(t, "MODEL_LOG 含 inp-low 的请求", k.modelAny("inp-low"), 0)
		eq(t, "建议流条数", len(k.proposed()), before)
	})

	k.step("ACT-INP-06 第 4、5、6 步：初筛为是的事件被完整起草后进入 proposed", func(t *testing.T) {
		k.screenCls("inp-high", 0.90)
		deadline := weekday(time.Now().AddDate(0, 0, 8), time.Friday).Format("2006-01-02")
		planned := weekday(time.Now(), time.Thursday).Format("2006-01-02")
		k.draft("inp-high 请在周五前审阅季度报告", map[string]any{"title": "审阅 inp-high 季度报告", "summary": "对方请你周五前审阅季度报告",
			"kind": "task", "project": k.Projects["acc-work"], "deadline": deadline, "planned_for": planned,
			"recommend": "accept", "reason": "对方明确提出请求"})
		k.clear()
		k.emit("inp-high-1", "inp-high 请在周五前审阅季度报告")
		ev := k.waitEvent(inpSrc, "inp-high-1", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "phase", it.Phase, "proposed")
		eq(t, "title", it.Title, "审阅 inp-high 季度报告")
		eq(t, "summary", it.Summary, "对方请你周五前审阅季度报告")
		eq(t, "kind", it.Kind, "task")
		eq(t, "project", it.Project, k.Projects["acc-work"])
		eq(t, "deadline", strings.TrimSuffix(strings.SplitN(it.Deadline, "T", 2)[0], ""), deadline)
		eq(t, "planned_for", strings.SplitN(it.PlannedFor, "T", 2)[0], planned)
		if p := it.Proposal; p == nil || p.Recommend != "accept" || p.Reason != "对方明确提出请求" || p.LowConfidence || len(p.Sources) != 1 || p.Sources[0] != ev.ID {
			t.Errorf("proposal 字段与起草结果不符: %+v", it.Proposal)
		}
		if n := len(k.modelAbout("inp-high", true)); n != 0 {
			t.Errorf("MODEL_LOG 里不应有 inp-high 的判断题（分类端点已定案），实际 %d", n)
		}
		eq(t, "起草请求数", len(k.modelAbout("inp-high", false)), 1)
		// 建议流（Web 页面读的 GET /v1/items?phase=proposed）显示该条。
		found := false
		for _, p := range k.proposed() {
			found = found || p.ID == ev.Item
		}
		if !found {
			t.Error("建议流应显示该条")
		}
	})

	k.step("ACT-INP-07 把握低的不丢：各级未定时留为建议并标\"把握低\"，仍被起草", func(t *testing.T) {
		k.screenCls("inp-unsure", 0.35)
		k.chatJudge("inp-unsure", "no", false)
		k.draft("inp-unsure 也许相关的通知", map[string]any{"title": "inp-unsure 起草标题", "kind": "task", "recommend": "dismiss", "reason": "可能无关"})
		k.clear()
		k.emit("inp-unsure-1", "inp-unsure 也许相关的通知")
		ev := k.waitEvent(inpSrc, "inp-unsure-1", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "title", it.Title, "inp-unsure 起草标题")
		if it.Proposal == nil || !it.Proposal.LowConfidence {
			t.Errorf("proposal.low_confidence 应为 true: %+v", it.Proposal)
		}
		j := k.judgment(ev.Judgment)
		if len(j.Levels) != 2 || j.Levels[0].Backend != "classifier" || j.Levels[0].PYes == nil || *j.Levels[0].PYes != 0.35 ||
			j.Levels[1].Answer != "no" || j.Levels[1].Sufficient == nil || *j.Levels[1].Sufficient || j.DecidedBy != "default" || !j.LowConfidence {
			t.Errorf("判断记录应显示两级（分类端点 0.35 → 对话端点 no 且资料不足），保守默认定案: %+v", j)
		}
	})

	k.step("ACT-INP-08 起草失败不挡路：生成端点报错时建议仍进入建议流", func(t *testing.T) {
		k.screenCls("inp-draftfail", 0.90)
		k.ModelStub.When("inp-draftfail 原始标题", failReply(500))
		k.clear()
		k.emit("inp-draftfail-1", "inp-draftfail 原始标题", func(e *Ev) { e.Summary = "原始摘要" })
		ev := k.waitEvent(inpSrc, "inp-draftfail-1", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "phase", it.Phase, "proposed")
		eq(t, "title", it.Title, "inp-draftfail 原始标题")
		eq(t, "summary", it.Summary, "")
		eq(t, "kind", it.Kind, "task")
		eq(t, "project", it.Project, "")
		if len(k.modelAbout("inp-draftfail", false)) == 0 {
			t.Error("生成端点应收到过起草请求（并返回 500）")
		}
	})

	k.step("ACT-INP-09 起草输出不合 schema 视同起草失败", func(t *testing.T) {
		k.screenCls("inp-badschema", 0.90)
		k.ModelStub.When("inp-badschema 原始标题", replyJSON(`{"kind":"foo","summary":"不该被采纳的摘要"}`))
		k.clear()
		k.emit("inp-badschema-1", "inp-badschema 原始标题")
		ev := k.waitEvent(inpSrc, "inp-badschema-1", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "title", it.Title, "inp-badschema 原始标题")
		eq(t, "summary", it.Summary, "")
		eq(t, "kind", it.Kind, "task")
	})

	k.step("ACT-INP-10 起草从内核给的候选清单里推荐\"补到已有事项\"", func(t *testing.T) {
		t2 := k.newOpenItem("inp-cand 供应商合同")
		k.screenCls("inp-cand", 0.90)
		k.draft("inp-cand 供应商合同第三版", map[string]any{"title": "inp-cand 起草标题", "kind": "task",
			"recommend": "attach", "attach_to": t2, "reason": "与已有的供应商合同是同一件事"})
		before := k.item(t2)
		k.clear()
		k.emit("inp-cand-1", "inp-cand 供应商合同第三版")
		ev := k.waitEvent(inpSrc, "inp-cand-1", wait)
		reqs := k.modelAbout("inp-cand 供应商合同第三版", false)
		if len(reqs) != 1 || !strings.Contains(reqs[0], "candidates") || !strings.Contains(reqs[0], t2) {
			t.Fatalf("起草请求的结构化输入里应有含 T2 的候选清单: %v", reqs)
		}
		p := k.item(ev.Item).Proposal
		if p == nil || p.Recommend != "attach" || p.AttachTo != t2 {
			t.Errorf("proposal 应记 recommend=attach、attach_to=T2: %+v", p)
		}
		after := k.item(t2)
		eq(t, "T2 阶段", after.Phase, before.Phase)
		eq(t, "T2 版本", after.Version, before.Version)
		eq(t, "T2 阶段仍为 open", after.Phase, "open")
	})

	k.step("ACT-INP-11 起草不能编造候选清单以外的事项 id", func(t *testing.T) {
		k.screenCls("inp-fake", 0.90)
		k.draft("inp-fake 某个通知", map[string]any{"title": "inp-fake 起草标题", "kind": "task", "recommend": "attach", "attach_to": "itm-does-not-exist", "reason": "编造"})
		k.clear()
		k.emit("inp-fake-1", "inp-fake 某个通知")
		ev := k.waitEvent(inpSrc, "inp-fake-1", wait)
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "phase", it.Phase, "proposed")
		if it.Proposal != nil && it.Proposal.AttachTo == "itm-does-not-exist" {
			t.Errorf("建议记下了编造的事项 id: %+v", it.Proposal)
		}
		if it.Proposal != nil && it.Proposal.Recommend == "attach" {
			t.Errorf("不应留下指向不存在事项的补到推荐: %+v", it.Proposal)
		}
	})

	k.step("ACT-INP-12 内置 inbound-triage 使用三个新模板名", func(t *testing.T) {
		for name, body := range map[string]string{"apa pack list": k.apa(k.OBS, 0, "pack", "list").Stdout, "GET /v1/packs": k.APIRaw("GET", "/v1/packs", "", k.OBS).Body} {
			var out struct {
				Packs []struct {
					Name, Source string
				} `json:"packs"`
			}
			k.must(json.Unmarshal([]byte(body), &out))
			loaded := false
			for _, p := range out.Packs {
				loaded = loaded || (p.Name == "inbound-triage" && p.Source == "builtin")
			}
			if !loaded {
				t.Errorf("%s 应列出已加载的内置 inbound-triage: %s", name, body)
			}
		}
		var hv struct {
			Signals []struct {
				ID string
				OK bool
			}
		}
		k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "health").Stdout), &hv))
		for _, s := range hv.Signals {
			if s.ID == "pack_rejections" && !s.OK {
				t.Errorf("内置包应无校验错误: %+v", s)
			}
		}
		// 起草请求带内置 triage-draft 模板的正文（本步骤之前所有起草请求都必须带它）。
		draftFile, err := os.ReadFile(filepath.Join(repoRoot(), "packs", "inbound-triage", "prompts", "triage-draft.md"))
		k.must(err)
		first := strings.TrimSpace(strings.SplitN(strings.SplitN(string(draftFile), "\n---\n", 2)[1], "\n", 2)[0])
		drafts := 0
		for _, r := range k.ModelStub.Requests() {
			if baseline.IsJudgeRequest(r) || !strings.Contains(r.BodyText, "attach_to") { // 其余生成请求是事项摘要等系统调用
				continue
			}
			drafts++
			if !strings.Contains(plain(r.BodyText), first) {
				t.Errorf("生成请求没有带 triage-draft 的提示词 %q: %s", first, r.BodyText)
			}
		}
		if drafts == 0 {
			t.Error("应已有起草请求")
		}
		// ACT-INP-05..07 的判断记录带模板名：初筛用 triage-screen，没有 v0.1 的单一判断模板。
		for _, id := range []string{"inp-low-1", "inp-unsure-1"} {
			ev := k.byEventID(inpSrc, id)[0]
			var raw map[string]any
			k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "judgment", "show", ev.Judgment).Stdout), &raw))
			tpl, _ := raw["template"].(string)
			if !strings.HasPrefix(tpl, "inbound-triage/triage-screen@") { // 不是 v0.1 的单一判断模板
				t.Errorf("%s 的初筛模板应为 triage-screen: %q", id, tpl)
			}
		}
	})
}

// TestFlowInboundUpgradeKeepsOldProposals 是 ACT-INP-13、14：v0.1 已生成的建议升级后不重新起草。
func TestFlowInboundUpgradeKeepsOldProposals(t *testing.T) {
	k := newKit(t)
	var (
		fx   = k.LedgerFixture(2)
		p0   string
		base string
	)
	obs, del := fx.M.Tokens["observer"], fx.M.Tokens["delegate"]

	k.step("ACT-INP-13 在 v0.1 账本副本上准备一条旧建议", func(t *testing.T) {
		v01 := k.V01Bin()
		srv, stop := k.serveHome(v01, fx.Home)
		out, code := k.cli(v01, srv, del, "propose", "--kind", "task", "--title", "v01-prop 旧建议", "--body", "旧正文", "--evidence", "升级演练前的旧建议")
		if code != 0 {
			t.Fatalf("v0.1 apa propose 退出码 %d: %s", code, out)
		}
		var m struct {
			ID string `json:"id"`
		}
		k.must(json.Unmarshal([]byte(out), &m))
		p0 = m.ID
		if _, code := k.cli(v01, srv, obs, "item", "show", p0); code != 0 {
			t.Fatalf("v0.1 内核里应能读到 P0")
		}
		stop()
	})

	k.step("ACT-INP-14 升级后 v0.1 已生成的建议不重新起草", func(t *testing.T) {
		var stop func()
		base, stop = k.serveHome(k.Bin["apa"], fx.Home)
		defer stop()
		// 夹具内核也接 CLS_STUB、MODEL_STUB：若旧建议被重新起草，两个替身日志里会留下请求。
		code, body, cookies := k.fixtureHTTP(base, "POST", "/v1/auth/login", "", `{"password":"`+fx.M.WebSecret+`"}`, nil)
		var login struct {
			CSRF string `json:"csrf_token"`
		}
		if code != 200 || json.Unmarshal([]byte(body), &login) != nil || len(cookies) == 0 {
			t.Fatalf("夹具 Web 登录失败: %d %s", code, body)
		}
		hdr := map[string]string{"Cookie": cookies[0].Name + "=" + cookies[0].Value, "X-CSRF-Token": login.CSRF}
		ep, _ := json.Marshal(map[string]any{
			"judge_endpoint":       map[string]any{"url": k.Cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1", "locality": "local"},
			"generate_endpoint":    map[string]any{"url": k.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": "local"},
			"screening_timeout_ms": 2000,
		})
		if code, body, _ := k.fixtureHTTP(base, "PUT", "/v1/settings/model-endpoints", "", string(ep), hdr); code != 200 {
			t.Fatalf("夹具接线端点: %d %s", code, body)
		}
		k.clear()
		eventsBefore := k.fixtureHTTP2(base, "/v1/audit/events?limit=500&object="+p0, hdr)
		time.Sleep(130 * time.Second) // 至少两个维护循环周期
		var it struct {
			Item itemView `json:"item"`
		}
		_, raw, _ := k.fixtureHTTP(base, "GET", "/v1/items/"+p0, obs, "", nil)
		k.must(json.Unmarshal([]byte(raw), &it))
		eq(t, "P0 阶段", it.Item.Phase, "proposed")
		eq(t, "P0 标题", it.Item.Title, "v01-prop 旧建议")
		if it.Item.Proposal != nil && (it.Item.Proposal.Recommend != "" || it.Item.Proposal.Reason != "" || len(it.Item.Proposal.Sources) != 0) {
			t.Errorf("P0 不应有升级后的起草结果: %+v", it.Item.Proposal)
		}
		if !strings.Contains(raw, "旧正文") {
			t.Errorf("P0 正文应不变: %s", raw)
		}
		eq(t, "含 v01-prop 的 triage-draft 请求数", k.modelAny("v01-prop"), 0)
		eq(t, "含 v01-prop 的 CLS 批次数", len(k.clsAbout("v01-prop")), 0)
		if after := k.fixtureHTTP2(base, "/v1/audit/events?limit=500&object="+p0, hdr); after != eventsBefore {
			t.Errorf("P0 的 Event 在升级后不应新增起草相关记录:\nbefore=%s\nafter=%s", eventsBefore, after)
		}
	})
}
