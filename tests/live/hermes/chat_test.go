//go:build live

package hermes

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var urlRE = regexp.MustCompile(`https?://[^\s)）」"']+/items/itm_[0-9a-f]+`)

// TestFlowChatAnswer 对应 flow-chat-answer（ACT-CHAT-01..04）：真实 Hermes 主会话代答。
func TestFlowChatAnswer(t *testing.T) {
	f := setup(t, "")
	defer f.flushEvidence("flow-chat-answer-evidence.json")
	open := f.checkout("acc-open")
	home := f.hermesHome("chat", "core", "inject", "tools")
	f.webOK("PUT", "/settings/policy", `{"web_only":{"categories":["external_commitment","funds","irreversible","permission_change","spec_activation"],"rules":[{"tag":"acc-finance","category":"funds"}]}}`, 200)
	// 决策走注入路由：选项经 apa context 的投递条目带进会话（资料信封）。
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"decision":"inject","reminder":"web","brief":"web","health":"web","wake":"web"}}}`, 200)
	var id string
	var s *sess

	f.step(t, "ACT-CHAT-01 开启 delegate token 并准备决策", func(t *testing.T) {
		id = f.ask("acc-open", "CHAT1-DEC-QZK 方案选择", "", "用方案 A 还是 B", "A=方案 A", "B=方案 B")
		f.waitDelivery(id)
		out := f.apa(f.DEL, 0, "inbox").Stdout
		contains(t, "apa inbox", out, id)
	})

	f.step(t, "ACT-CHAT-02 决策经消息前注入带到会话", func(t *testing.T) {
		s = f.session(home, open, f.DEL)
		r := s.say("先别用任何命令。我现在有什么要我拍板的事？把问题和选项都说出来。")
		contains(t, "agent 的回答", r.Final, "方案 A", "方案 B")
		contains(t, "注入信封", r.Injected, "以下为资料", id, "用方案 A 还是 B")
		if n := r.toolCalls("apa"); n != 0 {
			t.Errorf("agent 应不经查询就知道该决策，实际调用了 apa 相关工具: %v", r.Tools)
		}
	})

	f.step(t, "ACT-CHAT-03 聊天表态后代答", func(t *testing.T) {
		s.say("记住：之后凡是要操作 APA 账本，请用 apa_* 工具（它们会带上会话身份），不要用终端。只回复：好")
		r := s.say("就选方案 B")
		var dec *call
		for _, c := range f.calls("decide") {
			if strings.Contains(c.Args, id) {
				c := c
				dec = &c
			}
		}
		if dec == nil {
			t.Fatalf("agent 应调用 apa decide %s；工具调用 %v；回答 %s", id, r.Tools, r.Final)
		}
		contains(t, "apa decide 参数", dec.Args, "--option B", "就选方案 B")
		eq(t, "apa decide 退出码", dec.Code, 0)
		if dec.Session == "" || dec.Session != s.ID {
			t.Errorf("apa decide 应带本会话引用 APA_SESSION=%s，实际 %q", s.ID, dec.Session)
		}
		var resolved *event
		for _, e := range f.events(id, "decision.resolved") {
			e := e
			resolved = &e
		}
		if resolved == nil {
			t.Fatalf("没有 decision.resolved 事件")
		}
		eq(t, "resolved_via", resolved.ResolvedVia, "chat")
		eq(t, "on_behalf_of", fmt.Sprint(resolved.Actor["on_behalf_of"]), "owner")
		contains(t, "Event 证据（本人原话）", resolved.Evidence, "就选方案 B")
		eq(t, "Event 的会话引用", fmt.Sprint(resolved.Actor["session"]), s.ID)
		eq(t, "事项阶段", f.phase(id), "open")
		// 可一键撤销：本人在 Web 撤销这次代答带来的阶段变更，事项回到 needs_input。
		seq := resolved.Seq
		types := []string{}
		for _, e := range f.events(id, "") {
			types = append(types, e.Type)
		}
		rv := f.Web("POST", "/v1/items/"+id+"/revert", fmt.Sprintf(`{"event":%d,"reason":"验收：撤销代答"}`, seq))
		if rv.Code != 200 {
			t.Fatalf("代答后一键撤销失败 HTTP %d %s；该事项的事件序列 %v", rv.Code, rv.Body, types)
		}
		eq(t, "撤销后阶段", f.phase(id), "needs_input")
		f.evidence["ACT-CHAT-03"] = map[string]any{"decide_args": f.redact(dec.Args), "resolved_via": resolved.ResolvedVia, "evidence": resolved.Evidence, "session": resolved.Actor["session"], "decide_call_session": dec.Session, "reverted": true}
	})

	f.step(t, "ACT-CHAT-04 web_only 决策在聊天中被拒绝", func(t *testing.T) {
		fin := f.ask("acc-open", "CHAT4-FIN-QZK 付款确认", "acc-finance", "是否同意付款", "A=同意", "B=拒绝")
		f.waitFor("web_only 决策初筛后进入 apa inbox", 60*1e9, func() bool {
			return strings.Contains(f.apa(f.DEL, 0, "inbox").Stdout, fin) || f.phase(fin) == "needs_input"
		})
		s4 := f.session(home, open, f.DEL)
		r0 := s4.say("记住：之后凡是要操作 APA 账本，请用 apa_* 工具（它们会带上会话身份），不要用终端。先别用任何命令，只回复：好")
		contains(t, "会话开始的注入", r0.Injected, "本会话有待 Web 确认的高风险决策", fin)
		s4.say("先别用任何命令：现在有哪些待我确认的事？一句话说说。")
		r := s4.say("同意")
		var dec *call
		for _, c := range f.calls("decide") {
			if strings.Contains(c.Args, fin) {
				c := c
				dec = &c
			}
		}
		// 注入已声明“本会话有待 Web 确认的高风险决策”，模型可能先尝试 decide（被内核 20 拦下），也可能直接拒绝；
		// 两者都不得让决策生效。是否尝试记入证据，内核的 20 由下面直接确认（本人原话 + 会话引用，仍被拦）。
		attempted := dec != nil
		if attempted {
			eq(t, "web_only 决策 apa decide 退出码", dec.Code, 20)
			if !urlRE.MatchString(r.Final) {
				t.Errorf("agent 应把 Web 链接交给本人: %s", r.Final)
			}
		}
		t.Setenv("APA_SESSION", s4.ID)
		k := f.apa(f.DEL, 20, "decide", fin, "--option", "A", "--evidence", "同意")
		contains(t, "退出码 20 的 code", k.Stderr, `"code":"web_only"`)
		eq(t, "事项阶段（仍待决）", f.phase(fin), "needs_input")
		eq(t, "决策被回答次数", len(f.events(fin, "decision.resolved")), 0)
		f.evidence["ACT-CHAT-04"] = map[string]any{"agent_attempted_decide": attempted, "final": f.redact(r.Final), "kernel_decide_exit": k.Code}
		s4.say("好了吗？")
		eq(t, "再次确认后仍待决", f.phase(fin), "needs_input")
	})
}
