//go:build e2e

package decisions

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestFlowProposalTriage 对应验收文档 flow-proposal-triage（ACT-PROP-01..07）。
func TestFlowProposalTriage(t *testing.T) {
	f := setup(t)
	var a string

	t.Run("ACT-PROP-01 observer 以 proposed 新建，进建议流不进待办", func(t *testing.T) {
		a = f.proposal("prop-a")
		eq(t, "prop-a 阶段", f.phase(a), "proposed")
		inbox := f.apa(f.OBS, 0, "inbox").Stdout
		pending, suggestions, _ := strings.Cut(inbox, `"suggestions"`)
		if !strings.Contains(suggestions, a) || strings.Contains(pending, a) {
			t.Errorf("prop-a 应在建议流而不在待办: %s", inbox)
		}
	})

	t.Run("ACT-PROP-02 建议不产生投递", func(t *testing.T) {
		before := f.unread()
		if r := f.APIRaw("GET", "/v1/deliveries/next?wait=1", "", f.CHAN); r.Code != http.StatusNoContent {
			t.Errorf("CHAN 不应领到投递: %d %s", r.Code, r.Body)
		}
		eq(t, "Web 未读数", f.unread(), before)
		ctx := f.apa(f.DEL, 0, "context", "--session", "s1", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		if !strings.Contains(ctx, a) || !strings.Contains(ctx, "阶段 proposed") {
			t.Errorf("context 事项增量应带 prop-a: %s", ctx)
		}
		if pending, _, _ := strings.Cut(f.apa(f.OBS, 0, "inbox").Stdout, `"suggestions"`); strings.Contains(pending, a) {
			t.Error("待决不应包含建议")
		}
	})

	t.Run("ACT-PROP-03 本人在 Web 接受", func(t *testing.T) {
		f.webOK("POST", "/items/"+a+"/accept", `{}`, http.StatusOK)
		eq(t, "阶段", f.phase(a), "open")
		if strings.Contains(f.apa(f.OBS, 0, "inbox").Stdout, a) {
			t.Error("接受后应离开建议流")
		}
		eq(t, "Event via", f.last(a, "item.phase_changed").Actor.Via, "web")
	})

	t.Run("ACT-PROP-04 本人在 Web 丢弃并记原因，可重开", func(t *testing.T) {
		b := f.proposal("prop-b")
		f.webOK("POST", "/items/"+b+"/dismiss", `{"reason":"重复了"}`, http.StatusOK)
		eq(t, "阶段", f.phase(b), "dropped")
		if got := f.last(b, "item.phase_changed"); got.Reason != "重复了" {
			t.Errorf("reason 未记录: %+v", got)
		}
		f.webOK("POST", "/items/"+b+"/transition", `{"to":"open","reason":"重开"}`, http.StatusOK)
		eq(t, "重开后阶段", f.phase(b), "open")
	})

	t.Run("ACT-PROP-05 delegate 附本人原话代为接受", func(t *testing.T) {
		c := f.proposal("prop-c")
		f.apa(f.DEL, 0, "accept", c, "--evidence", "这个接了")
		eq(t, "阶段", f.phase(c), "open")
		ev := f.last(c, "item.phase_changed")
		if ev.Actor.OnBehalfOf != "owner" || ev.Ev != "这个接了" {
			t.Errorf("Event 缺 on_behalf_of 或证据: %+v", ev)
		}
		found := false
		for _, s := range f.agentChanges(c) {
			found = found || s.Seq == ev.Seq
		}
		if !found {
			t.Error("agent 变更视图里没有这次代答")
		}
	})

	t.Run("ACT-PROP-06 / 07 无证据代答和低级别凭据被拒绝", func(t *testing.T) {
		d := f.proposal("prop-d")
		f.apa(f.DEL, 23, "accept", d)
		f.apa(f.DEL, 23, "dismiss", d, "--reason", "重复")
		eq(t, "无证据后阶段", f.phase(d), "proposed")
		f.NoSession = true // 只带原话、不带会话引用，同样是 23
		f.apa(f.DEL, 23, "accept", d, "--evidence", "本人说接受")
		f.NoSession = false
		eq(t, "无会话后阶段", f.phase(d), "proposed")
		for name, tok := range map[string]string{"OBS": f.OBS, "WRK": f.WRK} {
			f.apa(tok, 4, "accept", d, "--evidence", "x")
			f.apa(tok, 4, "dismiss", d, "--reason", "x", "--evidence", "x")
			eq(t, name+" 之后阶段", f.phase(d), "proposed")
		}
	})
}

// TestFlowDecisionAnswer 对应 flow-decision-answer（ACT-DEC-01..07）。
func TestFlowDecisionAnswer(t *testing.T) {
	f := setup(t)

	t.Run("ACT-DEC-01 worker 提交决策请求", func(t *testing.T) {
		id := f.task("dec-1", "acc-work")
		f.ask(id)
		it := f.item(id)
		eq(t, "阶段", it["phase"], "needs_input")
		ask := it["ask"].(map[string]any)
		if ask["question"] != "选哪个方案" || ask["recommend"] != "A" || ask["free_text"] != true || len(ask["options"].([]any)) != 2 {
			t.Errorf("决策请求字段不全: %v", ask)
		}
		if !strings.Contains(f.apa(f.OBS, 0, "inbox").Stdout, id) {
			t.Error("决策待办里没有它")
		}
		// 一次只有一个问题：未回答时再提一个问题被拒绝，原问题不变。
		if r := f.Apa(f.WRK, "", "ask", id, "--question", "第二个问题", "--option", "X=x"); r.Code == 0 {
			t.Error("同一事项同时存在两个问题")
		}
		eq(t, "原问题", f.item(id)["ask"].(map[string]any)["question"], "选哪个方案")
	})

	t.Run("ACT-DEC-02 本人在 Web 回答，阶段按选项声明前进", func(t *testing.T) {
		plain := f.task("dec-2a", "acc-work")
		f.ask(plain)
		f.webOK("POST", "/items/"+plain+"/decide", `{"choice":"A"}`, http.StatusOK)
		r := f.resolution(plain)
		eq(t, "choice", r["choice"], "A")
		eq(t, "resolved_via", r["resolved_via"], "web")
		eq(t, "answered_by", r["answered_by"], "owner")
		eq(t, "未声明时回到 open", f.phase(plain), "open")
		if strings.Contains(f.apa(f.OBS, 0, "inbox").Stdout, plain) {
			t.Error("回答后应离开决策待办")
		}

		declared := f.task("dec-2b", "acc-work")
		f.ask(declared, "--next", "A=done")
		f.webOK("POST", "/items/"+declared+"/decide", `{"choice":"A"}`, http.StatusOK)
		eq(t, "按选项声明前进", f.phase(declared), "done")
	})

	t.Run("ACT-DEC-03 delegate 附证据代答", func(t *testing.T) {
		id := f.task("dec-3", "acc-work")
		f.ask(id)
		f.apa(f.DEL, 0, "decide", id, "--option", "B", "--evidence", "就选方案 B")
		r := f.resolution(id)
		eq(t, "choice", r["choice"], "B")
		eq(t, "resolved_via", r["resolved_via"], "chat")
		ev := f.last(id, "decision.resolved")
		if ev.Actor.OnBehalfOf != "owner" || ev.Ev != "就选方案 B" {
			t.Errorf("Event 缺 on_behalf_of 或证据: %+v", ev)
		}
		found := false
		for _, s := range f.agentChanges(id) {
			found = found || s.Seq == ev.Seq
		}
		if !found {
			t.Error("agent 变更视图里没有这次代答")
		}
	})

	t.Run("ACT-DEC-04/05 无证据代答与 worker 回答被拒绝", func(t *testing.T) {
		id := f.task("dec-4", "acc-work")
		f.ask(id)
		f.apa(f.DEL, 23, "decide", id, "--option", "A")
		eq(t, "无证据后阶段", f.phase(id), "needs_input")
		f.NoSession = true // 只带原话、不带会话引用，同样是 23
		f.apa(f.DEL, 23, "decide", id, "--option", "A", "--evidence", "本人说选 A")
		f.NoSession = false
		eq(t, "无会话后阶段", f.phase(id), "needs_input")
		f.apa(f.WRK, 4, "decide", id, "--option", "A", "--evidence", "x")
		eq(t, "worker 回答后阶段", f.phase(id), "needs_input")
		if f.resolution(id) != nil {
			t.Error("决策不应生效")
		}
	})

	t.Run("ACT-DEC-06 只认第一次回答", func(t *testing.T) {
		id := f.task("dec-6a", "acc-work")
		f.ask(id)
		f.apa(f.DEL, 0, "decide", id, "--option", "A", "--evidence", "先")
		late := f.apa(f.DEL, 6, "decide", id, "--option", "B", "--evidence", "x")
		if !strings.Contains(late.Stderr, "conflict") {
			t.Errorf("后到者应返回冲突说明: %s", late.Stderr)
		}
		eq(t, "结果未被覆盖", f.resolution(id)["choice"], "A")

		race := f.task("dec-6b", "acc-work")
		f.ask(race)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, opt := range []string{"A", "B"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = f.Apa(f.DEL, "", "decide", race, "--option", opt, "--evidence", "并发"+opt).Code
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
		if n := len(f.events(race, "decision.resolved")); n != 1 {
			t.Errorf("decision.resolved 应恰好一条，实际 %d", n)
		}
	})

	t.Run("ACT-DEC-07 回答别的想法", func(t *testing.T) {
		id := f.task("dec-7", "acc-work")
		f.ask(id)
		f.webOK("POST", "/items/"+id+"/decide", `{"text":"都不选，改用方案 C"}`, http.StatusOK)
		eq(t, "resolution.text", f.resolution(id)["text"], "都不选，改用方案 C")
		if ev := f.last(id, "decision.resolved"); !strings.Contains(strings.Join(mapStrings(ev.Payload), " "), "方案 C") {
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
