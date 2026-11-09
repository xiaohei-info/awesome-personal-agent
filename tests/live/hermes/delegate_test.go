//go:build live

package hermes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// children 返回父事项下 delegation.created 事件对应的子事项 id（按创建顺序）。
func (f *fx) children(parent string) []string {
	var out []string
	for _, e := range f.events("", "delegation.created") {
		rel, _ := e.Payload["relation"].(map[string]any)
		if rel["to"] == parent && rel["type"] == "parent" {
			out = append(out, e.Object)
		}
	}
	return out
}

// delegate 用 DEL 代本人在账本里建子事项（准备数据用，不经 Hermes）。
func (f *fx) delegate(parent, assignee string, extra ...string) string {
	f.t.Helper()
	args := append([]string{"delegate", "--parent", parent, "--assignee", assignee, "--evidence", "把它交给 " + assignee}, extra...)
	return idOf(f.t, f.apa(f.DEL, 0, args...).Stdout)
}

// TestFlowDelegateRecover 对应 flow-delegate-recover（ACT-DLG-01..11）：真实 Hermes 主会话委派，回执回到原事项。
func TestFlowDelegateRecover(t *testing.T) {
	f := setup(t, "")
	defer f.flushEvidence("flow-delegate-recover-evidence.json")
	work := f.checkout("acc-work")
	home := f.hermesHome("dlg", "core", "inject", "tools", "receipt")
	var s *sess
	var parent, child string

	f.step(t, "ACT-DLG-01 声明执行者", func(t *testing.T) {
		// 清单里同时有已在 Web 登记的 work-host.cli 和本次新声明的 hermes-coder.cli；后者确认前不得生效。
		host, _ := hostname()
		decl, _ := json.Marshal(map[string]any{"harness": harness, "executors": []map[string]any{
			{"id": "hermes-coder.cli", "kind": "external-cli", "scenarios": []string{"coding"}, "host": host}}})
		if r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "Hermes 的执行者声明"); r.Code != 0 {
			t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		listed := func() map[string]bool { // id -> allowed（项目 acc-work）
			var out struct {
				Executors []struct {
					ID      string `json:"id"`
					Allowed bool   `json:"allowed"`
				} `json:"executors"`
			}
			must(t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "executors", "--project", f.Projects["acc-work"]).Stdout), &out))
			m := map[string]bool{}
			for _, e := range out.Executors {
				m[e.ID] = e.Allowed
			}
			return m
		}
		before := listed()
		if _, ok := before["hermes-coder.cli"]; ok {
			t.Errorf("确认前新声明的执行者不应在清单: %v", before)
		}
		f.webOK("POST", "/settings/declarations/"+harness+"/1/confirm", "", 200)
		after := listed()
		if allowed, ok := after["hermes-coder.cli"]; !ok || allowed {
			t.Errorf("确认后应列出 hermes-coder.cli 且因不在 acc-work 名单而 allowed=false: %v", after)
		}
		if !after["work-host.cli"] {
			t.Errorf("acc-work 的名单内执行者 work-host.cli 应标明允许: %v", after)
		}
		f.evidence["ACT-DLG-01"] = map[string]any{"before_confirm": before, "after_confirm": after,
			"note": "work-host.cli 由 Web 预先登记，确认前就在清单中，无法用它验证“确认前不生效”，所以声明 hermes-coder.cli"}
	})

	f.step(t, "ACT-DLG-02 建立委派子事项", func(t *testing.T) {
		parent = f.add("acc-work", "DLG2-PARENT-QZK 修复登录超时", "", "", "")
		s = f.session(home, work, f.DEL)
		r := s.say("把「DLG2-PARENT-QZK 修复登录超时」这件事交给 work-host.cli。现在只在账本里委派，先不要启动执行者。")
		kids := f.children(parent)
		if len(kids) != 1 {
			t.Fatalf("应建立 1 个子事项，实际 %v；agent 回答 %s；工具 %v", kids, r.Final, r.Tools)
		}
		child = kids[0]
		it := f.item(child)
		eq(t, "子事项 assignee", it["assignee"], "work-host.cli")
		eq(t, "子事项 project", it["project"], f.Projects["acc-work"])
		p := f.item(parent)
		eq(t, "父事项阶段", p["phase"], "waiting")
		contains(t, "父事项 waiting_on", f.apa(f.OBS, 0, "item", "show", parent).Stdout, "work-host.cli")
		for _, c := range f.calls("@@EXECUTOR") {
			t.Errorf("委派阶段不应已调用执行者: %+v", c)
		}
	})

	f.step(t, "ACT-DLG-03 执行者拿到项目上下文", func(t *testing.T) {
		// 项目里登记一份自有型材料：标题可识别，正文带标记；上下文只能带标题，不能带正文。
		const matTitle, matBody = "DLG3-MAT-QZK 登录超时排查纪要", "MARKER-BODY-DLG3 这是材料正文"
		mf := filepath.Join(t.TempDir(), "m.md")
		must(t, os.WriteFile(mf, []byte(matBody), 0o600))
		f.apa(f.WRK, 0, "material", "commit", "--kind", "minutes", "--title", matTitle, "--project", f.Projects["acc-work"], "--from", mf, "--reason", "DLG3 登记材料")
		item := f.apa(f.WRK, 0, "context", "--scope", "item", "--item", child).Stdout
		contains(t, "item 作用域（子事项条目）", item, child)
		// 项目信息、repos、允许的执行者属于项目上下文（设计 §7.6），执行者按子事项所在项目读取。
		out := f.apa(f.WRK, 0, "context", "--scope", "project", "--project", f.Projects["acc-work"], "--format", "inject").Stdout
		contains(t, "项目上下文", out, "acc-work", "https://example.invalid/acc/acc-work.git", "work-host.cli")
		contains(t, "相关材料标题", out, matTitle)
		lacks(t, "不带正文", item+out, "MARKER-BODY")
		f.saveEvidence("act-dlg-03-context.txt", item+"\n"+out)
	})

	f.step(t, "ACT-DLG-04 执行者回执并附证据", func(t *testing.T) {
		f.apa(f.WRK, 0, "report", child, "--outcome", "done", "--summary", "已修复", "--pr", "https://example.invalid/acc/pr/42")
		eq(t, "子事项阶段", f.phase(child), "done")
		eq(t, "父事项阶段", f.phase(parent), "open")
		// 不带结果证据的完成回执：退出码 2，子事项不变。
		p4 := f.add("acc-work", "DLG4-PARENT-QZK 无证据完成", "", "", "")
		c4 := f.delegate(p4, "work-host.cli")
		f.apa(f.WRK, 2, "report", c4, "--outcome", "done", "--summary", "已修复")
		eq(t, "无证据完成回执后的子事项阶段", f.phase(c4), "open")
		eq(t, "无证据完成回执后的回执事件数", len(f.events(c4, "delegation.reported")), 0)
		reps := f.events(child, "delegation.reported")
		if len(reps) != 1 || !strings.Contains(toJSON(reps[0].Payload), "pr/42") {
			t.Errorf("事件流应有带 PR 证据的 delegation.reported: %+v", reps)
		}
	})

	f.step(t, "ACT-DLG-05 回收结果注入主会话", func(t *testing.T) {
		r := s.say("刚才委派的事怎么样了")
		if !strings.Contains(r.Final, "完成") && !strings.Contains(strings.ToLower(r.Final), "done") {
			t.Errorf("agent 应知道子事项已完成: %s", r.Final)
		}
		contains(t, "回收结果的注入", r.Injected, child)
		// 证据（PR 链接）不在注入信封里（信封只带 id、标题、阶段）：agent 要能说出来，必要时自己查账本。
		answer := r.Final
		firstHadEvidence := strings.Contains(answer, "pr/42")
		if !firstHadEvidence {
			answer = s.say("回执里附的证据（PR 链接）是什么？用 apa why " + child + " 查回执事件，把链接原样告诉我。").Final
		}
		contains(t, "agent 说出证据", answer, "pr/42")
		f.evidence["ACT-DLG-05"] = map[string]any{"first_answer_had_evidence": firstHadEvidence}
	})

	f.step(t, "ACT-DLG-06 插件捕获回执", func(t *testing.T) {
		p6 := f.add("acc-work", "DLG6-PARENT-QZK 补测试", "", "", "")
		c6 := f.delegate(p6, "work-host.cli")
		before := len(f.events("", "delegation.reported"))
		s6 := f.session(home, work, f.DEL)
		r := s6.say("委派子事项 " + c6 + " 已经建好（它就是要交给执行者的那一条，不要换成别的事项 id，也不要新建）。请用 apa_executor_call（id 填 " + c6 + "，executor 填 work-host.cli）取调用方式，再用终端直接执行那条命令，命令必须以执行者脚本开头、不要加 cd 或别的前缀。不要自己执行 apa report，回执交给插件。")
		f.waitFor("插件写入回执", 90*time.Second, func() bool { return f.phase(c6) == "done" })
		reps := f.events(c6, "delegation.reported")
		if len(reps) != 1 {
			t.Fatalf("应只捕获 1 次回执，实际 %d；回答 %s", len(reps), r.Final)
		}
		contains(t, "插件回执带出的结果证据", toJSON(reps[0].Payload), "pr/6")
		for _, c := range f.calls("report") {
			if strings.Contains(c.Args, c6) && c.Session == "" {
				t.Errorf("agent 不应自行运行 apa report（回执应来自插件捕获）: %+v", c)
			}
		}
		// 再调用一次不带事项 id 的工具：不产生任何回执。
		mid := len(f.events("", "delegation.reported"))
		eq(t, "第一次调用只新增 1 条回执", mid, before+1)
		s6.say("再用 work-host.cli 执行一件和账本无关的小事（没有事项 id）：随便打个招呼。用 apa_executor_call 取调用方式，再用终端执行。")
		time.Sleep(10 * time.Second)
		eq(t, "第二次调用后的回执数", len(f.events("", "delegation.reported")), mid)
	})

	f.step(t, "ACT-DLG-07 其他凭据不能回执", func(t *testing.T) {
		p7 := f.add("acc-work", "DLG7-PARENT-QZK", "", "", "")
		c7 := f.delegate(p7, "work-host.cli")
		f.apa(f.WRK2, 4, "report", c7, "--outcome", "done", "--summary", "冒充回执")
		eq(t, "子事项不变", f.phase(c7), "open")
		eq(t, "回执事件数", len(f.events(c7, "delegation.reported")), 0)
	})

	f.step(t, "ACT-DLG-08 全部子事项终态后父事项才回 open", func(t *testing.T) {
		p8 := f.add("acc-work", "DLG8-PARENT-QZK", "", "", "")
		a, b := f.delegate(p8, "work-host.cli"), f.delegate(p8, "work-host.cli")
		f.apa(f.WRK, 0, "report", a, "--outcome", "done", "--summary", "第一个完成", "--pr", "https://example.invalid/acc/pr/8")
		f.apa(f.WRK, 0, "ask", b, "--question", "需要本人决定", "--option", "A=继续", "--option", "B=放弃")
		eq(t, "一个 done 一个 needs_input 时父事项", f.phase(p8), "waiting")
		f.apa(f.DEL, 0, "item", "transition", b, "--to", "dropped", "--reason", "不做了", "--evidence", "不做了")
		eq(t, "两者都到终态后父事项", f.phase(p8), "open")
	})

	f.step(t, "ACT-DLG-09 回执阻塞", func(t *testing.T) {
		p9 := f.add("acc-work", "DLG9-PARENT-QZK", "", "", "")
		c9 := f.delegate(p9, "work-host.cli")
		f.apa(f.WRK, 0, "report", c9, "--outcome", "blocked", "--summary", "本机无检出")
		eq(t, "父事项仍 waiting", f.phase(p9), "waiting")
		reps := f.events(c9, "delegation.reported")
		if len(reps) != 1 || !strings.Contains(toJSON(reps[0].Payload), "本机无检出") || !strings.Contains(toJSON(reps[0].Payload), "blocked") {
			t.Errorf("阻塞原因应可查: %+v", reps)
		}
		if f.phase(c9) == "dropped" {
			t.Errorf("事项不应丢失")
		}
	})

	f.step(t, "ACT-DLG-10 Web 查看和改派", func(t *testing.T) {
		p10 := f.add("acc-work", "DLG10-PARENT-QZK", "", "", "")
		c10 := f.delegate(p10, "work-host.cli")
		// 任务委派页读的数据：委派子事项列表、回执事件、执行者清单（含是否允许）。
		list := f.webOK("GET", "/items?kind=delegation", "", 200)
		contains(t, "委派页子事项", list, c10)
		contains(t, "委派页执行者清单", f.webOK("GET", "/executors?project="+f.Projects["acc-work"], "", 200), "work-host.cli")
		// 把另一执行者加入名单后改派。
		f.webOK("PUT", "/settings/projects/"+f.Projects["acc-work"]+"/allowed-executors", `{"executors":["work-host.cli","other-host.cli"]}`, 200)
		ver := f.item(c10)["version"]
		r := f.Web("PATCH", "/v1/items/"+c10, `{"assignee":"other-host.cli"}`)
		if r.Code != 200 {
			// 页面带 If-Match；这里与页面一致。
			r = f.WebRaw("PATCH", "/v1/items/"+c10, `{"assignee":"other-host.cli"}`, true)
			t.Fatalf("改派失败 HTTP %d %s (version %v)", r.Code, r.Body, ver)
		}
		eq(t, "改派后 assignee", f.item(c10)["assignee"], "other-host.cli")
		if len(f.events(c10, "item.updated")) == 0 {
			t.Errorf("改派应有 Event")
		}
	})

	f.step(t, "ACT-DLG-11 回执后可选核验", func(t *testing.T) {
		stub := f.ModelStub
		const mark = "请只依据资料段的回执自述和证据"
		// observe：只记录。
		p11 := f.add("acc-work", "DLG11-PARENT-QZK", "", "", "")
		c11 := f.delegate(p11, "work-host.cli")
		f.apa(f.WRK, 0, "report", c11, "--outcome", "done", "--summary", "已完成", "--pr", "https://example.invalid/acc/pr/11")
		f.waitFor("observe 模式只记录“本来会做什么”", 30*time.Second, func() bool { return len(f.events(c11, "rule.observed")) > 0 })
		eq(t, "observe 时父事项", f.phase(p11), "open")
		// auto + MODEL_STUB 返回“存疑”。
		stub.Reset()
		stub.When(mark, replyJSON(`{"probability":0.2,"confidence":0.9}`))
		f.webOK("PATCH", "/rules/"+"delegation-verify%2Fevent_rules%2Fverify-report"+"/mode", `{"mode":"auto"}`, 200)
		p12 := f.add("acc-work", "DLG11B-PARENT-QZK", "", "", "")
		c12 := f.delegate(p12, "work-host.cli")
		f.apa(f.WRK, 0, "report", c12, "--outcome", "done", "--summary", "已完成", "--pr", "https://example.invalid/acc/pr/12")
		f.waitFor("存疑时父事项生成决策请求交本人", 90*time.Second, func() bool { return f.phase(p12) == "needs_input" })
		if kids := f.children(p12); len(kids) != 1 {
			t.Errorf("不得自动返工/重派: 子事项 %v", kids)
		}
	})
}
