//go:build e2e

package bgguards

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	exitDenied     = "4"
	exitTierClosed = "23"
	exitNotAllowed = "21"
)

// TestFlowBgGuards 对应验收文档 flow-bg-guards（ACT-BGG-01..07、09）；ACT-BGG-08 见 TestBgGuardsDefaultCap。
func TestFlowBgGuards(t *testing.T) {
	f := setup(t)
	f.auto("main-read", "hang-a", "hang-b", "main-short")

	f.step("ACT-BGG-01 运行者不能代答", func(t *testing.T) {
		f.mode("summary-only", preamble+`
$APA ask "$ITEM" --question 选哪个 --option A=甲 --option B=乙 > /dev/null 2>&1; rc ask
$APA decide "$ITEM" --option A --evidence x > "$OUT/decide.out" 2>&1; rc decide
$APA accept "$ITEM" --evidence x > "$OUT/accept.out" 2>&1; rc accept
$APA dismiss "$ITEM" --reason x --evidence x > "$OUT/dismiss.out" 2>&1; rc dismiss
`)
		id := f.task("bgg-01", "acc-work", "acc-main")
		f.settled(id, 1)
		eq(t, "运行者自己 ask（对照，应成功）", f.outFile("ask.rc"), "0")
		// 2026-11-20：运行凭据随所属 Agent 的档位；这次运行不归属放开了档位的 Agent，目标事项在范围内、档位未放开，得 23。
		for _, c := range []string{"decide", "accept", "dismiss"} {
			eq(t, "apa "+c+" 退出码", f.outFile(c+".rc"), exitTierClosed)
		}
		if n := len(f.events(id, "command.rejected")); n < 3 {
			t.Errorf("应各记一条拒绝事件（至少 3 条），实际 %d", n)
		}
		for _, typ := range []string{"decision.resolved", "proposal.accepted", "proposal.dismissed"} {
			if n := len(f.events(id, typ)); n != 0 {
				t.Errorf("事项不应出现 %s: %d 条", typ, n)
			}
		}
		eq(t, "事项阶段（决策仍待本人）", f.phase(id), "needs_input")
	})

	f.step("ACT-BGG-02 委派给名单外返回 21", func(t *testing.T) {
		f.mode("summary-only", preamble+`
$APA delegate --parent "$ITEM" --assignee other-host.cli > "$OUT/delegate.out" 2>&1; rc delegate
`)
		id := f.task("bgg-02", "acc-work", "acc-main")
		f.settled(id, 1)
		eq(t, "apa delegate 退出码", f.outFile("delegate.rc"), exitNotAllowed)
		if !strings.Contains(f.outFile("delegate.out"), "work-host.cli") {
			t.Errorf("错误应给出该项目允许的执行者: %s", f.outFile("delegate.out"))
		}
		if ids := f.listAssignee("other-host.cli"); len(ids) != 0 {
			t.Errorf("不应有名单外子事项: %v", ids)
		}
		eq(t, "未能委派，运行结束后事项转 needs_input", f.phase(id), "needs_input")
	})

	f.step("ACT-BGG-03 同一事项只有一个运行", func(t *testing.T) {
		f.mode("hang", "", "HANG_FOR=40s")
		id := f.task("bgg-03", "acc-work", "acc-hang")
		f.waitFor("第一个运行在跑", 20*time.Second, func() bool {
			rs := f.runsOf(id)
			return len(rs) == 1 && rs[0].State == "running"
		})
		// 同一事项再次命中规则（item.updated）。
		f.apa(f.DEL, 0, "item", "update", id, "--title", "bgg-03 改名", "--reason", "再次触发")
		f.waitFor("跳过并记 Event", 20*time.Second, func() bool { return len(f.events(id, "background.skipped")) == 1 })
		eq(t, "Run 数", len(f.runsOf(id)), 1)
		ctx := f.apa(f.OBS, 0, "context", "--scope", "item", "--item", id).Stdout
		if !strings.Contains(ctx, `"running":true`) {
			t.Errorf("apa context 应提示在途运行（running）: %s", ctx)
		}
		f.setMode("hang-b", "observe") // 运行结束时内核改事项，不应再触发
		f.mode("summary-only", "")
		f.waitFor("挂起的运行被超时终止或自然结束", 70*time.Second, func() bool { return f.runsOf(id)[0].State != "running" })
		eq(t, "Run 数（结束后仍只有一个）", len(f.runsOf(id)), 1)
	})

	f.step("ACT-BGG-03b 排队中的 Run 也算在途", func(t *testing.T) {
		f.StopKernel()
		cfg := filepath.Join(f.Home, "config.yaml")
		b, err := os.ReadFile(cfg)
		must(t, err)
		must(t, os.WriteFile(cfg, append(b, []byte("max_subprocesses: 1\n")...), 0o644))
		f.StartKernelAgain()
		f.setMode("hang-b", "auto")
		f.mode("hang", "", "HANG_FOR=40s")
		first := f.task("bgg-03b-first", "acc-work", "acc-hang")
		f.waitFor("占满额度的运行在跑", 20*time.Second, func() bool { rs := f.runsOf(first); return len(rs) == 1 && rs[0].State == "running" })
		second := f.task("bgg-03b-second", "acc-work", "acc-hang")
		f.waitFor("第二个事项的 Run 排队", 20*time.Second, func() bool { rs := f.runsOf(second); return len(rs) == 1 && rs[0].State == "queued" })
		f.apa(f.DEL, 0, "item", "update", second, "--title", "bgg-03b 改名", "--reason", "再次触发")
		f.waitFor("排队中的也跳过", 20*time.Second, func() bool { return len(f.events(second, "background.skipped")) == 1 })
		eq(t, "排队事项的 Run 数", len(f.runsOf(second)), 1)
		f.setMode("hang-b", "observe")
		f.mode("summary-only", "")
		f.waitFor("排队的运行跑完", 90*time.Second, func() bool {
			rs := f.runsOf(second)
			return len(rs) == 1 && rs[0].State != "queued" && rs[0].State != "running"
		})
		// 额度改回默认：下一步拨测试时钟会让到点的简报任务拉起，唯一的长时额度会被它占住。
		f.StopKernel()
		must(t, os.WriteFile(cfg, b, 0o644))
		f.StartKernelAgain()
	})

	// 2026-11-20 改写：运行故障（失败、超时）是事件不是问题——事项阶段不变、不挂问题，只有一次 Run。
	f.step("ACT-BGG-04 失败或超时只记事件，不交本人、不重试", func(t *testing.T) {
		f.mode("exit1", "")
		id := f.task("bgg-04-exit1", "acc-work", "acc-main")
		rs := f.settledFinal(id, 1)
		eq(t, "退出码 1 的 Run 状态", rs[0].State, "failed")
		eq(t, "退出码 1 时事项（阶段不变）", f.phase(id), "open")

		f.mode("hang", "")
		id2 := f.task("bgg-04-timeout", "acc-work", "acc-short")
		f.waitFor("超时用例的运行进入 running", 20*time.Second, func() bool {
			rs := f.runsOf(id2)
			return len(rs) == 1 && rs[0].State == "running"
		})
		f.TCAdvance(14460) // 推进测试时钟越过默认 4 小时时限
		rs2 := f.settledFinal(id2, 1)
		if rs2[0].State == "succeeded" {
			t.Errorf("超时的 Run 不应成功: %+v", rs2[0])
		}
		eq(t, "超时时事项（阶段不变）", f.phase(id2), "open")
		time.Sleep(6 * time.Second) // 不自动重试
		eq(t, "退出码 1 的 Run 数", len(f.runsOf(id)), 1)
		eq(t, "超时的 Run 数", len(f.runsOf(id2)), 1)
	})

	// 2026-11-20 改写：重启时中断是运行故障（lost）：Run 转 failed，事项阶段不变、不挂问题。
	f.step("ACT-BGG-05 重启时无存活进程按失败", func(t *testing.T) {
		f.mode("hang", preamble+`echo $PPID > "$OUT/stub.pid"
`)
		id := f.task("bgg-05", "acc-work", "acc-main")
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("事项 %s stub.pid=%q\n全部运行: %s\n事项事件: %s\n通知: %+v\n服务日志: %s", id, f.outFile("stub.pid"),
					f.apa(f.OBS, 0, "run", "list").Stdout, toJSON(f.events(id, "")), f.Notices(), f.ServiceLog())
			}
		})
		f.waitFor("运行中", 20*time.Second, func() bool {
			rs := f.runsOf(id)
			return len(rs) == 1 && rs[0].State == "running" && f.outFile("stub.pid") != ""
		})
		stub, err := strconv.Atoi(f.outFile("stub.pid"))
		must(t, err)
		must(t, syscall.Kill(f.ServePID(), syscall.SIGKILL))
		must(t, syscall.Kill(stub, syscall.SIGKILL))
		time.Sleep(500 * time.Millisecond)
		f.mode("summary-only", "")
		f.StartKernelAgain()
		f.waitFor("Run 转 failed", 30*time.Second, func() bool { return f.runsOf(id)[0].State == "failed" })
		time.Sleep(time.Second) // 等恢复收尾落账
		eq(t, "中断后的事项阶段（不转 needs_input）", f.phase(id), "open")
		eq(t, "Run 数", len(f.runsOf(id)), 1)
	})

	// 2026-11-26 改写（权限卡）：规则里的 access 已取消，工具档位一律取权限卡，规则写 edit 也按卡上的只读拉起。
	f.step("ACT-BGG-06 工具档位取权限卡，规则写的 access 不生效", func(t *testing.T) {
		f.auto("main-edit")
		id := f.task("bgg-06", "acc-work", "acc-edit")
		f.waitFor("拉起运行", 20*time.Second, func() bool { return len(f.runsOf(id)) == 1 })
		eq(t, "运行的工具档位", f.runsOf(id)[0].Access, "read")
	})

	f.step("ACT-BGG-07 声明未覆盖档位时不拉起", func(t *testing.T) {
		f.declareRaw("acc-read-only", map[string]any{
			"command": []string{filepath.Join(f.Root, "acc-harness.sh"), "{prompt}"},
			"session": map[string]any{"extract": "$.session_id"},
			"access":  map[string]any{"read": []string{"--access=read"}},
		})
		f.webOK("PUT", "/settings/agents/acc-read-only/card", `{"groups":{"view":{},"organize":{},"source_events":{}},"tool":"full"}`, 200)
		f.auto("ro-edit")
		id := f.task("bgg-07", "acc-work", "acc-ro")
		// 2026-11-20 改写：同 ACT-BGG-06，原等待事项转 needs_input。
		f.waitFor("诊断页记下没拉起", 20*time.Second, func() bool { return f.notified(id, "background_refused") })
		eq(t, "事项阶段（不转 needs_input）", f.phase(id), "open")
		eq(t, "Run 数（不回退到其他档位）", len(f.runsOf(id)), 0)
		eq(t, "拒绝记录", len(f.events(id, "background.refused")), 1)
	})

	f.step("ACT-BGG-09 策略变更后复核", func(t *testing.T) {
		f.mode("summary-only", preamble+`
$APA ask "$ITEM" --question 选哪个 --option A=甲 --option B=乙 --resume-note 选定后继续 > /dev/null 2>&1; rc ask
`)
		id := f.task("bgg-09", "acc-work", "acc-main")
		rs := f.settled(id, 1)
		eq(t, "ask 退出码", f.outFile("ask.rc"), "0")
		eq(t, "运行结束后事项", f.phase(id), "needs_input")
		f.setMode("main-read", "observe") // 策略变更：规则不再是 auto
		f.webOK("POST", "/items/"+id+"/decide", `{"choice":"A","evidence":"本人在 Web 选 A"}`, 200)
		f.waitFor("复核不通过", 20*time.Second, func() bool { return len(f.events(id, "resume.refused")) == 1 })
		time.Sleep(2 * time.Second)
		eq(t, "不续接，Run 数", len(f.runsOf(id)), len(rs))
		// 2026-11-20 改写（本人裁决："没拉起"类改为事件）：原断言事项仍 needs_input、待办流里有"续接未执行"的新问题；
		// 续接被拒不再挂问题：事项停在本人回答后的 open，诊断页有一条 resume_refused 通知（原因写明规则已不是 auto）告诉本人，
		// 待办流里不再有这件事。"不续接、Run 数不变"不变。
		eq(t, "事项阶段（回答后，不再转 needs_input）", f.phase(id), "open")
		if f.NoticeWith("resume_refused", "auto") != 1 || !f.notified(id, "resume_refused") {
			t.Errorf("复核不通过应在诊断页通知本人: %+v", f.Notices())
		}
		if inbox := f.apa(f.OBS, 0, "inbox").Stdout; strings.Contains(inbox, id) {
			t.Errorf("复核不通过不应再进待办: %s", inbox)
		}
	})
}

// TestBgGuardsDefaultCap 对应 ACT-BGG-08（2026-11-26 改写）：全新数据目录上没有任何权限卡，Agent 一律取默认卡（工具只读）。
func TestBgGuardsDefaultCap(t *testing.T) {
	e := baseline.New(t)
	e.StartKernel()
	body := e.Web("GET", "/v1/settings/access-limits", "")
	if body.Code != 200 || !strings.Contains(body.Body, `"agent_cards":{`) || strings.Contains(body.Body, `"tool":"full"`) || strings.Contains(body.Body, `"tool":"edit"`) {
		t.Fatalf("全新数据目录不应有放宽的权限卡: %d %s", body.Code, body.Body)
	}
}
