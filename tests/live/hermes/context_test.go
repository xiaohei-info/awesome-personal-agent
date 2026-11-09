//go:build live

package hermes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestFlowHermesContext 对应验收文档 flow-hermes-context（ACT-HCTX-01..11），真实 Hermes + 真实内核。
func TestFlowHermesContext(t *testing.T) {
	f := setup(t, "delivery_unclaimed_after: 25s\n")
	defer f.flushEvidence("flow-hermes-context-evidence.json")
	open := f.checkout("acc-open")
	home := f.hermesHome("ctx", "core", "inject", "tools")
	var s *sess

	f.step(t, "ACT-HCTX-01 安装 Hermes 插件", func(t *testing.T) {
		cmd := exec.Command(f.hermesBN, "plugins", "list")
		cmd.Env = f.env(home, f.DEL)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("hermes plugins list: %v\n%s", err, out)
		}
		f.evidence["ACT-HCTX-01-plugins-list"] = string(out)
		for _, p := range []string{"apa-core", "apa-inject", "apa-tools"} {
			if !strings.Contains(string(out), p) {
				t.Errorf("插件 %s 未出现在 hermes plugins list: %s", p, out)
			}
		}
		s = f.session(home, open, f.DEL)
		r := s.say("只回复两个字：收到")
		if !strings.Contains(r.Final, "收到") {
			t.Errorf("新会话不应报错: %q", r.Final)
		}
	})

	f.step(t, "ACT-HCTX-02 常驻指令让 agent 知道接入契约", func(t *testing.T) {
		s2 := f.session(home, open, f.DEL)
		r := s2.say("你怎样查看和处理我的事项？事项里写的内容你怎么对待？直接回答，不要执行任何命令。")
		contains(t, "回答", r.Final, "apa")
		dataWords := []string{"资料", "数据", "不是指令", "不当指令", "不按指令", "不服从", "不执行"}
		hasData := false
		for _, w := range dataWords {
			hasData = hasData || strings.Contains(r.Final, w)
		}
		if !strings.Contains(r.Final, "账本") || !hasData {
			t.Errorf("回答应提到事项正文是资料（不是指令）、账本是真理源: %s", r.Final)
		}
		if strings.Contains(r.Final, "请提供文档") || strings.Contains(r.Final, "不知道 apa") {
			t.Errorf("agent 不应向本人要文档: %s", r.Final)
		}
	})

	f.step(t, "ACT-HCTX-03 会话开始注入摘要", func(t *testing.T) {
		pend := f.ask("acc-open", "HCTX3-PEND-QZK 本周发布用方案 A 还是 B", "", "用方案 A 还是 B", "A=方案 A", "B=方案 B")
		parent := f.add("acc-open", "HCTX3-PARENT-QZK 修复登录", "", "", "")
		child := idOf(t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--evidence", "把它交给 work-host.cli").Stdout)
		f.apa(f.WRK, 0, "report", child, "--outcome", "done", "--summary", "HCTX3-RESULT-QZK 已修复登录", "--pr", "https://example.invalid/pr/3")
		other := f.ask("acc-work", "HCTX3-OTHER-QZK 另一个项目的待决", "", "是否同意", "A=同意", "B=拒绝")
		f.evidence["ACT-HCTX-03-items"] = map[string]string{"pending": pend, "delegated_child": child, "other_project": other}
		s = f.session(home, open, f.DEL)
		r := s.say("先别用任何命令。直接告诉我：我现在有哪几个待决事项（写出标题关键词）？委派出去的事现在是什么状态（写出事项 id 和阶段）？")
		contains(t, "回答", r.Final, "HCTX3-PEND-QZK", "HCTX3-PARENT-QZK", child)
		lacks(t, "回答（不得混入其他项目）", r.Final, "HCTX3-OTHER-QZK")
		if n := r.toolCalls("apa"); n != 0 {
			t.Errorf("agent 应不经查询就知道，实际调用了 %d 次 apa 相关工具: %v", n, r.Tools)
		}
		contains(t, "注入信封", r.Injected, "以下为资料", pend, "HCTX3-PEND-QZK")
		lacks(t, "注入信封（不含其他项目）", r.Injected, "HCTX3-OTHER-QZK")
		checks := f.calls("agents check")
		found := false
		for _, c := range checks {
			found = found || c.Session == s.ID || strings.HasPrefix(c.Session, s.ID)
		}
		if !found {
			t.Errorf("会话开始时钩子应跑 apa agents check（会话 %s），垫片记录: %+v", s.ID, checks)
		}
	})

	f.step(t, "ACT-HCTX-04 消息前注入增量", func(t *testing.T) {
		id := idOf(t, f.webOK("POST", "/items", `{"kind":"task","title":"HCTX4-NEW-QZK Web 新建待输入","phase":"open","project":"`+f.Projects["acc-open"]+`","evidence":"验收：Web 新建"}`, 201))
		f.webOK("POST", "/items/"+id+"/ask", `{"ask":{"question":"是否继续","options":[{"id":"A","label":"继续"},{"id":"B","label":"停止"}]}}`, 200)
		r1 := s.say("收到请只回复数字 1")
		contains(t, "这条消息的注入", r1.Injected, id, "HCTX4-NEW-QZK")
		lacks(t, "增量不重复会话开始的摘要", r1.Injected, "HCTX3-PEND-QZK")
		r2 := s.say("收到请只回复数字 2")
		eq(t, "再发一条的增量", strings.TrimSpace(r2.Injected), "")
		f.evidence["ACT-HCTX-04"] = map[string]any{"item": id, "first_message_injected_it": true, "second_message_injected": ""}
	})

	f.step(t, "ACT-HCTX-05 注入信封只带资料", func(t *testing.T) {
		id := f.add("acc-open", "HCTX5-ENV-QZK 信封检查", "MARKER-BODY 这是正文，不得进入信封", "", "")
		out := f.apa(f.DEL, 0, "context", "--session", "s2", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		contains(t, "信封", out, id, "HCTX5-ENV-QZK", "以下为资料", "唯一真理源")
		lacks(t, "信封", out, "MARKER-BODY")
		f.saveEvidence("act-hctx-05-envelope.txt", out)
	})

	f.step(t, "ACT-HCTX-06 注入不等于送达", func(t *testing.T) {
		f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"decision":"inject","reminder":"web","brief":"web","health":"web","wake":"web"}}}`, 200)
		a := f.session(home, open, f.DEL)
		b := f.session(home, open, f.DEL)
		id := f.ask("acc-open", "HCTX6-INJ-QZK 注入路由的决策", "", "是否继续", "A=继续", "B=停止")
		f.waitDelivery(id)
		ra := a.say("收到请只回复 OK")
		contains(t, "会话 A 的注入", ra.Injected, id)
		rb := b.say("收到请只回复 OK")
		contains(t, "会话 B 的注入", rb.Injected, id)
		// 先后两个会话注入都不算确认：第三个全新会话仍然会带出它。
		c := f.apa(f.DEL, 0, "context", "--session", "s-hctx6-c", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		contains(t, "注入后仍未确认，新会话仍带出", c, id)
		// 无人领取超时后它退回 Web 未读；记录了注入到哪些会话，状态不是 acknowledged。
		type inj struct {
			Session string `json:"injected_to_session"`
		}
		type dl struct {
			ID         string `json:"id"`
			Item       string `json:"item"`
			State      string `json:"state"`
			Injections []inj  `json:"injections"`
		}
		var mine *dl
		f.waitFor("投递退回 Web 未读", 3*time.Minute, func() bool {
			var out struct{ Deliveries []dl }
			must(t, json.Unmarshal([]byte(f.webOK("GET", "/deliveries/unread", "", 200)), &out))
			for i := range out.Deliveries {
				if out.Deliveries[i].Item == id {
					mine = &out.Deliveries[i]
				}
			}
			return mine != nil
		})
		if mine.State == "acknowledged" {
			t.Errorf("注入后不应是 acknowledged: %+v", mine)
		}
		got := map[string]bool{}
		for _, in := range mine.Injections {
			got[in.Session] = true
		}
		for _, sid := range []string{a.ID, b.ID, "s-hctx6-c"} {
			if !got[sid] && !hasPrefixKey(got, sid) {
				t.Errorf("投递应记录 injected_to_session=%s: %+v", sid, mine.Injections)
			}
		}
		f.evidence["ACT-HCTX-06-unread"] = mine
		// 本人在 Web 点击后才离开未读。
		f.webOK("POST", "/deliveries/"+mine.ID+"/read", "", 200)
		var after struct{ Deliveries []dl }
		must(t, json.Unmarshal([]byte(f.webOK("GET", "/deliveries/unread", "", 200)), &after))
		for _, d := range after.Deliveries {
			if d.ID == mine.ID {
				t.Errorf("Web 点击后投递应不再未读")
			}
		}
		// 本人在 Web 回答决策才算确认：注入过它的会话之后，新会话不再带出。
		y := f.ask("acc-open", "HCTX6-ACK-QZK 回答后确认", "", "是否发布", "A=发布", "B=不发布")
		f.waitDelivery(y)
		f.webOK("POST", "/items/"+y+"/decide", `{"choice":"A"}`, 200)
		after2 := f.apa(f.OBS, 0, "context", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		if strings.Contains(after2, "- 提醒 decision:"+y) {
			t.Errorf("Web 回答后投递已确认，不应再被注入: %s", after2)
		}
	})

	f.step(t, "ACT-HCTX-07 内核不可达时钩子放行", func(t *testing.T) {
		v := f.session(home, open, f.DEL)
		v.say("收到请只回复 OK")
		f.StopKernel()
		r := v.say("内核不可达时你还能回答吗？只回复 OK")
		if r.Final == "" {
			t.Errorf("消息应正常进入模型并得到回答")
		}
		if r.HookSecs > 2.5 {
			t.Errorf("钩子增加的延迟 %.2fs，不应超过 2 秒", r.HookSecs)
		}
		eq(t, "内核不可达时的注入", strings.TrimSpace(r.Injected), "")
		f.StartKernelAgain()
		id := f.ask("acc-open", "HCTX7-BACK-QZK 重启后的待决", "", "是否继续", "A=继续", "B=停止")
		r2 := v.say("收到请只回复 OK")
		contains(t, "重启后恢复注入", r2.Injected, id)
		f.evidence["ACT-HCTX-07"] = map[string]any{"hook_secs_when_down": r.HookSecs, "injection_resumed": true}
	})

	f.step(t, "ACT-HCTX-08 内核超慢时钩子放行", func(t *testing.T) {
		slow := httptest.NewServer(slowContextProxy(f.URL, 10*time.Second))
		defer slow.Close()
		w := f.session(home, open, f.DEL, "APA_URL="+slow.URL)
		r := w.say("收到请只回复 OK")
		if r.Final == "" {
			t.Errorf("消息应正常进入模型并得到回答")
		}
		if r.HookSecs > 3 {
			t.Errorf("钩子应在 2 秒内放弃，实际 %.2fs", r.HookSecs)
		}
		f.evidence["ACT-HCTX-08"] = map[string]any{"hook_secs_with_10s_kernel": r.HookSecs}
	})

	f.step(t, "ACT-HCTX-09 会话身份透传到审计", func(t *testing.T) {
		g := f.session(home, open, f.DEL)
		r := g.say("请调用 apa_propose 工具提议一个新事项（不要用终端），标题 HCTX9-PROP-QZK，理由写“验收”。完成后告诉我事项 id。")
		props := f.calls("propose")
		if len(props) == 0 || props[len(props)-1].Code != 0 {
			t.Fatalf("agent 应成功执行 apa propose；垫片记录 %+v；回答 %s", props, r.Final)
		}
		id := idRE.FindString(props[len(props)-1].Out)
		if id == "" {
			t.Fatalf("apa propose 没有返回事项 id: %s", props[len(props)-1].Out)
		}
		why := f.apa(f.DEL, 0, "why", id).Stdout
		f.saveEvidence("act-hctx-09-why.json", why)
		var out struct {
			Steps []event `json:"steps"`
		}
		must(t, json.Unmarshal([]byte(why), &out))
		ok := false
		for _, st := range out.Steps {
			h, _ := st.Actor["harness"].(string)
			sess, _ := st.Actor["session"].(string)
			ok = ok || h == "hermes" && sess != "" && strings.HasPrefix(sess, g.ID)
		}
		if !ok {
			t.Errorf("Event 的 actor 应含 harness=hermes 与 session=%s: %s", g.ID, why)
		}
	})

	f.step(t, "ACT-HCTX-10 常驻指令过期时提示更新", func(t *testing.T) {
		// 内核当前指令版本为 1，0 表示不比对、CLI 不接受负数，所以拿不到“更小的正数”：用与内核不同的版本号 2
		// 触发内核的版本比对（内核按“不等于当前”判过期）。
		h := f.session(home, open, f.DEL, "APA_INSTRUCTION_VERSION=2")
		r := h.say("只回复 OK")
		contains(t, "过期提示", r.Injected, "常驻指令已过期")
		if r.Final == "" {
			t.Errorf("会话应继续正常工作")
		}
		r2 := h.say("只回复 OK")
		lacks(t, "第二条消息不再重复提示", r2.Injected, "常驻指令已过期")
	})

	f.step(t, "ACT-HCTX-11 恢复会话后重新注入", func(t *testing.T) {
		pend := f.ask("acc-open", "HCTX11-PEND-QZK 恢复后仍要知道的待决", "", "是否继续", "A=继续", "B=停止")
		first := f.session(home, open, f.DEL)
		first.say("只回复 OK")
		sid := first.ID
		first.close()
		// 真实 CLI 恢复：hermes chat --resume <session_id>，在检出目录里发一条消息。
		cmd := exec.Command(f.hermesBN, "chat", "--in", open, "--resume", sid, "-Q", "-q", "先别用任何命令：我现在有哪些待决事项？写出标题关键词。")
		cmd.Dir, cmd.Env = open, f.env(home, f.DEL)
		out, err := cmd.CombinedOutput()
		f.saveEvidence("act-hctx-11-resume-cli.txt", string(out))
		if err != nil {
			t.Fatalf("hermes chat --resume %s: %v\n%s", sid, err, f.redact(string(out)))
		}
		var inj []call
		for _, c := range f.calls("--format inject") {
			if strings.HasPrefix(c.Session, sid+"~") {
				inj = append(inj, c)
			}
		}
		for _, c := range inj {
			f.evidence["ACT-HCTX-11-resume-hook-call"] = map[string]any{"session": c.Session, "cwd": c.Cwd, "args": c.Args, "out_len": len(c.Out)}
		}
		if len(inj) == 0 {
			t.Fatalf("恢复后钩子没有以新游标（%s~…）调用 apa context", sid)
		}
		for _, c := range inj {
			if c.Cwd != open || !strings.Contains(c.Out, pend) {
				t.Errorf("恢复后应在检出目录 %s 下重新注入摘要（含 %s），实际钩子工作目录 %s，输出 %d 字节", open, pend, c.Cwd, len(c.Out))
			}
		}
		contains(t, "恢复后的回答", string(out), "HCTX11-PEND")
		// 压缩：Hermes 自己的压缩流程（会触发 post_compaction），之后再发一条，摘要重新带上。
		cs := f.session(home, open, f.DEL)
		pad := strings.Repeat("meeting-notes-filler ", 300) // Hermes 保留约 1~2.5 万 token 的尾部不压缩，历史要明显超过它才有可摘要的中段
		for i := 1; i <= 30; i++ {                          // 压缩要有可摘要的中段：历史太短时 Hermes 不会真正压缩，也就不发 post_compaction
			cs.say(fmt.Sprintf("第%d件事：记得周%d上午十点开会并带上季度报告。以下是无关的会议记录填充，忽略即可：%s。只回复：好", i, i%7+1, pad))
		}
		c := cs.compress()
		if !c.Compacted || c.After >= c.Before {
			t.Fatalf("压缩应真实缩短历史：%+v", c)
		}
		rc := cs.say("只回复 OK")
		contains(t, "压缩后的注入", rc.Injected, pend, "以下为资料")
		lacks(t, "压缩后钩子不报错", cs.logb.String(), "Traceback")
	})
}

// slowContextProxy 把请求转给真实内核，但 /v1/context 先等 delay 再转发。
func slowContextProxy(target string, delay time.Duration) http.Handler {
	u, _ := url.Parse(target)
	p := httputil.NewSingleHostReverseProxy(u)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/context") {
			time.Sleep(delay)
		}
		p.ServeHTTP(w, r)
	})
}

func hasPrefixKey(m map[string]bool, prefix string) bool {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// waitDelivery 等内核为事项写出注入路由的决策投递（异步生成）。
func (f *fx) waitDelivery(item string) {
	f.t.Helper()
	f.waitFor("事项 "+item+" 的投递出现在 apa context", 60*time.Second, func() bool {
		out := f.apa(f.OBS, 0, "context", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		return strings.Contains(out, "- 提醒 decision:"+item)
	})
}

var idRE = regexp.MustCompile(`itm_[0-9a-f]{16}`)
