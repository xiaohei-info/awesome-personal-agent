//go:build e2e

package authz

import (
	"strings"
	"sync"
	"testing"
)

// TestFlowDecisionAnswer 按验收文档 flow-decision-answer 的顺序执行 ACT-DEC-01..09：决策请求进入待办流，本人可在 Web 回答，
// delegate 可在私聊会话附原话或凭授权单代答，只认第一次回答。任一动作未通过即停止。
func TestFlowDecisionAnswer(t *testing.T) {
	k := newKit(t)
	// workItem 是 DEL 代本人建的、分配给 work-host.cli 的 open 事项；ask 由 WRK 提出。
	workItem := func(title string) string {
		return k.idOf("item add", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", workHost,
			"--project", k.Projects["acc-work"], "--evidence", "e2e setup"))
	}
	askBy := func(id string, extra ...string) {
		k.expect("ask", k.Apa(k.WRK, "", append([]string{"ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text"}, extra...)...), 0)
		k.settle(id)
	}
	pending := func() string { return k.Apa(k.OBS, "", "inbox").Stdout }

	k.step("ACT-DEC-01 worker 为自己的事项提交决策请求", func(t *testing.T) {
		id := workItem("dec-1")
		k.expect("ask", k.Apa(k.WRK, "", "ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text"), 0)
		k.settle(id)
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

	k.step("ACT-DEC-03 delegate 在私聊会话附原话代答", func(t *testing.T) {
		id := workItem("dec-3")
		askBy(id)
		k.expect("direct decide", k.run(k.DEL, at("dec-s3", "direct"), "decide", id, "--option", "B", "--evidence", "就选方案 B"), 0)
		eq(t, "choice", k.resolution(id)["choice"], "B")
		eq(t, "resolved_via", k.resolution(id)["resolved_via"], "chat")
		ev := k.lastEvent(id, "decision.resolved")
		if ev.Actor["on_behalf_of"] != "owner" || ev.Evidence != "就选方案 B" || ev.Actor["session"] != "dec-s3" || ev.Actor["session_kind"] != "direct" {
			t.Errorf("Event 缺 on_behalf_of、证据、会话引用或会话类型: %+v", ev)
		}
		found := false
		for _, c := range k.agentChanges(id) {
			found = found || c.Seq == ev.Seq
		}
		if !found {
			t.Error("“agent 变更”视图里没有这次代答")
		}
	})

	k.step("ACT-DEC-04 无本人原话也无授权单的代答被拒绝", func(t *testing.T) {
		id := workItem("dec-4")
		askBy(id)
		k.expect("无依据", k.run(k.DEL, nil, "decide", id, "--option", "A"), exitBasis)
		eq(t, "阶段", k.phase(id), "needs_input")
		if k.resolution(id) != nil {
			t.Error("决策不应生效")
		}
		if rej := k.lastRejection(id); rej == nil || rej["code"] != "basis_required" {
			t.Errorf("决策待办应标明这次被拒绝的代答: %v", rej)
		}
	})

	k.step("ACT-DEC-05 worker 不能回答决策", func(t *testing.T) {
		id := workItem("dec-5")
		askBy(id)
		v := k.version(id)
		k.expect("worker decide", k.run(k.WRK, at("dec-s5", "direct"), "decide", id, "--option", "A", "--evidence", "x"), exitDenied)
		if k.version(id) != v || k.phase(id) != "needs_input" || k.resolution(id) != nil {
			t.Error("事项不应变化")
		}
	})

	k.step("ACT-DEC-06 只认第一次回答", func(t *testing.T) {
		first := workItem("dec-6a")
		askBy(first)
		k.expect("先到", k.run(k.DEL, at("dec-s6", "direct"), "decide", first, "--option", "A", "--evidence", "先"), 0)
		late := k.run(k.DEL, at("dec-s6", "direct"), "decide", first, "--option", "B", "--evidence", "x")
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
				codes[i] = k.run(k.DEL, at("dec-s6", "direct"), "decide", race, "--option", opt, "--evidence", "并发"+opt).Code
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

	k.step("ACT-DEC-08 多人会话附原话的代答被拒", func(t *testing.T) {
		id := workItem("dec-8")
		askBy(id)
		k.rejectedNotDirect("group decide", k.run(k.DEL, at("dec-g8", "group"), "decide", id, "--option", "A", "--evidence", "选 A"))
		eq(t, "阶段", k.phase(id), "needs_input")
		if k.resolution(id) != nil {
			t.Error("决策不应生效")
		}
	})

	k.step("ACT-DEC-09 凭授权单代答不看会话类型", func(t *testing.T) {
		g := k.grant([]string{"decision.answer"}, "acc-work")
		id := workItem("dec-9")
		askBy(id)
		k.expect("unknown + grant", k.run(k.DEL, at("", "unknown"), "decide", id, "--option", "A", "--grant", g), 0)
		eq(t, "resolved_via", k.resolution(id)["resolved_via"], "grant")
		eq(t, "Event 记的授权单", k.lastEvent(id, "decision.resolved").Grant, g)
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
