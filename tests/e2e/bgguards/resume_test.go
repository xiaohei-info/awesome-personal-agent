//go:build e2e

package bgguards

import (
	"strings"
	"testing"
	"time"
)

const askScript = preamble + `
$APA ask "$ITEM" --question 选哪个 --option A=甲 --option B=乙 --resume-note RN-1 > "$OUT/ask.out" 2>&1; rc ask
`

// askAndStop 建 tag 事项，等运行者用 apa ask 提交带 resume_note 的决策请求并结束；返回事项与第一次运行。
func (f *fx) askAndStop(title string, tags ...string) (string, run) {
	f.t.Helper()
	f.mode("summary-only", askScript)
	id := f.task(title, "acc-work", tags...)
	rs := f.settled(id, 1)
	eq(f.t, "ask 退出码 "+f.outFile("ask.out"), f.outFile("ask.rc"), "0")
	eq(f.t, "Run state", rs[0].State, "succeeded")
	f.waitFor("事项转 needs_input", 20*time.Second, func() bool { return f.phase(id) == "needs_input" })
	return id, rs[0]
}

func (f *fx) answer(id, choice string, extra ...string) {
	f.t.Helper()
	f.mode("summary-only", "", extra...) // 续接运行不再重复提问
	f.webOK("POST", "/items/"+id+"/decide", `{"choice":"`+choice+`","evidence":"本人在 Web 回答"}`, 200)
}

func (f *fx) resumeEvents(id string) []event { return f.events(id, "resume.launched") }

func other(rs []run, first run) run {
	for _, r := range rs {
		if r.ID != first.ID {
			return r
		}
	}
	return run{}
}

// TestFlowBgResume 对应验收文档 flow-bg-resume（ACT-RES-01..05，替身版）。
func TestFlowBgResume(t *testing.T) {
	f := setup(t)
	f.auto("main-read", "bgm-explicit", "task-work")
	// 决策走 Web 未读投递，便于核对触达。
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false}}`, 200)
	var item string
	var first run

	f.step("ACT-RES-01 决定前结束运行并通知", func(t *testing.T) {
		item, first = f.askAndStop("res-01", "acc-main")
		if first.Session != "sess-"+first.ID && f.sessionOf(item, first.ID) != "sess-"+first.ID {
			t.Errorf("事项应记录首次运行的会话 id")
		}
		// 触达：决策请求进入本人的待办与投递流。
		if inbox := f.apa(f.OBS, 0, "inbox").Stdout; !strings.Contains(inbox, item) {
			t.Errorf("决策请求应触达本人: %s", inbox)
		}
		f.waitFor("触达送达（Web 未读投递）", 15*time.Second, func() bool {
			return strings.Contains(f.webOK("GET", "/deliveries/unread", "", 200), item)
		})
		if r := f.runsOf(item)[0]; r.State == "running" || r.State == "queued" {
			t.Errorf("运行应已结束: %+v", r)
		}
	})

	f.step("ACT-RES-02 回答后用同一会话续接", func(t *testing.T) {
		f.answer(item, "A")
		evs := f.resumeEvents(item)
		eq(t, "resume.launched 数（回答同一事务写入续接 Run）", len(evs), 1)
		sess := f.sessionOf(item, first.ID)
		eq(t, "续接方式", evs[0].Payload["mode"], "resumed")
		eq(t, "续接用的会话 id", evs[0].Payload["session_id"], sess)
		next := other(f.settled(item, 2), first)
		args := f.runFile(next.ID, "args.txt")
		for _, want := range []string{"--resume", sess, "RN-1", "A"} {
			if !strings.Contains(args, want) {
				t.Errorf("续接参数缺少 %q:\n%s", want, args)
			}
		}
		if tok := f.runEnv(next.ID, "APA_TOKEN"); tok == "" || tok == f.runEnv(first.ID, "APA_TOKEN") {
			t.Errorf("续接应使用新的运行凭据")
		}
		time.Sleep(3 * time.Second) // 事件规则随后命中时应跳过
		eq(t, "没有重复拉起，Run 数", len(f.runsOf(item)), 2)
	})

	f.step("ACT-RES-03 不支持续接时用新会话", func(t *testing.T) {
		id, first := f.askAndStop("res-03", "acc-explicit")
		eq(t, "harness", first.Harness, harnessNoResume)
		f.answer(id, "B")
		evs := f.resumeEvents(id)
		eq(t, "resume.launched 数", len(evs), 1)
		eq(t, "续接方式", evs[0].Payload["mode"], "fresh")
		next := other(f.settled(id, 2), first)
		args := f.runFile(next.ID, "args.txt")
		ctx := f.runFile(next.ID, "context.md")
		if strings.Contains(args, "--resume") {
			t.Errorf("不支持续接的 harness 不应带 --resume: %s", args)
		}
		for _, want := range []string{"选哪个", "RN-1"} {
			if !strings.Contains(args, want) || !strings.Contains(ctx, want) {
				t.Errorf("新会话提示词/资料文件缺少 %q:\nargs=%s\nctx=%s", want, args, ctx)
			}
		}
		if !strings.Contains(ctx, "挂靠消息") {
			t.Errorf("资料文件应含挂靠消息段: %s", ctx)
		}
	})

	f.step("ACT-RES-04 会话失效时用新会话", func(t *testing.T) {
		id, first := f.askAndStop("res-04", "acc-main")
		f.answer(id, "A", "LOSE_SESSION=1") // 带 --resume 的命令退出码 1 并输出“No session found matching”
		f.settled(id, 3)                    // 原运行、失败的续接、新会话
		if len(f.events(id, "resume.session_lost")) != 1 {
			t.Errorf("会话失效应记 resume.session_lost")
		}
		evs := f.resumeEvents(id)
		if len(evs) < 2 || evs[len(evs)-1].Payload["mode"] != "fresh" {
			t.Errorf("应改用新会话续接: %+v", evs)
		}
		ok := false
		for _, r := range f.runsOf(id) {
			if r.ID != first.ID && r.State == "succeeded" && !strings.Contains(f.runFile(r.ID, "args.txt"), "--resume") {
				ok = true
			}
		}
		if !ok {
			t.Errorf("应有一个不带 --resume 且成功的新会话运行: %+v", f.runsOf(id))
		}
		if f.phase(id) == "needs_input" && len(f.events(id, "decision.requested")) < 1 {
			t.Errorf("事项不应卡住")
		}
	})

	f.step("ACT-RES-05 恢复前按当前名单复核", func(t *testing.T) {
		f.mode("summary-only", askScript)
		parent := f.task("res-05", "acc-work", "acc-task")
		var child string
		f.waitFor("任务型子事项的运行结束", 40*time.Second, func() bool {
			for _, r := range f.runs("--category", "background") {
				if r.Runner == "task" && r.Target != parent && r.Target != "" && r.State == "succeeded" {
					if _, ok := f.item(r.Target)["ask"]; ok || f.phase(r.Target) == "needs_input" {
						child = r.Target
						return true
					}
				}
			}
			return false
		})
		before := len(f.runsOf(child))
		f.webOK("PUT", "/settings/projects/"+f.Projects["acc-work"]+"/allowed-executors", `{"executors":[]}`, 200)
		f.answer(child, "A")
		f.waitFor("复核不通过", 20*time.Second, func() bool { return len(f.events(child, "resume.refused")) == 1 })
		time.Sleep(2 * time.Second)
		eq(t, "不续接，Run 数", len(f.runsOf(child)), before)
		eq(t, "resume.launched 数", len(f.resumeEvents(child)), 0)
		eq(t, "子事项阶段", f.phase(child), "needs_input")
	})
}
