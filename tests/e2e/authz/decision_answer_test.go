//go:build e2e

package authz

import (
	"strings"
	"sync"
	"testing"
)

// TestFlowDecisionAnswer 按验收文档 flow-decision-answer 的顺序执行 ACT-DEC-01..09：决策请求进入待办流，本人可在 Web 回答，
// 档位放开的 Agent 可以替本人回答（二档，记成 Agent 自己做的、可按次撤回），只整理的得 23，只认第一次回答。任一动作未通过即停止。
func TestFlowDecisionAnswer(t *testing.T) {
	k := newKit(t)
	// workItem 是 DEL 代本人建的、分配给 work-host.cli 的 open 事项；ask 由 WRK 提出。
	workItem := func(title string) string {
		return k.idOf("item add", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", workHost,
			"--project", k.Projects["acc-work"]))
	}
	askBy := func(id string, extra ...string) {
		k.expect("ask", k.Apa(k.WRK, "", append([]string{"ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text"}, extra...)...), 0)
	}
	pending := func() string { return k.Apa(k.OBS, "", "inbox").Stdout }

	k.step("ACT-DEC-01 worker 为自己的事项提交决策请求", func(t *testing.T) {
		id := workItem("dec-1")
		k.expect("ask", k.Apa(k.WRK, "", "ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text"), 0)
		eq(t, "阶段", k.phase(id), "needs_input")
		a := k.ask(id)
		if a["question"] != "选哪个方案" || a["recommend"] != "A" || a["free_text"] != true || len(a["options"].([]any)) != 2 {
			t.Errorf("决策请求字段不全: %v", a)
		}
		if !strings.Contains(pending(), id) {
			t.Error("决策待办里没有它")
		}
		// 一次只有一个问题：未回答时再提一个问题被拒绝，原问题不变。
		if r := k.Apa(k.WRK, "", "ask", id, "--question", "第二个问题", "--option", "X=x"); r.Code == 0 {
			t.Error("同一事项同时存在两个问题")
		}
		eq(t, "原问题", k.ask(id)["question"], "选哪个方案")
	})

	k.step("ACT-DEC-02 本人在 Web 回答，阶段按选项声明前进", func(t *testing.T) {
		plain := workItem("dec-2a")
		askBy(plain)
		k.webOK("POST", "/v1/items/"+plain+"/decide", `{"choice":"A"}`, 200)
		r := k.resolution(plain)
		eq(t, "choice", r["choice"], "A")
		eq(t, "resolved_via", r["resolved_via"], "web")
		eq(t, "answered_by", r["answered_by"], "owner")
		eq(t, "未声明时回到 open", k.phase(plain), "open")
		if strings.Contains(pending(), plain) {
			t.Error("回答后应离开决策待办")
		}
		declared := workItem("dec-2b")
		askBy(declared, "--next", "A=done")
		k.webOK("POST", "/v1/items/"+declared+"/decide", `{"choice":"A"}`, 200)
		eq(t, "按选项声明前进", k.phase(declared), "done")
	})

	// 2026-11-20 改写（裁决 2：二档全开含回答问题）：档位放开的 Agent 替本人回答，记成 Agent 自己做的，可在 Agent 改动里按次撤回。
	k.step("ACT-DEC-03 档位放开的 Agent 代答，可见且可按次撤回", func(t *testing.T) {
		id := workItem("dec-3")
		askBy(id)
		k.expect("Agent decide", k.Apa(k.DEL, "", "decide", id, "--option", "B"), 0)
		eq(t, "choice", k.resolution(id)["choice"], "B")
		eq(t, "resolved_via", k.resolution(id)["resolved_via"], "agent")
		ev := k.lastEvent(id, "decision.resolved")
		if ev.Actor["type"] != "agent" || ev.Actor["on_behalf_of"] != nil || ev.Evidence != "" || ev.Basis["via"] != "allowance" || ev.Basis["agent"] != k.AgentID {
			t.Errorf("代答的 Event 应记 Agent 自己与它的档位依据: %+v", ev)
		}
		found := false
		for _, c := range k.agentChanges(id) {
			found = found || c.Seq == ev.Seq
		}
		if !found {
			t.Fatal("“Agent 改动”视图里没有这次代答")
		}
		k.webOK("POST", "/v1/items/"+id+"/revert", `{"event":`+itoa(ev.Seq)+`}`, 200)
		eq(t, "撤回后阶段", k.phase(id), "needs_input")
		if k.resolution(id) != nil {
			t.Error("撤回后回答应清除")
		}
	})

	k.step("ACT-DEC-04 档位未放开的代答被拒绝", func(t *testing.T) {
		id := workItem("dec-4")
		askBy(id)
		k.expect("只整理", k.run(k.DELO, selfReported, "decide", id, "--option", "A"), exitBasis)
		eq(t, "阶段", k.phase(id), "needs_input")
		if k.resolution(id) != nil {
			t.Error("决策不应生效")
		}
		if rej := k.lastRejection(id); rej == nil || rej["code"] != "tier_closed" {
			t.Errorf("决策待办应标明这次被拒绝的代答: %v", rej)
		}
	})

	// worker 在自己的事项上在范围内，但它的钥匙不归属任何 Agent，档位恒为只整理：23（2026-11-20 前为 4）。
	k.step("ACT-DEC-05 未归属 Agent 的 worker 不能回答决策", func(t *testing.T) {
		id := workItem("dec-5")
		askBy(id)
		v := k.version(id)
		k.expect("worker decide", k.run(k.WRK, nil, "decide", id, "--option", "A"), exitBasis)
		if k.version(id) != v || k.phase(id) != "needs_input" || k.resolution(id) != nil {
			t.Error("事项不应变化")
		}
	})

	k.step("ACT-DEC-06 只认第一次回答", func(t *testing.T) {
		first := workItem("dec-6a")
		askBy(first)
		k.expect("先到", k.run(k.DEL, nil, "decide", first, "--option", "A"), 0)
		late := k.run(k.DEL, nil, "decide", first, "--option", "B")
		k.expect("后到", late, 6)
		// 已有结果由 next 指向 ask.resolution，第一次回答原样保留。
		if b := k.errBody(late); b["code"] != "conflict" || !strings.Contains(asString(b["next"]), "ask.resolution") {
			t.Errorf("后到者应返回冲突并指向已有结果: %s", late.Stderr)
		}
		eq(t, "结果未被覆盖", k.resolution(first)["choice"], "A")

		race := workItem("dec-6b")
		askBy(race)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, opt := range []string{"A", "B"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = k.run(k.DEL, nil, "decide", race, "--option", opt).Code
			}()
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, c := range codes {
			switch c {
			case 0:
				ok++
			case 6:
				conflict++
			}
		}
		if ok != 1 || conflict != 1 {
			t.Errorf("并发代答应恰好一个成功一个退出码 6，实际 %v", codes)
		}
		if n := len(k.auditOf(race, "decision.resolved")); n != 1 {
			t.Errorf("decision.resolved 应恰好一条，实际 %d", n)
		}
	})

	k.step("ACT-DEC-07 回答“别的想法”", func(t *testing.T) {
		id := workItem("dec-7")
		askBy(id)
		k.webOK("POST", "/v1/items/"+id+"/decide", `{"text":"都不选，改用方案 C"}`, 200)
		eq(t, "resolution.text", k.resolution(id)["text"], "都不选，改用方案 C")
		ev := k.lastEvent(id, "decision.resolved")
		if !strings.Contains(strings.Join(mapStrings(ev.Payload), " "), "方案 C") {
			t.Errorf("Event 里查不到文本: %+v", ev)
		}
	})

}

func mapStrings(m map[string]any) []string {
	var out []string
	for _, v := range m {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			out = append(out, mapStrings(x)...)
		}
	}
	return out
}
