//go:build e2e

package bgguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runFile 读内核为该次运行建的目录里的文件（替身 harness 在运行目录写 args.txt、env.txt）。
func (f *fx) runFile(runID, name string) string {
	b, _ := os.ReadFile(filepath.Join(f.Home, "runs", runID, name))
	return string(b)
}

// runEnv 取运行者进程收到的环境变量（如 APA_TOKEN）。
func (f *fx) runEnv(runID, key string) string {
	for _, l := range strings.Split(f.runFile(runID, "env.txt"), "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return v
		}
	}
	return ""
}

func hasType(evs []event, typ string) bool {
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// TestFlowBgMainSession 对应验收文档 flow-bg-main-session（ACT-BGM-02..09；BGM-01 是准备步骤，由 setup 完成）。
func TestFlowBgMainSession(t *testing.T) {
	f := setup(t)
	// ACT-BGM-01：权限上限 edit、默认 harness 为替身、work-host.cli 在名单内；规则初始 observe。
	f.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"read","by_harness":{"`+harness+`":"edit","`+harnessNoResume+`":"edit"}}}`, 200)
	for _, r := range []string{"bgm-created", "bgm-reopen", "bgm-explicit"} {
		if !strings.Contains(f.webOK("GET", "/rules", "", 200), `"mode":"observe"`) {
			t.Fatalf("规则 %s 初始应为 observe", r)
		}
	}
	var parent, child string
	var first run

	f.step("ACT-BGM-02 observe 只记录不拉起", func(t *testing.T) {
		observed := f.task("bgm-02 observe", "acc-work", "acc-bgm")
		f.waitFor("记下 rule.observed", 30*time.Second, func() bool { return hasType(f.events(observed, "rule.observed"), "rule.observed") })
		eq(t, "Run 数", len(f.runsOf(observed)), 0)
		if why := f.apa(f.DEL, 0, "why", observed).Stdout; !strings.Contains(why, "rule.observed") {
			t.Errorf("apa why 应有“本来会做什么”的记录: %s", why)
		}
	})

	f.step("ACT-BGM-03 切到 auto 后拉起运行", func(t *testing.T) {
		f.auto("bgm-created", "bgm-reopen")
		f.mode("summary-only", preamble+`
$APA context --scope item --item "$ITEM" > "$OUT/ctx.out" 2>&1; rc ctx
$APA delegate --parent "$ITEM" --assignee work-host.cli --title 修复交给执行者 > "$OUT/deleg.out" 2>&1; rc deleg
$APA report "$ITEM" --outcome progress --summary 已委派 > "$OUT/progress.out" 2>&1; rc progress
`)
		parent = f.task("bgm-03 把修复交给执行者", "acc-work", "acc-bgm")
		f.waitFor("Run 建立", 30*time.Second, func() bool { return len(f.runsOf(parent)) >= 1 })
		first = f.runsOf(parent)[0]
		eq(t, "category", first.Category, "background")
		eq(t, "runner", first.Runner, "main_session")
		eq(t, "launch", first.Launch, "headless")
		eq(t, "access", first.Access, "edit")
		eq(t, "规则 id", first.Basis.RuleID, rulePrefix+"bgm-created")
		if first.Basis.PolicyVersion == 0 {
			t.Errorf("Run 应记策略版本: %+v", first.Basis)
		}
		if a, _ := f.item(parent)["assignee"].(string); a != "" {
			t.Errorf("运行者不占事项的 assignee，实际 %q", a)
		}
	})

	f.step("ACT-BGM-04 运行者读上下文并委派", func(t *testing.T) {
		first = f.settled(parent, 1)[0]
		eq(t, "context 退出码 "+f.outFile("ctx.out"), f.outFile("ctx.rc"), "0")
		eq(t, "delegate 退出码 "+f.outFile("deleg.out"), f.outFile("deleg.rc"), "0")
		child = idFrom(f.outFile("deleg.out"))
		c := f.item(child)
		eq(t, "子事项 assignee", c["assignee"], "work-host.cli")
		eq(t, "子事项 unattended", c["unattended"], true)
		var delegated bool
		for _, e := range f.events(child, "") {
			if strings.Contains(e.Type, "delegat") || strings.Contains(e.Type, "item.created") {
				delegated = delegated || e.Actor.Principal == first.ID || strings.Contains(toJSON(e.Actor), first.ID)
			}
		}
		if !delegated {
			t.Errorf("委派写入的操作者应是该运行 %s: %+v", first.ID, f.events(child, ""))
		}
		if f.runFile(first.ID, "args.txt") == "" {
			t.Errorf("无头进程应由 APA 所在主机拉起（运行目录里缺少替身留下的 args.txt）")
		}
	})

	f.step("ACT-BGM-05 运行结束，父事项等待", func(t *testing.T) {
		eq(t, "Run state", first.State, "succeeded")
		eq(t, "父事项阶段", f.phase(parent), "waiting")
		if s := f.sessionOf(parent, first.ID); s != "sess-"+first.ID {
			t.Errorf("事项应记录运行者会话 id sess-%s，实际 %q", first.ID, s)
		}
		tok := f.runEnv(first.ID, "APA_TOKEN")
		if tok == "" {
			t.Fatal("替身没有留下运行凭据")
		}
		eq(t, "已失效的运行凭据调 apa inbox 的退出码", f.Apa(tok, "", "inbox").Code, 3)
	})

	f.step("ACT-BGM-06 回执后下一次运行接续", func(t *testing.T) {
		f.mode("summary-only", "")
		f.apa(f.WRK, 0, "report", child, "--outcome", "done", "--summary", "修复完成", "--pr", "https://example.invalid/pr/11")
		eq(t, "子事项阶段", f.phase(child), "done")
		f.waitFor("规则再次命中，建新 Run", 30*time.Second, func() bool { return len(f.runsOf(parent)) >= 2 })
		var next run
		for _, r := range f.runsOf(parent) {
			if r.ID != first.ID {
				next = r
			}
		}
		if next.ChainDepth <= first.ChainDepth {
			t.Errorf("新 Run 的 chain_depth = %d，应大于 %d", next.ChainDepth, first.ChainDepth)
		}
		f.settled(parent, 2)
	})

	f.step("ACT-BGM-07 全程回放", func(t *testing.T) {
		why := f.apa(f.DEL, 0, "why", parent).Stdout
		for _, want := range []string{"background.launched", rulePrefix + "bgm-created", "run.finished", "waiting on work-host.cli", "all delegated items finished"} {
			if !strings.Contains(why, want) {
				t.Errorf("apa why 缺少 %q", want)
			}
		}
	})

	f.step("ACT-BGM-08 运行结束时仍是 open 交本人", func(t *testing.T) {
		f.mode("no-receipt", "")
		id := f.task("bgm-08 不回执", "acc-work", "acc-bgm")
		rs := f.settled(id, 1)
		eq(t, "Run 数", len(rs), 1)
		f.waitFor("事项转 needs_input", 20*time.Second, func() bool { return f.phase(id) == "needs_input" })
		a, _ := f.item(id)["ask"].(map[string]any)
		if opts, _ := a["options"].([]any); len(opts) < 3 {
			t.Errorf("应给出重新运行、改派、放弃三个选项: %+v", a)
		}
		time.Sleep(5 * time.Second)
		eq(t, "不自动再次拉起，Run 数", len(f.runsOf(id)), 1)
		eq(t, "默认 harness", rs[0].Harness, harness)
	})

	f.step("ACT-BGM-09 指定 harness", func(t *testing.T) {
		f.auto("bgm-explicit")
		f.mode("summary-only", "")
		id := f.task("bgm-09 指定 harness", "acc-work", "acc-explicit")
		eq(t, "规则声明的 harness", f.settled(id, 1)[0].Harness, harnessNoResume)
	})
}
