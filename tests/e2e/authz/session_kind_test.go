//go:build e2e

package authz

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// markerSkd 写在决策的问题与标题里：被拒绝时的错误体与审计不得带出它（ACT-SKD-13）。
const markerSkd = "MARKER-SKD"

// TestFlowSessionKind 按验收文档 flow-session-kind 的顺序执行 ACT-SKD-01..14：凭本人原话的代答只在与本人的私聊会话里被接受，
// 多人、未知或未上报的会话一律按依据不足拒绝，授权单与 agent 自己的工作不受影响。任一动作未通过即停止。
func TestFlowSessionKind(t *testing.T) {
	k := newKit(t)
	k.EnableStubRuns()
	q := "用方案 A 还是 B " + markerSkd
	var d1, d2, d3, d4, p1, p2, i1, i2 string

	k.step("ACT-SKD-01 准备普通决策、建议与本人的事项", func(t *testing.T) {
		d1, d2, d3, d4 = k.decision("acc-open", q), k.decision("acc-open", q), k.decision("acc-open", q), k.decision("acc-open", q)
		p1 = k.idOf("propose", k.Apa(k.DEL, "", "propose", "--kind", "task", "--title", "skd 建议 1", "--project", k.Projects["acc-open"]))
		p2 = k.idOf("propose", k.Apa(k.DEL, "", "propose", "--kind", "task", "--title", "skd 建议 2", "--project", k.Projects["acc-open"]))
		i1 = k.ownItem("acc-open", "skd 本人的事项")
		inbox := k.Apa(k.OBS, "", "inbox")
		k.expect("inbox", inbox, 0)
		for _, id := range []string{d1, d2, d3, d4, p1, p2} {
			if !strings.Contains(inbox.Stdout, id) {
				t.Errorf("inbox 缺 %s: %s", id, inbox.Stdout)
			}
		}
		for _, id := range []string{d1, d2, d3, d4} {
			if a := k.ask(id); a["screening_pending"] == true {
				t.Errorf("%s 仍在初筛中", id)
			}
		}
		eq(t, "SKD-I1 阶段", k.phase(i1), "open")
	})

	k.step("ACT-SKD-02 私聊会话（direct）凭原话代答被接受", func(t *testing.T) {
		k.expect("decide", k.run(k.DEL, at("skd-s1", "direct"), "decide", d1, "--option", "B", "--evidence", "就选方案 B"), 0)
		r := k.resolution(d1)
		eq(t, "choice", r["choice"], "B")
		eq(t, "resolved_via", r["resolved_via"], "chat")
		why := k.Apa(k.OBS, "", "why", d1)
		k.expect("why", why, 0)
		ev := k.lastEvent(d1, "decision.resolved")
		eq(t, "on_behalf_of", ev.Actor["on_behalf_of"], "owner")
		eq(t, "证据原文", ev.Evidence, "就选方案 B")
		eq(t, "session", ev.Actor["session"], "skd-s1")
		eq(t, "会话类型", ev.Actor["session_kind"], "direct")
		eq(t, "harness", ev.Actor["harness"], "acc-stub")
	})

	untouched := func(t *testing.T, id string) {
		t.Helper()
		eq(t, id+" 阶段", k.phase(id), "needs_input")
		if k.resolution(id) != nil {
			t.Errorf("%s 不应被回答", id)
		}
	}

	k.step("ACT-SKD-03 多人会话（group）凭原话代答被拒", func(t *testing.T) {
		b := k.rejectedNotDirect("group decide", k.run(k.DEL, at("skd-s2", "group"), "decide", d2, "--option", "A", "--evidence", "同意 A"))
		if b["why"] == "" || b["web_url"] == nil || b["web_url"] == "" {
			t.Errorf("错误体须带 why 与非空 web_url: %v", b)
		}
		untouched(t, d2)
		rej := k.lastRejection(d2)
		if rej == nil || rej["code"] != "session_not_direct" {
			t.Errorf("决策待办应标明这次被拒绝的代答: %v", rej)
		}
	})

	k.step("ACT-SKD-04 会话类型 unknown 时凭原话代答被拒", func(t *testing.T) {
		k.rejectedNotDirect("unknown decide", k.run(k.DEL, at("skd-s3", "unknown"), "decide", d2, "--option", "A", "--evidence", "同意 A"))
		untouched(t, d2)
	})

	k.step("ACT-SKD-05 未上报会话类型时凭原话代答被拒（等同 unknown）", func(t *testing.T) {
		k.rejectedNotDirect("unreported decide", k.run(k.DEL, at("skd-s4", ""), "decide", d3, "--option", "A", "--evidence", "同意 A"))
		untouched(t, d3)
	})

	k.step("ACT-SKD-06 HTTP 入口同样按 X-APA-Session-Kind 判定", func(t *testing.T) {
		hdr := func(kind string) map[string]string {
			return map[string]string{"X-APA-Evidence": "同意 A", "X-APA-Session": "skd-s5", "X-APA-Session-Kind": kind}
		}
		path := "/v1/items/" + d3 + "/decide"
		g := k.APIRawHeaders("POST", path, `{"choice":"A"}`, k.DEL, hdr("group"))
		var e map[string]any
		if g.Code != http.StatusForbidden || json.Unmarshal([]byte(g.Body), &e) != nil || e["code"] != "session_not_direct" || e["web_url"] == nil || e["web_url"] == "" {
			t.Fatalf("group 请求应被拒: %d %s", g.Code, g.Body)
		}
		// HTTP 状态与其他退出码 23 的情况（依据不足）相同。
		other := k.decision("acc-open", q)
		if n := k.APIRawHeaders("POST", "/v1/items/"+other+"/decide", `{"choice":"A"}`, k.DEL, nil); n.Code != g.Code {
			t.Errorf("无依据请求 HTTP %d，应与 session_not_direct 的 %d 相同", n.Code, g.Code)
		}
		untouched(t, d3)
		d := k.APIRawHeaders("POST", path, `{"choice":"A"}`, k.DEL, hdr("direct"))
		if d.Code != http.StatusOK {
			t.Fatalf("direct 请求应成功: %d %s", d.Code, d.Body)
		}
		eq(t, "resolved_via", k.resolution(d3)["resolved_via"], "chat")
	})

	k.step("ACT-SKD-07 非私聊会话接受建议被拒，建议保持 proposed", func(t *testing.T) {
		k.rejectedNotDirect("group accept", k.run(k.DEL, at("skd-s6", "group"), "accept", p1, "--evidence", "收下"))
		eq(t, "SKD-P1 阶段", k.phase(p1), "proposed")
		if rej := k.lastRejection(p1); rej == nil || rej["code"] != "session_not_direct" {
			t.Errorf("建议流应标明这次被拒绝的处理: %v", rej)
		}
		if n := len(k.auditOf(p1, "item.phase_changed")); n != 0 {
			t.Errorf("被拒后不应有阶段变更: %d", n)
		}
	})

	k.step("ACT-SKD-08 非私聊会话修改本人的事项被拒，事项转 needs_input", func(t *testing.T) {
		title := k.item(i1)["title"]
		k.rejectedNotDirect("group item update", k.run(k.DEL, at("skd-s7", "group"), "item", "update", i1, "--title", "改个标题", "--reason", "x", "--evidence", "帮我改标题"))
		it := k.item(i1)
		eq(t, "标题不变", it["title"], title)
		eq(t, "SKD-I1 阶段", it["phase"], "needs_input")
		a, _ := it["ask"].(map[string]any)
		eq(t, "ask.risk_class", a["risk_class"], "web_only")
		opts, _ := a["options"].([]any)
		if len(opts) != 1 || opts[0].(map[string]any)["label"] != "继续" {
			t.Errorf("内核生成的 ask 选项只能是“继续”: %v", a["options"])
		}
		raw, _ := json.Marshal(a)
		notContains(t, "内核生成的 ask", string(raw), "改个标题")
	})

	k.step("ACT-SKD-09 授权单不受会话类型影响", func(t *testing.T) {
		g := k.grant([]string{"decision.answer"}, "acc-open")
		other := k.decision("acc-open", q)
		k.expect("group + grant", k.run(k.DEL, at("skd-s9", "group"), "decide", d4, "--option", "A", "--grant", g), 0)
		k.expect("unreported + grant", k.run(k.DEL, nil, "decide", other, "--option", "A", "--grant", g), 0)
		for _, id := range []string{d4, other} {
			eq(t, id+" resolved_via", k.resolution(id)["resolved_via"], "grant")
			ev := k.lastEvent(id, "decision.resolved")
			eq(t, "on_behalf_of", ev.Actor["on_behalf_of"], "owner")
			eq(t, "Event 记的授权单", ev.Grant, g)
		}
	})

	k.step("ACT-SKD-10 agent 自己的工作（第一档）在非私聊会话里照常", func(t *testing.T) {
		i2 = k.idOf("item add", k.Apa(k.DEL, "", "item", "add", "--kind", "task", "--title", "skd agent 的事项", "--phase", "open",
			"--assignee", workHost, "--project", k.Projects["acc-open"], "--evidence", "e2e setup"))
		g := at("skd-s10", "group")
		before := len(k.rejections())
		k.expect("propose", k.run(k.WRK, g, "propose", "--kind", "task", "--title", "skd 候选"), 0)
		k.expect("ask", k.run(k.WRK, g, "ask", i2, "--question", "要继续吗", "--option", "yes=是", "--option", "no=否"), 0)
		k.expect("progress", k.run(k.WRK, g, "item", "progress", i2, "--text", "已完成一半"), 0)
		if len(k.rejections()) != before {
			t.Error("第一档命令不应被拒绝")
		}
		for _, e := range k.auditOf(i2, "") {
			// 内核以 system 身份代本人异步起草摘要（§6.3），带 on_behalf_of 是设计，不属于 agent 的写入。
			if p, _ := e.Actor["principal"].(string); strings.HasPrefix(p, "system:") {
				continue
			}
			if _, ok := e.Actor["on_behalf_of"]; ok && e.Actor["principal"] != "" && e.Type != "item.created" {
				t.Errorf("agent 自己的工作不带 on_behalf_of: %+v", e)
			}
		}
	})

	k.step("ACT-SKD-11 读取类调用与委派回执不需要会话类型", func(t *testing.T) {
		k.expect("inbox", k.run(k.DEL, nil, "inbox"), 0)
		ctx := k.run(k.DEL, nil, "context", "--session", "skd-s8", "--scope", "project", "--project", k.Projects["acc-open"], "--format", "inject")
		k.expect("context", ctx, 0)
		if !strings.Contains(ctx.Stdout, "以下为资料") {
			t.Errorf("context 应返回注入内容: %s", ctx.Stdout)
		}
		k.expect("item show", k.run(k.DEL, nil, "item", "show", i2), 0)
		// SKD-10 的 ask 让 SKD-I2 停在 needs_input，而只有 open/waiting 的事项能委派：先由本人在 Web 回答它（文档两步之间的隐含前提）。
		k.webOK("POST", "/v1/items/"+i2+"/decide", `{"choice":"yes"}`, 200)
		eq(t, "SKD-I2 阶段", k.phase(i2), "open")
		child := k.idOf("delegate", k.run(k.DEL, nil, "delegate", "--parent", i2, "--assignee", workHost, "--title", "skd 子事项"))
		k.expect("report", k.run(k.WRK, nil, "report", child, "--outcome", "done", "--summary", "完成", "--pr", "file://"+k.LocalRepo+"#skd"), 0)
		eq(t, "子事项阶段", k.phase(child), "done")
	})

	k.step("ACT-SKD-12 运行凭据即使报 direct 也不能代答", func(t *testing.T) {
		d := k.decision("acc-open", q)
		_, _, env := k.StubRun("hang")
		tok, run := env["APA_TOKEN"], env["APA_RUN"]
		if tok == "" || run == "" {
			t.Fatalf("取不到运行凭据: %v", env)
		}
		r := k.run(tok, append(at("skd-run", "direct"), "APA_RUN="+run), "decide", d, "--option", "A", "--evidence", "同意")
		k.expect("运行凭据 decide", r, exitDenied)
		untouched(t, d)
	})

	k.step("ACT-SKD-13 被拒的代答留痕且错误不带用户内容", func(t *testing.T) {
		var seen = map[string]string{}
		for _, e := range k.auditOf(d2, "command.rejected") {
			seen[asString(e.Actor["session"])] = asString(e.Actor["session_kind"])
			eq(t, "原因", e.Payload["code"], "session_not_direct")
			if e.Actor["principal"] == "" || e.Actor["principal"] == nil {
				t.Errorf("拒绝应记发起凭据: %+v", e)
			}
		}
		if seen["skd-s2"] != "group" || seen["skd-s3"] != "unknown" {
			t.Errorf("D2 的两次拒绝应各有会话引用与会话类型: %v", seen)
		}
		d3rej := k.auditOf(d3, "command.rejected")
		if len(d3rej) == 0 {
			t.Fatal("D3 的拒绝（未上报会话类型）没有 command.rejected")
		}
		e := d3rej[0]
		eq(t, "D3 会话引用", e.Actor["session"], "skd-s4")
		if kind, _ := e.Actor["session_kind"].(string); kind != "" && kind != "unknown" {
			t.Errorf("未上报的会话类型应缺失/unknown: %v", e.Actor)
		}
		eq(t, "D3 原因", e.Payload["code"], "session_not_direct")
		b := k.errBody(k.run(k.DEL, at("skd-s2", "group"), "decide", d2, "--option", "A", "--evidence", "同意 A"))
		for _, f := range []string{"why", "next"} {
			notContains(t, "错误体 "+f, asString(b[f]), markerSkd)
		}
		notContains(t, "apa why", k.Apa(k.OBS, "", "why", d2).Stderr, markerSkd)
	})

	k.step("ACT-SKD-14 插件未上报会话类型时 apa doctor 指出原因", func(t *testing.T) {
		script := filepath.Join(k.Root, "skd-selfcheck.sh")
		write := func(reports bool) {
			body := `#!/bin/sh
echo '{"plugin_version":"0.1.0","hooks":[],"reports_session_kind":` + map[bool]string{true: "true", false: "false"}[reports] + `,"overlaps":[]}'
`
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		decl, _ := json.Marshal(map[string]any{"harness": "acc-stub", "headless": map[string]any{
			"command": []string{k.Bin["stub-harness"], "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
			"access": map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
			"resume": []string{k.Bin["stub-harness"], "--resume", "{prompt}"}},
			"selfcheck": map[string]any{"command": []string{script}}})
		write(false)
		r := k.Apa(k.DEL, string(decl), "declare", "-", "--evidence", "skd selfcheck")
		k.expect("declare", r, 0)
		var dr struct {
			Rev int `json:"rev"`
		}
		_ = json.Unmarshal([]byte(r.Stdout), &dr)
		if dr.Rev == 0 {
			dr.Rev = 2
		}
		k.webOK("POST", "/v1/settings/declarations/acc-stub/"+itoa(dr.Rev)+"/confirm", "", 200)
		k.rejectedNotDirect("插件一侧未上报", k.run(k.DEL, at("skd-s14", ""), "decide", k.decision("acc-open", q), "--option", "A", "--evidence", "同意 A"))

		host := func() (status, why, next, all string) {
			rr := k.Apa("", "", "doctor", "--json")
			var rep struct {
				Checks []struct {
					Name    string                                        `json:"name"`
					Status  string                                        `json:"status"`
					Details []struct{ Subject, Status, Why, Next string } `json:"details"`
				} `json:"checks"`
			}
			if err := json.Unmarshal([]byte(rr.Stdout), &rep); err != nil {
				t.Fatalf("doctor 输出不是 JSON（退出码 %d）: %q %s", rr.Code, rr.Stdout, rr.Stderr)
			}
			for _, c := range rep.Checks {
				if c.Name != "host_plugin" {
					continue
				}
				for _, d := range c.Details {
					if d.Subject == "acc-stub" {
						return d.Status, d.Why, d.Next, rr.Stdout
					}
				}
			}
			t.Fatalf("doctor 没有点名宿主插件 acc-stub: %s", rr.Stdout)
			return
		}
		status, why, next, out := host()
		if status != "warning" && status != "error" {
			t.Errorf("未上报会话类型应给出提醒或错误: %s %s", status, why)
		}
		if !strings.Contains(why, "session type") || !strings.Contains(why, "rejected") || !strings.Contains(next, "upgrade the plugin") {
			t.Errorf("原因应写明未上报会话类型、凭原话的代答被拒，下一步升级插件: why=%q next=%q", why, next)
		}
		notContains(t, "doctor 输出", out, markerSkd)
		write(true)
		status, why, _, out = host()
		eq(t, "上报后宿主插件检查", status, "ok")
		notContains(t, "doctor 输出", out, markerSkd)
		_ = why
	})
}

func asString(v any) string { s, _ := v.(string); return s }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
