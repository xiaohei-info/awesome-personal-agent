//go:build e2e

package agents

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// apaEnv 与 f.apa 相同，但可追加环境变量（如 APA_SESSION）并在 dir 里运行；返回退出码与输出。
func (f *fx) apaEnv(token, dir string, extra []string, args ...string) (int, string, string) {
	f.t.Helper()
	cmd := exec.Command(f.Bin["apa"], args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, f.apaBaseEnv(token, args)...), extra...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			f.t.Fatalf("运行 apa %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, o.String(), e.String()
}

func contains(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s 应含 %q，实际：\n%.800s", what, sub, s)
		}
	}
}

func lacks(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("%s 不应含 %q，实际：\n%.800s", what, sub, s)
		}
	}
}

// delegate 用 DEL 代本人在账本里建子事项。
func (f *fx) delegate(parent, assignee string) string {
	f.t.Helper()
	return idOf(f.t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", assignee, "--evidence", "把它交给 "+assignee).Stdout)
}

func (f *fx) workItem(title string) string {
	f.t.Helper()
	return idOf(f.t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open",
		"--project", f.Projects["acc-work"], "--evidence", "验收准备").Stdout)
}

// TestFlowDelegateRecover 对应验收文档 flow-delegate-recover（ACT-DLG-02..10；ACT-DLG-01 是 flow-step，归环境基线）：
// 真实 apa 二进制与内核进程，执行者身份由 WRK 等运行凭据承担，替身 harness 只在需要无头运行的流程里出现。
func TestFlowDelegateRecover(t *testing.T) {
	f := setup(t, "")
	f.registerCheckout()
	work, dir := f.Projects["acc-work"], f.repoDir()
	session := []string{"APA_SESSION=acc-session-1", "APA_SESSION_KIND=direct"}
	var parent, child string

	t.Run("ACT-DLG-02 建立委派子事项", func(t *testing.T) {
		parent = f.workItem("DLG2 修复登录超时")
		code, out, errs := f.apaEnv(f.DEL, "", session, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--evidence", "把它交给 work-host.cli")
		if code != 0 {
			t.Fatalf("apa delegate 退出码 %d: %s%s", code, out, errs)
		}
		child = idOf(t, out)
		it := f.item(child)
		eq(t, "子事项 assignee", it["assignee"], "work-host.cli")
		eq(t, "子事项 project", it["project"], work)
		eq(t, "父事项阶段", f.phase(parent), "waiting")
		contains(t, "父事项 waiting_on 指向执行者", f.apa(f.OBS, 0, "item", "show", parent).Stdout, "work-host.cli")
	})

	t.Run("ACT-DLG-03 执行者拿到项目上下文，不含材料正文", func(t *testing.T) {
		const title, body = "DLG3-MAT 登录超时排查纪要", "MARKER-BODY-DLG3 这是材料正文"
		mf := f.tmpFile("m.md", body)
		f.apa(f.WRK, 0, "material", "commit", "--kind", "minutes", "--title", title, "--project", work, "--from", mf, "--reason", "DLG3 登记材料")
		_, itemCtx, _ := f.apaEnv(f.WRK, dir, nil, "context", "--scope", "item", "--item", child)
		contains(t, "item 作用域", itemCtx, child)
		// 项目信息、repos、允许的执行者、相关材料标题属于项目上下文（设计 §7.6）：执行者在检出目录里读取，由目录映射到项目。
		code, projCtx, errs := f.apaEnv(f.WRK, dir, nil, "context", "--format", "inject")
		if code != 0 {
			t.Fatalf("apa context 退出码 %d: %s", code, errs)
		}
		contains(t, "项目上下文", projCtx, "acc-work", f.LocalRepo, "work-host.cli", title)
		// AGENTS.md 指引由同一份规范渲染并随检出带到（设计 §10 第 3 步：插件补充 AGENTS.md）。
		contains(t, "AGENTS.md 渲染", f.webOK("GET", "/agents-md?repo="+f.LocalRepo, "", 200), "apa:managed")
		lacks(t, "上下文不带材料正文", itemCtx+projCtx, "MARKER-BODY")
	})

	t.Run("ACT-DLG-04 执行者回执并附证据；无结果证据的完成回执被拒", func(t *testing.T) {
		f.apa(f.WRK, 0, "report", child, "--outcome", "done", "--summary", "已修复", "--pr", "https://example.invalid/acc/pr/42")
		eq(t, "子事项阶段", f.phase(child), "done")
		eq(t, "父事项阶段", f.phase(parent), "open")
		reps := f.events(child, "delegation.reported")
		if len(reps) != 1 || !strings.Contains(toJSON(reps[0].Payload), "pr/42") {
			t.Errorf("事件流应有带 PR 证据的 delegation.reported: %+v", reps)
		}
		p4 := f.workItem("DLG4 无证据完成")
		c4 := f.delegate(p4, "work-host.cli")
		f.apa(f.WRK, 2, "report", c4, "--outcome", "done", "--summary", "已修复")
		eq(t, "无证据完成回执后的子事项阶段", f.phase(c4), "open")
		eq(t, "无证据完成回执后的回执事件数", len(f.events(c4, "delegation.reported")), 0)
	})

	t.Run("ACT-DLG-05 回收结果注入主会话", func(t *testing.T) {
		code, out, errs := f.apaEnv(f.DEL, dir, session, "context", "--format", "inject")
		if code != 0 {
			t.Fatalf("apa context 退出码 %d: %s%s", code, out, errs)
		}
		// 注入信封只带 id、标题、阶段与建议动作（设计 §7.6），回执结果以"阶段 done"回到主会话；
		// PR 证据经信封给出的动作（apa why / item show）读取，与 v0.1 已通过的做法一致。
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, child) {
				line = l
			}
		}
		contains(t, "子事项条目", line, "阶段 done", "apa item show "+child)
		contains(t, "证据经 apa why 可读", f.apa(f.DEL, 0, "why", child).Stdout, "pr/42")
	})

	t.Run("ACT-DLG-06 插件捕获回执（契约测试）", func(t *testing.T) {
		// 契约测试覆盖：执行者工具调用参数带子事项 id 且执行者不自行回执 → 写入回执；不带事项 id 的调用 → 不产生任何回执。
		// 真实 Hermes 下的同一行为由本机升级验证 L9 承接。
		cmd := exec.Command("python3", "-m", "pytest", "-q", "-p", "no:cacheprovider", "plugins/hermes/tests/receipt/")
		cmd.Dir = filepath.Join("..", "..", "..")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pytest 失败: %v\n%s", err, out)
		}
		contains(t, "pytest 输出", string(out), "passed")
		lacks(t, "pytest 输出", string(out), "failed", "error")
	})

	t.Run("ACT-DLG-07 其他凭据不能回执", func(t *testing.T) {
		c7 := f.delegate(f.workItem("DLG7"), "work-host.cli")
		f.apa(f.WRK2, 4, "report", c7, "--outcome", "done", "--summary", "冒充回执", "--pr", "https://example.invalid/acc/pr/7")
		eq(t, "子事项不变", f.phase(c7), "open")
		eq(t, "回执事件数", len(f.events(c7, "delegation.reported")), 0)
	})

	t.Run("ACT-DLG-08 全部子事项终态后父事项才回 open", func(t *testing.T) {
		p8 := f.workItem("DLG8")
		a, b := f.delegate(p8, "work-host.cli"), f.delegate(p8, "work-host.cli")
		f.apa(f.WRK, 0, "report", a, "--outcome", "done", "--summary", "第一个完成", "--pr", "https://example.invalid/acc/pr/8")
		f.apa(f.WRK, 0, "ask", b, "--question", "需要本人决定", "--option", "A=继续", "--option", "B=放弃")
		eq(t, "一个 done 一个 needs_input 时的子事项", f.phase(b), "needs_input")
		eq(t, "一个 done 一个 needs_input 时父事项", f.phase(p8), "waiting")
		f.apa(f.DEL, 0, "item", "transition", b, "--to", "dropped", "--reason", "不做了", "--evidence", "不做了")
		eq(t, "两者都到终态后父事项", f.phase(p8), "open")
	})

	t.Run("ACT-DLG-09 回执阻塞，进展可查、父事项仍 waiting", func(t *testing.T) {
		p9 := f.workItem("DLG9")
		c9 := f.delegate(p9, "work-host.cli")
		f.apa(f.WRK, 0, "report", c9, "--outcome", "blocked", "--summary", "本机无检出")
		eq(t, "父事项", f.phase(p9), "waiting")
		reps := f.events(c9, "delegation.reported")
		if len(reps) != 1 || !strings.Contains(toJSON(reps[0].Payload), "本机无检出") || !strings.Contains(toJSON(reps[0].Payload), "blocked") {
			t.Errorf("阻塞原因应可查: %+v", reps)
		}
		if ph := f.phase(c9); ph == "dropped" || ph == "done" {
			t.Errorf("阻塞回执不应让事项终结或丢失: %s", ph)
		}
	})

	t.Run("ACT-DLG-10 Web 查看和改派", func(t *testing.T) {
		c10 := f.delegate(f.workItem("DLG10"), "work-host.cli")
		contains(t, "委派页子事项", f.webOK("GET", "/items?kind=delegation", "", 200), c10)
		contains(t, "委派页执行者清单", f.webOK("GET", "/executors?project="+work, "", 200), "work-host.cli")
		contains(t, "回执事件可见", f.webOK("GET", "/audit/events?limit=500&type=delegation.created", "", 200), c10)
		// 先把另一执行者加入名单再改派。
		f.webOK("PUT", "/settings/projects/"+work+"/allowed-executors", `{"executors":["work-host.cli","other-host.cli"]}`, 200)
		f.webOK("PATCH", "/items/"+c10, `{"assignee":"other-host.cli"}`, 200)
		eq(t, "改派后 assignee", f.item(c10)["assignee"], "other-host.cli")
		if len(f.events(c10, "item.updated")) == 0 {
			t.Errorf("改派应有 Event")
		}
	})
}
