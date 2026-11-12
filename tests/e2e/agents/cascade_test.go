//go:build e2e

package agents

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const (
	judgeMark = "这条来源事件是否值得成为建议"          // inbound-triage/triage.md（判断型）
	titleMark = "为这条来源事件起一个不超过 40 字的建议标题" // inbound-triage/triage-title.md（生成型）
)

// proposals 返回 proposed 阶段的事项 id → 标题。
func (f *fx) proposals() map[string]string {
	f.t.Helper()
	var out struct {
		Items []struct{ ID, Title, Phase, Kind string } `json:"items"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list", "--phase", "proposed").Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	m := map[string]string{}
	for _, it := range out.Items {
		m[it.ID] = it.Title
	}
	return m
}

func (f *fx) judgeReq(choice string, conf float64) modelstub.Rule {
	b, _ := json.Marshal(map[string]any{"choice": choice, "confidence": conf})
	return modelstub.Rule{Match: judgeMark, Reply: reply(string(b))}
}

// itemTitles 返回全部阶段的事项 id → 标题（规则可能很快把建议改成别的阶段）。
func (f *fx) itemTitles() map[string]string {
	f.t.Helper()
	var out struct {
		Items []struct{ ID, Title string } `json:"items"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list").Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	m := map[string]string{}
	for _, it := range out.Items {
		m[it.ID] = it.Title
	}
	return m
}

// calls 返回 category 的调用记录数。
func (f *fx) callCount(category string) int { return len(f.agentCalls("", category)) }

// TestFlowAgentCascade 对应 flow-agent-cascade（ACT-CAS-01..10）。
// 判断型与生成型经真实入站流水线（对话捕获 → inbound-triage 的 triage/triage-title）触发，任务型经验收包的事件规则 call_agent 触发。
func TestFlowAgentCascade(t *testing.T) {
	t.Skip("v0.1 判断引擎语义（置信度阈值、escalate_to 级联、follow_up 的 do[]）已被 C.7/C.11 移除；对应引擎由判断引擎与跟进梯度的后续节点重写，届时替换本测试")
	f := setup(t, "")
	f.auto("run-task", "run-gen", "auto-accept-note")
	f.webOK("PATCH", "/projects/"+f.Projects["acc-open"], `{"capture":true}`, 200)
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}

	step("ACT-CAS-01 判断型只写回枚举和置信度", func(t *testing.T) {
		f.ModelStub.Reset()
		f.ModelStub.When(judgeMark, reply(`{"choice":"task","confidence":0.9}`))
		f.capture("acc-open", "CAS01 记得周五前给装修公司回电话", "s1")
		f.waitFor("判断型调用记录", 20*time.Second, func() bool { return f.callCount("judge") == 1 })
		f.waitFor("建议入账", 20*time.Second, func() bool { return len(f.proposals()) == 1 })
		calls := f.agentCalls("", "")
		if len(calls) != 1 {
			t.Fatalf("应只有 1 条专属 agent 调用记录，实际 %+v", calls)
		}
		p := calls[0].Payload
		eq(t, "category", p["category"], "judge")
		eq(t, "actor.type", calls[0].Actor.Type, "agent")
		eq(t, "choice", p["choice"], "task")
		eq(t, "confidence", p["confidence"], 0.9)
		allowed := map[string]bool{"run_id": true, "category": true, "template": true, "fingerprint": true, "reused": true, "state": true, "ref": true, "choice": true, "confidence": true,
			"stats": true} // stats 是模板声明 event_stats 时记入的数值（ACT-CAS-08）
		for k := range p {
			if !allowed[k] {
				t.Errorf("判断型 Event 出现枚举、ref、置信度之外的字段 %q: %v", k, p[k])
			}
		}
		if rs := f.runs(); len(rs) != 0 {
			t.Errorf("判断型不应有子进程 Run: %+v", rs)
		}
		if n := len(f.modelRequests(judgeMark)); n != 1 {
			t.Errorf("模型请求数 = %d，期望 1", n)
		}
	})

	step("ACT-CAS-02 低置信升级到生成型", func(t *testing.T) {
		t.Skip("v0.1 判断引擎语义（置信度阈值、escalate_to 级联、follow_up 的 do[]）已被 C.7/C.11 移除；对应引擎由判断引擎与跟进梯度的后续节点重写，届时替换本测试")
		f.ModelStub.Reset()
		f.ModelStub.When(judgeMark, reply(`{"choice":"task","confidence":0.1}`))
		f.ModelStub.When(titleMark, reply(`{"insufficient_info":false,"title":"CAS02 生成的标题","summary":"一句话摘要"}`))
		before := f.callCount("judge")
		f.capture("acc-open", "CAS02 有个不太确定的事", "s1")
		f.waitFor("两级调用与结果写回", 20*time.Second, func() bool {
			for _, title := range f.proposals() {
				if title == "CAS02 生成的标题" {
					return true
				}
			}
			return false
		})
		judge, gen := f.agentCalls("", "judge"), f.agentCalls("", "generate")
		if len(judge) != before+1 || len(gen) != 1 {
			t.Fatalf("应有 1 条新的判断型和 1 条生成型记录，实际 judge=%d(原 %d) generate=%d", len(judge), before, len(gen))
		}
		if judge[len(judge)-1].Seq >= gen[0].Seq {
			t.Error("判断型应先于生成型")
		}
		eq(t, "判断型置信度", judge[len(judge)-1].Payload["confidence"], 0.1)
		eq(t, "生成型模板", gen[0].Payload["template"], "triage-title")
		// apa why 回放这次生成型调用，能看到它的结果。
		out := f.apa(f.DEL, 0, "why", gen[0].Object).Stdout
		if !strings.Contains(out, "CAS02 生成的标题") || !strings.Contains(out, "generate") {
			t.Errorf("apa why 看不到生成型结果: %s", out)
		}
		if rs := f.runs(); len(rs) != 0 {
			t.Errorf("判断型、生成型不应有子进程 Run: %+v", rs)
		}
	})

	// 任务型：验收包的事件规则在事项创建时 call_agent。acc-gen 模板是生成型，信息不足时升级到 acc-task（任务型）。
	insufficient := func() {
		f.ModelStub.Reset()
		f.ModelStub.When("ACCGEN", reply(`{"insufficient_info":true}`))
	}
	// taskRunFor 等触发出的任务型 Run 结束，返回它和目标事项。
	taskRunFor := func(n int) run {
		var r run
		f.waitFor("第 N 个任务型 Run 结束", 30*time.Second, func() bool {
			rs := f.taskRuns()
			if len(rs) < n || rs[n-1].State == "running" || rs[n-1].State == "queued" {
				return false
			}
			r = rs[n-1]
			return true
		})
		return r
	}
	// taskCall 等任务型运行的回执落账（Run 终态先于回执处理），返回那条调用记录。
	taskCall := func(target string) event {
		f.waitFor("任务型回执落账："+target, 10*time.Second, func() bool { return len(f.agentCalls(target, "task")) == 1 })
		return f.agentCalls(target, "task")[0]
	}
	phaseChanges := func(id, to string) int {
		n := 0
		for _, e := range f.events(id, "item.phase_changed") {
			if item, _ := e.Payload["item"].(map[string]any); item["phase"] == to {
				n++
			}
		}
		return n
	}

	step("ACT-CAS-03 信息不足时升级到任务型", func(t *testing.T) {
		insufficient()
		f.mode("receipt")
		trig := f.task("CAS03 需要查证", "acc-gen")
		r := taskRunFor(1)
		eq(t, "任务型 Run 状态", r.State, "succeeded")
		eq(t, "Run 的 harness", r.Harness, harness)
		if r.Target == trig {
			t.Error("任务型应在新建的目标事项上运行")
		}
		eq(t, "生成型已先行调用", len(f.templateCalls(trig, "generate", "acc-gen")), 1)
		call := taskCall(r.Target)
		eq(t, "目标事项经 CLI 回执转 done", f.phase(r.Target), "done")
		eq(t, "转 done 的次数（回执不重复生效）", phaseChanges(r.Target, "done"), 1)
		eq(t, "回执路径", call.Payload["via"], "cli")
		eq(t, "会话 id 取自 stdout 末段", call.Payload["session_id"], "stub-session-1")
	})

	step("ACT-CAS-04 任务型失败交本人，不重试", func(t *testing.T) {
		insufficient()
		f.mode("exit1")
		f.task("CAS04 任务型失败", "acc-gen")
		r := taskRunFor(2)
		eq(t, "Run 状态", r.State, "failed")
		if r.ExitCode == nil || *r.ExitCode != 1 {
			t.Errorf("退出码 = %v，期望 1", r.ExitCode)
		}
		taskCall(r.Target)
		it := f.item(r.Target)
		eq(t, "目标事项阶段", it["phase"], "needs_input")
		ask, _ := it["ask"].(map[string]any)
		if q, _ := ask["question"].(string); !strings.Contains(q, "exit status 1") {
			t.Errorf("needs_input 没有附失败摘要: %v", ask["question"])
		}
		var ids []string
		opts, _ := ask["options"].([]any)
		for _, o := range opts {
			ids = append(ids, o.(map[string]any)["id"].(string))
		}
		eq(t, "选项", strings.Join(ids, ","), "rerun,reassign,drop")
		time.Sleep(3 * time.Second)
		eq(t, "目标事项的 Run 数（不重试）", len(f.runs("--target", r.Target)), 1)
		eq(t, "任务型 Run 总数", len(f.taskRuns()), 2)
	})

	step("ACT-CAS-05 生成型输出不符合 schema 视为失败", func(t *testing.T) {
		f.ModelStub.Reset()
		f.ModelStub.When("ACCGEN", reply(`{"note":"缺少必填的 insufficient_info"}`))
		f.mode("receipt")
		trig := f.task("CAS05 不合规", "acc-gen")
		f.waitFor("生成型失败记录", 20*time.Second, func() bool { return len(f.templateCalls(trig, "generate", "acc-gen")) == 1 })
		calls := f.templateCalls(trig, "generate", "acc-gen")
		eq(t, "state", calls[0].Payload["state"], "failed")
		eq(t, "error", calls[0].Payload["error"], "schema")
		if _, has := calls[0].Payload["result"]; has {
			t.Error("不合规的内容进入了账本记录")
		}
		time.Sleep(2 * time.Second)
		it := f.item(trig)
		eq(t, "触发事项阶段（结果未写回）", it["phase"], "open")
		if it["spec"] != nil {
			t.Errorf("不合规内容被写进 spec: %v", it["spec"])
		}
		eq(t, "任务型 Run 总数（未升级）", len(f.taskRuns()), 2)
		eq(t, "模型请求数（不合规不重试）", len(f.modelRequests("ACCGEN")), 1)
	})

	step("ACT-CAS-06 stdout 末段兜底回执", func(t *testing.T) {
		insufficient()
		f.mode("summary-only")
		f.task("CAS06 只有末段摘要", "acc-gen")
		r := taskRunFor(3)
		eq(t, "Run 状态", r.State, "succeeded")
		eq(t, "回执路径", taskCall(r.Target).Payload["via"], "stdout")
		eq(t, "按末段写回后的阶段", f.phase(r.Target), "done")

		insufficient()
		f.mode("no-receipt")
		f.task("CAS06 都不写", "acc-gen")
		r = taskRunFor(4)
		eq(t, "Run 状态（进程本身成功）", r.State, "succeeded")
		eq(t, "回执路径", taskCall(r.Target).Payload["via"], "failed")
		eq(t, "两者都缺视为失败，事项阶段", f.phase(r.Target), "needs_input")
	})

	// 判断型与生成型的复用与统计：经对话捕获。每次返回新出现的建议 id。
	newProposal := func(text, session string) string {
		known := f.itemTitles()
		f.capture("acc-open", text, session)
		var id string
		f.waitFor("新建议入账："+text, 20*time.Second, func() bool {
			for k := range f.itemTitles() {
				if _, ok := known[k]; !ok {
					id = k
					return true
				}
			}
			return false
		})
		return id
	}

	step("ACT-CAS-07 同一输入复用结果", func(t *testing.T) {
		f.ModelStub.Reset()
		f.ModelStub.When(judgeMark, reply(`{"choice":"task","confidence":0.9}`))
		before := f.callCount("judge")
		newProposal("CAS07 完全相同的输入", "s1")
		newProposal("CAS07 完全相同的输入", "s2") // 会话不同，来源事件不同；模板、结构化输入、资料、模型都相同
		judge := f.agentCalls("", "judge")
		if len(judge) != before+2 {
			t.Fatalf("判断型调用记录 = %d，期望 %d", len(judge), before+2)
		}
		eq(t, "第一次是否复用", judge[len(judge)-2].Payload["reused"], false)
		eq(t, "第二次是否复用", judge[len(judge)-1].Payload["reused"], true)
		eq(t, "模型请求数", len(f.modelRequests(judgeMark)), 1)
	})

	step("ACT-CAS-08 Event 统计进资料段，且不进指纹", func(t *testing.T) {
		f.ModelStub.Reset()
		f.ModelStub.When("CAS08 NOTE", reply(`{"choice":"note","confidence":0.9}`))
		f.ModelStub.When(judgeMark, reply(`{"choice":"task","confidence":0.9}`))
		a, b, c, d := newProposal("CAS08 A", "s1"), newProposal("CAS08 B", "s1"), newProposal("CAS08 C", "s1"), newProposal("CAS08 D", "s1")
		note := newProposal("CAS08 NOTE", "s1") // 规则 auto-accept-note 自动接受，不是本人的决定
		f.waitFor("规则自动接受", 20*time.Second, func() bool { return f.phase(note) == "open" })
		f.webOK("POST", "/items/"+a+"/accept", `{}`, 200)
		f.webOK("POST", "/items/"+b+"/accept", `{}`, 200)
		f.webOK("POST", "/items/"+c+"/dismiss", `{"reason":"不要"}`, 200)

		f.ModelStub.Reset()
		f.ModelStub.When(judgeMark, reply(`{"choice":"task","confidence":0.9}`))
		newProposal("CAS08 E", "s1")
		reqs := f.modelRequests(judgeMark)
		if len(reqs) != 1 || !strings.Contains(reqs[0].BodyText, "accepted: 2") || !strings.Contains(reqs[0].BodyText, "dismissed: 1") {
			t.Fatalf("资料段应含 accepted: 2 与 dismissed: 1（规则自动接受不计）: %+v", reqs)
		}
		judge := f.agentCalls("", "judge")
		stats, _ := judge[len(judge)-1].Payload["stats"].(map[string]any)
		eq(t, "Event 里的 accepted", stats["accepted"], float64(2))
		eq(t, "Event 里的 dismissed", stats["dismissed"], float64(1))

		// 统计变化（再丢弃一条）不使相同输入重新判断。
		f.webOK("POST", "/items/"+d+"/dismiss", `{"reason":"不要"}`, 200)
		newProposal("CAS08 E", "s2")
		judge = f.agentCalls("", "judge")
		last := judge[len(judge)-1].Payload
		eq(t, "统计变化后是否复用", last["reused"], true)
		eq(t, "模型请求数（没有重新判断）", len(f.modelRequests(judgeMark)), 1)
		eq(t, "新统计记入调用 Event", last["stats"].(map[string]any)["dismissed"], float64(2))
	})

	signal := func(id string) (ok bool, detail map[string]any) {
		t.Helper()
		var h struct {
			Signals []struct {
				ID     string          `json:"id"`
				OK     bool            `json:"ok"`
				Detail json.RawMessage `json:"detail"`
			} `json:"signals"`
		}
		if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "health").Stdout), &h); err != nil {
			t.Fatal(err)
		}
		for _, s := range h.Signals {
			if s.ID == id {
				_ = json.Unmarshal(s.Detail, &detail) // 非对象的 detail（数组）视为空
				return s.OK, detail
			}
		}
		t.Fatalf("健康视图没有信号 %s", id)
		return
	}

	step("ACT-CAS-09 模型调用失败退避重试且可见", func(t *testing.T) {
		f.ModelStub.Reset()
		f.ModelStub.SetDefault(modelstub.Reply{Status: 500})
		f.capture("acc-open", "CAS09 模型端点故障时提交", "s1")
		// 卡片起草失败也计入 model_unavailable，信号可能早于判断型的重试结束；以该来源事件进入未判定积压为准。
		backlog := func() float64 {
			ok, d := signal("unjudged_backlog")
			n, _ := d["model_unavailable"].(float64)
			if ok {
				return 0
			}
			return n
		}
		f.waitFor("健康视图出现模型不可用且来源事件未判定", 40*time.Second, func() bool {
			ok, _ := signal("model_unavailable")
			return !ok && backlog() > 0
		})
		eq(t, "退避重试的模型请求数（首次 + 3 次重试）", len(f.modelRequests(judgeMark)), 4)
		eq(t, "故障期间的来源事件留在未判定积压里", backlog(), 1.0)
		// 恢复后：新的判断照常完成；故障期间的事件由后台自动重判（§9.3），本人也可以直接忽略积压，两条路都让积压清空。
		f.ModelStub.Reset()
		f.ModelStub.When(judgeMark, reply(`{"choice":"task","confidence":0.9}`))
		id := newProposal("CAS09 模型恢复后提交", "s1")
		eq(t, "恢复后判断完成的建议阶段", f.phase(id), "proposed")
		f.webOK("POST", "/source-events/ignore", `{"all":true}`, 200)
		// 起草失败的告警要等同一模板之后的真实成功调用才清除（见 kernel/health 测试），本步骤只验判断型：积压清空。
		eq(t, "处理后未判定积压清空", backlog(), 0.0)
	})

	step("ACT-CAS-10 harness 不可用时账本照常", func(t *testing.T) {
		insufficient()
		f.mode("receipt")
		f.webOK("PUT", "/settings/policy", `{"default_harness":""}`, 200) // 取消默认 harness
		runsBefore := len(f.taskRuns())
		trig := f.task("CAS10 harness 不可用", "acc-gen")
		var target string
		f.waitFor("级联终止于 needs_input", 20*time.Second, func() bool {
			for _, id := range f.titled("CAS10 harness 不可用") {
				if id != trig && f.phase(id) == "needs_input" {
					target = id
					return true
				}
			}
			return false
		})
		ask, _ := f.item(target)["ask"].(map[string]any)
		if q, _ := ask["question"].(string); !strings.Contains(q, "harness") {
			t.Errorf("needs_input 没有说明 harness 不可用: %v", ask["question"])
		}
		eq(t, "没有拉起任务型 Run", len(f.taskRuns()), runsBefore)
		// 账本、Web、CLI、判断型、生成型都照常。
		made := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "CAS10 照常建事项", "--phase", "open", "--evidence", "验收")
		eq(t, "CLI 建的事项", f.phase(idOf(t, made.Stdout)), "open")
		if r := f.Web("GET", "/v1/items", ""); r.Code != 200 || !strings.Contains(r.Body, "CAS10 照常建事项") {
			t.Errorf("Web 读不到账本: %d %s", r.Code, r.Body)
		}
		f.ModelStub.Reset()
		f.ModelStub.When(judgeMark, reply(`{"choice":"task","confidence":0.1}`))
		f.ModelStub.When(titleMark, reply(`{"insufficient_info":false,"title":"CAS10 判断与生成照常","summary":"s"}`))
		id := newProposal("CAS10 harness 不可用时的捕获", "s1")
		eq(t, "判断型与生成型照常（生成的标题）", f.itemTitles()[id], "CAS10 判断与生成照常")
		f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	})
}
