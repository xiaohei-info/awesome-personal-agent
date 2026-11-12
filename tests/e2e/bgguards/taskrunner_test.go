//go:build e2e

package bgguards

import (
	"strings"
	"testing"
)

// TestFlowBgTaskRunner 对应验收文档 flow-bg-task-runner（ACT-BGT-01..04）。
func TestFlowBgTaskRunner(t *testing.T) {
	f := setup(t)
	f.auto("task-work", "task-other")
	var parent, child string

	f.step("ACT-BGT-01 建好子事项后拉起", func(t *testing.T) {
		f.mode("summary-only", preamble+`
$APA report "$ITEM" --outcome done --summary 任务型完成 --link https://example.invalid/e2e/evidence > "$OUT/report.out" 2>&1; rc report
`)
		parent = f.task("bgt-parent", "acc-work", "acc-task")
		f.waitFor("子事项的运行结束", 40*1e9, func() bool {
			for _, r := range f.runs("--category", "background") {
				if r.Runner == "task" && r.State != "queued" && r.State != "running" {
					child = r.Target
					return true
				}
			}
			return false
		})
		rs := f.runsOf(child)
		if len(rs) != 1 || rs[0].Runner != "task" || rs[0].State != "succeeded" {
			t.Fatalf("Run 的目标应是子事项且成功: %+v", rs)
		}
		if child == parent || len(f.runsOf(parent)) != 0 {
			t.Fatalf("Run 目标不应是触发事项 %s: child=%s", parent, child)
		}
		it := f.item(child)
		eq(t, "子事项 assignee", it["assignee"], "work-host.cli")
		eq(t, "子事项阶段", it["phase"], "done")
		eq(t, "回执 CLI 退出码 "+f.outFile("report.out"), f.outFile("report.rc"), "0")
		// 证据写在子事项的回执里，不在别的事项上。
		if !strings.Contains(f.apa(f.OBS, 0, "why", child).Stdout, "delegation.reported") {
			t.Errorf("子事项上应有回执 Event")
		}
		if strings.Contains(f.apa(f.OBS, 0, "why", parent).Stdout, "delegation.reported") {
			t.Errorf("回执不应写到触发事项 %s", parent)
		}
		if rel := f.apa(f.OBS, 0, "item", "show", child).Stdout; !strings.Contains(rel, `"from":"`+child+`","to":"`+parent+`","type":"parent"`) {
			t.Errorf("子事项应挂在触发事项 %s 下: %s", parent, rel)
		}
	})

	f.step("ACT-BGT-02 执行者不在名单时不拉起", func(t *testing.T) {
		before := len(f.runs())
		id := f.task("bgt-other", "acc-work", "acc-task-other")
		f.waitFor("事项转 needs_input", 20*1e9, func() bool { return f.phase(id) == "needs_input" })
		eq(t, "新增 Run 数", len(f.runs())-before, 0)
		ev := f.events(id, "background.refused")
		if len(ev) != 1 || !strings.Contains(ev[0].Reason+" "+toJSON(ev[0].Payload), "other-host.cli") {
			t.Errorf("应有一条写明执行者的拒绝记录: %+v", ev)
		}
		for _, it := range f.listAssignee("other-host.cli") {
			t.Errorf("不应建出指派给名单外执行者的子事项: %s", it)
		}
	})

	f.step("ACT-BGT-03 运行凭据只限目标范围", func(t *testing.T) {
		// 无关事项：另一条委派链上的子事项（assignee 是 WRK 的执行者），运行凭据不属于它。
		other := f.task("bgt-unrelated-parent", "acc-work")
		unrelated := idOf(t, f.apa(f.DEL, 0, "delegate", "--parent", other, "--assignee", "work-host.cli", "--evidence", "e2e 准备无关子事项").Stdout)
		f.mode("summary-only", preamble+`
$APA report "$ITEM" --outcome progress --summary 写目标 > "$OUT/target_report.out" 2>&1; rc target_report
$APA delegate --parent "$ITEM" --assignee work-host.cli --title 运行建的子事项 > "$OUT/sub.out" 2>&1; rc sub_delegate
$APA ask "$ITEM" --question 要继续吗 --option A=是 --option B=否 --deadline 2099-01-01T00:00:00Z --default-on-timeout B > "$OUT/timer_ask.out" 2>&1; rc timer_ask
$APA report `+unrelated+` --outcome progress --summary 越界 > /dev/null 2>&1; rc other_report
$APA item transition `+unrelated+` --to done --reason 越界 --evidence 越界 > /dev/null 2>&1; rc other_transition
$APA project create --name evil-project > /dev/null 2>&1; rc project_create
`)
		f.task("bgt-scope", "acc-work", "acc-task")
		f.waitFor("脚本跑完", 40*1e9, func() bool { return f.outFile("project_create.rc") != "" })
		for name, want := range map[string]string{"target_report": "0", "sub_delegate": "0", "timer_ask": "0",
			"other_report": "4", "other_transition": "4", "project_create": "4"} {
			eq(t, name+" 退出码 "+f.outFile(name+".out"), f.outFile(name+".rc"), want)
		}
		eq(t, "无关事项阶段", f.phase(unrelated), "open")
		if f.projectNamed("evil-project") {
			t.Error("运行凭据不应建出项目")
		}
		eq(t, "运行建的子事项 assignee", f.item(idFrom(f.outFile("sub.out")))["assignee"], "work-host.cli")
		if len(f.events(idFrom(f.outFile("timer_ask.out")), "decision.requested")) == 0 {
			t.Error("Timer（决策超时）应已建立")
		}
	})

	f.step("ACT-BGT-04 不回退到本机凭据", func(t *testing.T) {
		// 本机凭据文件在 APA_HOME 里（apa login 保存）。运行凭据有三个来源（§7.7、§17.4）：APA_TOKEN、APA_RUN_CONTEXT 指向的
		// 上下文文件、APA_RUN_CREDENTIAL_FILE。全部去掉、只留 APA_RUN 时，CLI 不得回退使用本机凭据。
		// 对照组去掉 APA_RUN 与运行标记（PATH 末尾的 .apa-run 条目），才是真正走本机凭据。
		if r := f.Apa("", "", "login", "--url", f.URL, "--token", f.WRK); r.Code != 0 {
			t.Fatalf("apa login: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		f.mode("summary-only", preamble+`
export APA_HOME="`+f.Home+`"
LOCALPATH=$(printf '%s' "$PATH" | tr ':' '\n' | grep -v '/\.apa-run$' | paste -sd: -)
env -u APA_RUN -u APA_TOKEN -u APA_RUN_CONTEXT -u APA_RUN_CREDENTIAL_FILE PATH="$LOCALPATH" $APA inbox > "$OUT/local.out" 2>&1; rc local_inbox
APA_RUN=${APA_RUN:-run_nofallback} env -u APA_TOKEN -u APA_RUN_CONTEXT -u APA_RUN_CREDENTIAL_FILE $APA inbox > "$OUT/run.out" 2>&1; rc run_inbox
`)
		f.task("bgt-nofallback", "acc-work", "acc-task")
		f.waitFor("脚本跑完", 40*1e9, func() bool { return f.outFile("run_inbox.rc") != "" })
		eq(t, "本机凭据本身可用（对照）", f.outFile("local_inbox.rc"), "0")
		eq(t, "设了 APA_RUN 且无任何运行凭据来源的 apa inbox 退出码", f.outFile("run_inbox.rc"), "3")
	})
}
