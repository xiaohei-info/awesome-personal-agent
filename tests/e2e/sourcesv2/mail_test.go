//go:build e2e

package sourcesv2

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// webSource 在 Web 来源页登记一个来源（本人身份；自定义命令只能走 Web）。
func (k *kit) webSource(body map[string]any) {
	k.t.Helper()
	b, _ := json.Marshal(body)
	k.webOK("POST", "/sources", string(b), 201)
}

func (k *kit) patchSource(id string, body map[string]any) {
	k.t.Helper()
	b, _ := json.Marshal(body)
	k.webOK("PATCH", "/sources/"+id, string(b), 200)
}

// clearToolLog 清空 $TOOL_LOG。
func (k *kit) clearToolLog() {
	k.t.Helper()
	if err := os.WriteFile(filepath.Join(k.ToolBin, "tool.log"), nil, 0o644); err != nil {
		k.t.Fatal(err)
	}
}

func TestFlowSourceMail(t *testing.T) {
	k := setup(t)
	const src = "src_mail"
	home := k.Projects["acc-home"]

	k.step("ACT-MAIL-01 登记邮件适配器（录制样本）", func(t *testing.T) {
		k.SetToolCase("mail-1.1.0")
		// lookback_days 放大：样本邮件固定在 2026-11-06，默认只回看 7 天，真实日期一过就读不到。
		k.webSource(map[string]any{"id": src, "type": "script", "command": filepath.Join(progDir, "mail-himalaya"), "schedule": "1s", "timeout": 20,
			"source_type": "mail", "dedupe_domain": "acc-mail", "default_project": home,
			"config": map[string]any{"account": "acc@example.com", "lookback_days": 36500}})
		k.clearToolLog()
		s := k.show(src)
		if !s.Enabled || s.SourceType != "mail" || s.DedupeDomain != "acc-mail" || s.DefaultProject != home {
			t.Errorf("来源登记不对: %+v", s)
		}
		_ = filepath.Walk(k.Home, func(p string, info os.FileInfo, err error) error {
			if err == nil && strings.Contains(strings.ToLower(info.Name()), "himalaya") {
				t.Errorf("数据目录下不应有邮件工具的配置或口令文件: %s", p)
			}
			return nil
		})
		if _, err := os.Stat(filepath.Join(k.Home, "sources", src)); err == nil {
			t.Error("邮件来源不应有凭据目录")
		}
	})

	k.step("ACT-MAIL-02 新邮件成为标准事件并交初筛", func(t *testing.T) {
		k.clsYes("mid probe", 0.05)
		for _, m := range []string{"mid-4", "mid-5", "mid-6"} {
			id := "<" + m + "@example.com>"
			ev := k.settled(src, id)
			if ev.Type != "mail.received" || len(ev.CorrelationKeys) != 1 || ev.CorrelationKeys[0] != "thread:"+id {
				t.Errorf("%s: 关联键应只有 thread:<自己的 Message-ID>: %+v", id, ev.CorrelationKeys)
			}
			if ev.SenderVerified {
				t.Errorf("%s: sender_verified 不应为 true", id)
			}
			if !k.eventRule(ev, "mail-received") {
				t.Errorf("%s: 应命中 mail-received", id)
			}
			if j := k.judgment(ev); !strings.Contains(j.Template, "triage-screen") {
				t.Errorf("%s: 应经 triage-screen 初筛: %+v", id, j)
			}
		}
		eq(t, "来源事件数", len(k.events(src)), 3)
	})

	k.step("ACT-MAIL-03 拉取只调用只读命令", func(t *testing.T) {
		calls := k.toolCalls("himalaya")
		var versions, lists, reads int
		for _, argv := range calls {
			joined := strings.Join(argv, " ")
			switch {
			case len(argv) == 1 && argv[0] == "--version":
				versions++
			case len(argv) >= 2 && argv[0] == "envelope" && argv[1] == "list":
				lists++
			case len(argv) >= 2 && argv[0] == "message" && argv[1] == "read" && strings.Contains(joined, "--preview"):
				reads++
			default:
				t.Errorf("出现了版本查询、envelope list、带 --preview 的 message read 之外的调用: %v", argv)
			}
			if strings.Contains(joined, "BODY-PLACEHOLDER") || strings.Contains(joined, "mid probe") {
				t.Errorf("邮件内容不应出现在命令行参数里: %v", argv)
			}
		}
		if versions == 0 || lists == 0 || reads < 3 {
			t.Errorf("应有版本查询、envelope list 与每封一次的 message read: %d/%d/%d", versions, lists, reads)
		}
	})

	// 另一个来源：一个脚本来源，验证邮件来源出错时它照常运行。
	other := filepath.Join(k.Root, "other-source.sh")
	if err := os.WriteFile(other, []byte("#!/bin/sh\ncat >/dev/null\necho '{\"cursor\":\"c1\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	k.RegisterScriptSource("src_other", other)

	// failsAs 把工具替身切到 c，等邮件来源再跑一次并确认：退出码 2、生成 needs_input、游标不动。
	failsAs := func(t *testing.T, c string) (SrcState, SrcState) {
		before := k.show(src)
		k.SetToolCase(c)
		var s SrcState
		k.until("case "+c+" 的拉取失败并生成 needs_input", func() bool {
			s = k.show(src)
			return s.Stats.Runs > before.Stats.Runs && strings.HasPrefix(s.Stats.LastError, exitConfig)
		})
		if s.Cursor != before.Cursor {
			t.Errorf("游标不应前进: %q → %q", before.Cursor, s.Cursor)
		}
		if s.Stats.NeedsInputItem == "" || k.item(s.Stats.NeedsInputItem).Phase != "needs_input" {
			t.Errorf("应生成 needs_input 事项: %+v", s.Stats)
		}
		return before, s
	}

	k.step("ACT-MAIL-04 工具未安装时生成 needs_input", func(t *testing.T) {
		otherRuns := k.show("src_other").Stats.Runs
		bin, off := filepath.Join(k.ToolBin, "himalaya"), filepath.Join(k.ToolBin, "himalaya.off")
		if err := os.Rename(bin, off); err != nil {
			t.Fatal(err)
		}
		_, s := failsAs(t, "mail-1.1.0")
		if err := os.Rename(off, bin); err != nil {
			t.Fatal(err)
		}
		if k.show("src_other").Stats.Runs <= otherRuns {
			t.Error("其他来源应照常运行")
		}
		// 放回后恢复拉取，不产生重复。
		k.until("放回后下一周期照常拉取", func() bool {
			return k.show(src).Stats.ConsecutiveFailures == 0 && k.show(src).Stats.Runs > s.Stats.Runs
		})
		eq(t, "来源事件数", len(k.events(src)), 3)
	})

	k.step("ACT-MAIL-05 版本不在声明范围时生成 needs_input", func(t *testing.T) {
		k.clearToolLog()
		_, s := failsAs(t, "mail-version-0.1")
		if !strings.Contains(s.Stats.LastError, "exit status 2") {
			t.Errorf("退出码应为 2: %+v", s.Stats)
		}
		for _, argv := range k.toolCalls("himalaya") {
			if len(argv) != 1 || argv[0] != "--version" {
				t.Errorf("版本查询之后不应再调用取数命令: %v", argv)
			}
		}
		if !strings.Contains(k.item(s.Stats.NeedsInputItem).Raw["title"].(string), src) {
			t.Errorf("needs_input 事项应指向来源 %s", src)
		}
	})

	k.step("ACT-MAIL-06 认证失败时生成 needs_input", func(t *testing.T) {
		failsAs(t, "mail-auth-fail")
		k.SetToolCase("mail-1.1.0-new")
		ev := k.settled(src, "<mid-7@example.com>")
		if ev.Type != "mail.received" {
			t.Errorf("恢复后 mid-7 应入账: %+v", ev)
		}
		eq(t, "来源事件数（已有三条不重复）", len(k.events(src)), 4)
	})

	k.step("ACT-MAIL-07 输出无法解析时不前进", func(t *testing.T) {
		before, _ := failsAs(t, "mail-garbled")
		eq(t, "来源事件数", len(k.events(src)), 4)
		if s := k.show(src); s.Cursor != before.Cursor || len(k.rejects(src)) != 0 {
			t.Errorf("不应输出新游标、不应跳过字段继续: %+v", s)
		}
	})

	k.step("ACT-MAIL-08 两个邮件适配器的契约测试含线程头样本", func(t *testing.T) {
		cmd := exec.Command("go", "test", "-count=1", "./sources/mail/...", "./sources/applepim/...")
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go test ./sources/mail/... ./sources/applepim/...: %v\n%s", err, out)
		}
		for _, f := range []string{"sources/mail/testdata/1.1.0/version.txt", "sources/mail/testdata/1.1.0/list.json", "sources/mail/testdata/2.0.0/version.txt",
			"sources/applepim/testdata/messages.json", "sources/applepim/testdata/messages-thread.json", "sources/applepim/testdata/get-mid-p2.json", "sources/applepim/testdata/get-mid-p3.json"} {
			if _, err := os.Stat(filepath.Join(repoRoot(), f)); err != nil {
				t.Errorf("缺录制样本 %s", f)
			}
		}
		thread, _ := filepath.Glob(filepath.Join(repoRoot(), "sources/mail/testdata/1.1.0/thread/*"))
		if len(thread) == 0 {
			t.Error("himalaya 缺带 References/In-Reply-To 头的回信样本")
		}
		// 两个适配器的测试都断言回信的 correlation_keys 含原信的 thread:<Message-ID>。
		for _, f := range []string{"sources/mail/mail_test.go", "sources/applepim/applepim_test.go"} {
			b, err := os.ReadFile(filepath.Join(repoRoot(), f))
			if err != nil || !strings.Contains(string(b), "thread:") {
				t.Errorf("%s 缺线程关联键断言: %v", f, err)
			}
		}
		// 结论已写回附录 D.3 的 MEAS-MAIL-THREAD。
		d, err := os.ReadFile(filepath.Join(repoRoot(), "docs/appendix-d-adapters.md"))
		if err != nil || !strings.Contains(string(d), "MEAS-MAIL-THREAD") || !strings.Contains(string(d), "录制样本部分（W1 开发验收）已有结论") {
			t.Errorf("附录 D 缺 MEAS-MAIL-THREAD 的开发验收结论: %v", err)
		}
	})

	k.step("ACT-MAIL-09 换工具不动规则和内核", func(t *testing.T) {
		k.SetToolCase("mail-1.1.0-new") // MAIL-07 的损坏输出已撤，邮箱回到 mid-4..7
		k.clsYes("ACT-MAIL-09 新邮件", 0.05)
		script := filepath.Join(k.Root, "acc-mail-adapter.sh")
		// 走 §9.2 的验收脚本：输出与样本同一 Message-ID 的事件和一封新邮件，游标原样交回（切回原适配命令时它仍是合法的时间游标）。
		body := `#!/bin/sh
cursor=$(sed -n 's/.*"cursor":"\([^"]*\)".*/\1/p')
echo '{"id":"<mid-5@example.com>","type":"mail.received","subject":"[APA-LIVE-TEST] mid probe 2","occurred_at":"2026-11-06T05:00:00Z","correlation_keys":["thread:<mid-5@example.com>"],"sender":"agent@example.com"}'
echo '{"id":"<mid-8@example.com>","type":"mail.received","subject":"ACT-MAIL-09 新邮件","occurred_at":"2026-11-06T10:00:00Z","correlation_keys":["thread:<mid-8@example.com>"],"sender":"agent@example.com"}'
echo '{"cursor":"'"$cursor"'"}'
`
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		items := len(k.suggestions())
		orig := k.show(src)
		k.patchSource(src, map[string]any{"command": script})
		ev := k.settled(src, "<mid-8@example.com>")
		if ev.Type != "mail.received" || !k.eventRule(ev, "mail-received") {
			t.Errorf("mail-received 应照常匹配新事件: %+v", ev)
		}
		n := 0
		for _, e := range k.events(src) {
			if e.DedupeKey.EventID == "<mid-5@example.com>" {
				n++
			}
		}
		eq(t, "<mid-5@example.com> 的事件条数", n, 1)
		eq(t, "建议流条数", len(k.suggestions()), items)
		k.patchSource(src, map[string]any{"command": filepath.Join(progDir, "mail-himalaya")})
		k.until("改回原适配命令后正常拉取", func() bool {
			s := k.show(src)
			return s.Stats.ConsecutiveFailures == 0 && s.Stats.Runs > orig.Stats.Runs+2
		})
		eq(t, "来源事件数", len(k.events(src)), 5)
	})

	var rootEv SrcEvent
	var root string // <mid-t1@example.com> 建出的事项
	k.step("ACT-MAIL-10 回信与原信共用线程关联键", func(t *testing.T) {
		const subject = "[APA-LIVE-TEST] thread root"
		k.clsYes("thread root", 0.9)
		k.draft(subject, map[string]any{"title": "线程根邮件", "kind": "task", "recommend": "accept"})
		k.SetToolCase("mail-thread-1") // 先只放原信
		rootEv = k.settled(src, "<mid-t1@example.com>")
		if len(rootEv.CorrelationKeys) == 0 || !contains(rootEv.CorrelationKeys, "thread:<mid-t1@example.com>") {
			t.Errorf("原信的 correlation_keys 应含 thread:<mid-t1@example.com>: %v", rootEv.CorrelationKeys)
		}
		if rootEv.Disposition != "proposed" || rootEv.Item == "" {
			t.Fatalf("原信应成为建议: %+v", rootEv)
		}
		root = rootEv.Item
	})

	// waitFor 把事项转 waiting（等外部回音）。
	waitExternal := func(item string) {
		k.apa(k.DEL, 0, "item", "transition", item, "--to", "waiting", "--reason", "等对方回信", "--waiting-on", "external:报价", "--evidence", "本人：等对方回信")
		eq(k.t, "转 waiting 后阶段", k.item(item).Phase, "waiting")
	}
	// reminded 报告事项是否被跟进梯度提醒过（followup.reminded）。
	reminded := func(item string) bool { return len(k.audit("followup.reminded", item)) > 0 }

	k.step("ACT-MAIL-11 等回音的事项收到回信后回到 open", func(t *testing.T) {
		k.webOK("POST", "/items/"+root+"/accept", `{}`, 200)
		waitExternal(root)
		// 对照事项：同样等外部回音但没有回信。跟进定时没有 API 可读，用测试时钟推过 72 小时间隔，看谁被提醒。
		ctl := k.idOf(k.apa(k.DEL, 0, "item", "add", "--kind", "task", "--title", "对照等待事项", "--phase", "open", "--project", home, "--evidence", "对照"))
		waitExternal(ctl)
		before := k.item(root)
		sug := len(k.suggestions())
		reqs := len(k.ModelStub.Requests())

		k.SetToolCase("mail-thread-2")
		reply := k.settled(src, "<mid-t2@example.com>")
		if reply.Disposition != "attached" || reply.AttachState != "processed" || reply.Item != root {
			t.Fatalf("回信应 attached 到原事项、attach_state=processed: %+v", reply)
		}
		after := k.item(root)
		if after.Phase != "open" || after.Title != before.Title || after.Raw["body"] != before.Raw["body"] {
			t.Errorf("事项应回到 open、标题与正文不变: %+v", after)
		}
		var wake WhyStep
		for _, s := range k.why(root) {
			if s.Type == "item.phase_changed" && strings.Contains(s.Reason, "收到回音") {
				wake = s
			}
		}
		if wake.Actor.Type != "system" || !strings.Contains(wake.Reason, reply.ID) {
			t.Errorf("Event 操作者应为 system、reason 含\"收到回音\"和来源事件 id: %+v", wake)
		}
		if !strings.Contains(k.apa(k.OBS, 0, "item", "show", root).Stdout, reply.ID) {
			t.Error("回信应作为挂靠消息显示在事项上")
		}
		eq(t, "建议流条数", len(k.suggestions()), sug)
		if k.hasDraftRequest("Re: [APA-LIVE-TEST] thread root") {
			t.Error("MODEL_LOG 不应有该回信的起草请求")
		}
		_ = reqs

		// 72 小时过去：没回信的对照事项被提醒，已回到 open 的事项没有（未到期的跟进定时已取消）。
		k.TCAdvance(73 * 3600)
		k.until("对照事项的跟进定时到期并提醒", func() bool { return reminded(ctl) })
		if reminded(root) {
			t.Error("回信后跟进定时应已取消，不应再提醒")
		}
	})

	k.step("ACT-MAIL-12 只有 In-Reply-To 的回信同样归属", func(t *testing.T) {
		waitExternal(root)
		k.SetToolCase("mail-thread")
		reply := k.settled(src, "<mid-t3@example.com>")
		if !contains(reply.CorrelationKeys, "thread:<mid-t1@example.com>") {
			t.Errorf("correlation_keys 应含 thread:<mid-t1@example.com>: %v", reply.CorrelationKeys)
		}
		if reply.Disposition != "attached" || reply.Item != root {
			t.Errorf("应挂到同一事项: %+v", reply)
		}
		k.until("事项回到 open", func() bool { return k.item(root).Phase == "open" })
	})

	var pimRoot string
	k.step("ACT-MAIL-13 桌面邮件应用适配器同样产出线程关联键", func(t *testing.T) {
		k.patchSource(src, map[string]any{"enabled": false})
		k.clearToolLog()
		const pim = "src_pimmail"
		k.webSource(map[string]any{"id": pim, "type": "script", "command": filepath.Join(progDir, "apple-pim-source"), "schedule": "1s", "timeout": 20,
			"source_type": "mail", "dedupe_domain": "acc-pim-mail", "default_project": home,
			"config": map[string]any{"kind": "mail", "account": "acc", "bin_dir": k.ToolBin, "lookback_days": 36500}})
		k.clsYes("报价", 0.9)
		k.draft("报价", map[string]any{"title": "报价邮件", "kind": "task", "recommend": "accept"})
		k.SetToolCase("pim-mail-thread-1") // 先放原信
		orig := k.settledPrefix(pim, "")
		if orig.Disposition != "proposed" || orig.Item == "" {
			t.Fatalf("原信应成为建议: %+v", orig)
		}
		pimRoot = orig.Item
		k.webOK("POST", "/items/"+pimRoot+"/accept", `{}`, 200)
		waitExternal(pimRoot)
		k.SetToolCase("pim-mail-thread")
		var reply SrcEvent
		k.until("回信入账并挂靠", func() bool {
			for _, e := range k.events(pim) {
				if e.ID != orig.ID && e.Disposition != "pending" {
					reply = e
					return true
				}
			}
			return false
		})
		if len(orig.CorrelationKeys) == 0 || !contains(reply.CorrelationKeys, orig.CorrelationKeys[0]) || !strings.HasPrefix(orig.CorrelationKeys[0], "thread:") {
			t.Errorf("两条事件的 correlation_keys 都应含原信的 thread 键: 原信 %v 回信 %v", orig.CorrelationKeys, reply.CorrelationKeys)
		}
		if reply.Disposition != "attached" || reply.Item != pimRoot {
			t.Errorf("回信应挂到原事项: %+v", reply)
		}
		k.until("事项回到 open", func() bool { return k.item(pimRoot).Phase == "open" })
		for _, argv := range k.toolCalls("mail-cli") {
			if (argv[0] != "messages" && argv[0] != "get") || !contains(argv, "--engine=sqlite") {
				t.Errorf("mail-cli 只能是带 --engine=sqlite 的 messages、get: %v", argv)
			}
		}
		if len(k.toolCalls("mail-cli")) == 0 {
			t.Error("TOOL_LOG 里没有 mail-cli 调用")
		}
		k.patchSource(pim, map[string]any{"enabled": false})
		k.patchSource(src, map[string]any{"enabled": true})
	})

	var old, o2Suggestion string
	k.step("ACT-MAIL-14 没有线程关联键的旧事项收到回信时成为带补到候选的新建议", func(t *testing.T) {
		old = k.idOf(k.apa(k.DEL, 0, "item", "add", "--kind", "task", "--title", "等王经理回复报价", "--body", "已发报价给王经理 wang@example.com，等他回复。",
			"--phase", "open", "--project", home, "--evidence", "本人：等王经理回复报价"))
		waitExternal(old)
		k.clsYes("Re: 报价", 0.9)
		k.draft("Re: 报价", map[string]any{"title": "王经理回复了报价", "kind": "task", "recommend": "attach", "attach_to": old})
		sug := len(k.suggestions())
		k.SetToolCase("mail-old-reply")
		ev := k.settled(src, "<mid-o2@example.com>")
		if ev.Disposition != "proposed" || ev.Item == "" || ev.Item == old {
			t.Fatalf("回信应成为新建议（不挂靠、不丢弃）: %+v", ev)
		}
		var cand bool
		for _, r := range k.ModelStub.Requests() {
			if strings.Contains(r.BodyText, "起草一条建议") && strings.Contains(r.BodyText, "Re: 报价") && strings.Contains(r.BodyText, old) {
				cand = true
			}
		}
		if !cand {
			t.Error("起草请求的候选清单应含该旧事项")
		}
		prop, _ := k.item(ev.Item).Raw["proposal"].(map[string]any)
		if prop["recommend"] != "attach" || prop["attach_to"] != old {
			t.Errorf("建议应 recommend=attach、attach_to 为旧事项: %v", prop)
		}
		eq(t, "旧事项阶段", k.item(old).Phase, "waiting")
		eq(t, "建议流条数", len(k.suggestions()), sug+1)
		o2Suggestion = ev.Item
	})

	k.step("ACT-MAIL-15 本人为旧事项登记线程关联键后，后续回信回到原事项", func(t *testing.T) {
		// 本人不采纳"补到旧事项"，丢弃这条回信建议，改为登记线程键（验收文档在 ACT-MAIL-14 与 15 之间没写这一步）。
		// 不处置它，同线程键会同时命中"旧事项"和这条仍是 proposed 的建议两个未关闭事项，内核不猜（设计 §9.4 "不猜"），
		// 后续回信只会并进那条建议；而 attach 会替本人把键登记到旧事项上，之后 relate 就是冲突（退出码 6）。
		k.apa(k.DEL, 0, "dismiss", o2Suggestion, "--reason", "改为登记线程键", "--evidence", "本人：这封线程我自己登记到报价事项")
		sug := len(k.suggestions())
		k.SessionKind = "direct"
		k.apa(k.DEL, 0, "item", "relate", old, "--key", "thread:<mid-o1@example.com>", "--domain", "acc-mail", "--evidence", "本人原话：这封线程归到报价事项")
		k.SessionKind = ""
		var detail struct {
			Keys []struct{ Domain, Key string } `json:"correlation_keys"`
		}
		if err := json.Unmarshal([]byte(k.apa(k.OBS, 0, "item", "show", old).Stdout), &detail); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range detail.Keys {
			found = found || (c.Domain == "acc-mail" && c.Key == "thread:<mid-o1@example.com>")
		}
		if !found {
			t.Errorf("事项详情的关联来源线程应显示该键: %+v", detail.Keys)
		}
		k.SetToolCase("mail-old-reply-2")
		ev := k.settled(src, "<mid-o3@example.com>")
		if ev.Disposition != "attached" || ev.Item != old {
			t.Errorf("回信应挂到旧事项: %+v", ev)
		}
		k.until("事项回到 open", func() bool { return k.item(old).Phase == "open" })
		eq(t, "建议流条数", len(k.suggestions()), sug)
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
