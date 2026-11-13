//go:build e2e

package sourcesv2

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// 代码托管样本 case 对外的"小时"（相对安装时刻，见 ghAt），按验收文档里的出场顺序递增。
const (
	hOpened, hLabeled, hOpened2, hOpened3, hPRClosed, hPRUpdated7, hAgentsMD = 1, 2, 3, 4, 5, 6, 7
)

// noDelivery 断言渠道没有可领取的投递（建议不打扰本人）。
func (k *kit) noDelivery() {
	k.t.Helper()
	if r := k.APIRaw("GET", "/v1/deliveries/next?wait=1", "", k.CHAN); r.Code != http.StatusNoContent {
		k.t.Errorf("CHAN 不应领到投递: %d %s", r.Code, r.Body)
	}
}

// TestFlowSourceGithub 按验收文档 flow-source-github 的顺序执行 ACT-GH-01..10。
func TestFlowSourceGithub(t *testing.T) {
	k := setup(t)
	const src = "src_gh"
	var prRef = "https://github.com/acc-org/acc-repo/pull/7"
	var accepted, acceptedEvent, acceptedJudgment string

	k.step("ACT-GH-01 登记代码托管来源", func(t *testing.T) {
		k.webOK("POST", "/projects/"+k.Projects["acc-work"]+"/repos", `{"op":"add_remote","remote":"https://github.com/acc-org/acc-repo"}`, 200)
		k.SetToolCase("gh-empty")
		k.addPull(src, "github-source", "github", map[string]any{"credentials-path": filepath.Join(k.Home, "sources", src)},
			map[string]any{"repos": []string{"acc-org/acc-repo"}})
		k.cycle(src, 1)
		s := k.show(src)
		if !s.Enabled || s.SourceType != "github" {
			t.Errorf("来源应 enabled、source_type=github: %+v", s)
		}
		n := 0
		for _, argv := range k.toolCalls("gh") {
			if argv[0] != "api" {
				t.Errorf("gh 只能调用只读的 api: %v", argv)
			}
			n++
		}
		if n == 0 {
			t.Error("TOOL_LOG 里没有 gh 的 api 调用")
		}
	})

	k.step("ACT-GH-02 新 Issue 经初筛与起草成为建议，按仓库归项目", func(t *testing.T) {
		k.clsYes("导出文件乱码", 0.9)
		k.draft("导出文件乱码", map[string]any{"title": "修复导出乱码", "kind": "task", "recommend": "accept"})
		k.ghAt("gh-issue-opened", hOpened)
		ev := k.settled(src, "issue-acc-org-acc-repo#11-opened")
		if ev.Disposition != "proposed" || ev.Item == "" || ev.Type != "issue.opened" {
			t.Fatalf("事件应成为建议: %+v", ev)
		}
		accepted, acceptedEvent, acceptedJudgment = ev.Item, ev.ID, k.judgment(ev).ID
		it := k.item(accepted)
		if it.Title != "修复导出乱码" || it.Phase != "proposed" || it.Project != k.Projects["acc-work"] {
			t.Errorf("建议应是\"修复导出乱码\"、proposed、按远程地址归入 acc-work: %+v", it)
		}
		if !k.ruleHit(accepted, "inbound-triage/source_rules/github-issue") {
			t.Error("应命中 github-issue 规则")
		}
		if j := k.judgment(ev); j.Template == "" || !strings.Contains(j.Template, "triage-screen") || j.DecidedBy != "classifier" {
			t.Errorf("应经 triage-screen 初筛、由分类端点定案: %+v", j)
		}
		if k.modelAbout("导出文件乱码") < 1 || !k.hasDraftRequest("导出文件乱码") {
			t.Error("MODEL_LOG 应有这条事件的起草请求")
		}
		if n := len(k.Cls.Batches()); n == 0 {
			t.Error("CLS_LOG 应有 triage-screen 题")
		}
		k.noDelivery()
		ctx := k.apa(k.DEL, 0, "context", "--session", "s1", "--scope", "project", "--project", k.Projects["acc-work"], "--format", "inject").Stdout
		if !strings.Contains(ctx, accepted) || !strings.Contains(ctx, "阶段 proposed") {
			t.Errorf("apa context 应能看到这条建议: %s", ctx)
		}
	})

	var low SrcEvent
	k.step("ACT-GH-03 初筛落入不确定带且对话端点也未定时留为建议并标把握低", func(t *testing.T) {
		const subject = "登录页文案错别字"
		k.clsYes(subject, 0.4)
		k.chatJudge(subject, "yes", false)
		k.draft(subject, map[string]any{"title": "修正登录页错别字", "kind": "task", "recommend": "accept"})
		k.ghAt("gh-issue-labeled", hLabeled)
		low = k.settledPrefix(src, "issue-acc-org-acc-repo#12-")
		if low.Disposition != "proposed" || low.Item == "" {
			t.Fatalf("事件应成为建议: %+v", low)
		}
		if n := k.judgeRequests(subject); n < 1 {
			t.Errorf("MODEL_LOG 应有同一道初筛题的重答请求，实际 %d", n)
		}
		j := k.judgment(low)
		if len(j.Levels) != 2 || j.Levels[0].Backend != "classifier" || j.Levels[1].Backend != "chat" || j.DecidedBy != "default" {
			t.Errorf("判断记录应有分类端点与对话端点两级、由保守默认定案: %+v", j)
		}
		if p := k.item(low.Item).Raw["proposal"].(map[string]any); p["low_confidence"] != true {
			t.Errorf("建议应标把握低: %v", p)
		}
		// apa why 回放该来源事件：处置步骤展开判断记录，两级逐级列出。
		d, ok := stepOf(k.why(low.ID), "inbound.disposed")
		if !ok || d.Judgment == nil || d.Judgment.Record == nil || d.Judgment.ID != j.ID || d.Judgment.DecidedBy != "default" || len(d.Judgment.Record.Levels) != 2 {
			t.Errorf("apa why <来源事件> 应展开两级判断、由 default 定案: %+v", d)
		}
	})

	k.step("ACT-GH-04 初筛判为不值得时只记录", func(t *testing.T) {
		const subject = "夜间同步偶尔超时"
		k.clsYes(subject, 0.05)
		sug := k.suggestions()
		k.ghAt("gh-issue-opened-2", hOpened2)
		ev := k.settled(src, "issue-acc-org-acc-repo#13-opened")
		if ev.Disposition != "ignored" || ev.Item != "" {
			t.Fatalf("事件应 ignored、无事项: %+v", ev)
		}
		if k.hasDraftRequest(subject) {
			t.Error("MODEL_LOG 不应有起草请求")
		}
		eq(t, "建议流条数", len(k.suggestions()), len(sug))
		k.noDelivery()
	})

	k.step("ACT-GH-05 接受后进入决策并送达", func(t *testing.T) {
		bot := k.StartChannelExt()
		k.webOK("POST", "/items/"+accepted+"/accept", `{}`, 200)
		eq(t, "接受后阶段", k.item(accepted).Phase, "open")
		// 接受后的事项没有指派；worker 只能对自己的事项提问，先由本人（DEL 附原话）指给 work-host.cli。
		k.apa(k.DEL, 0, "item", "update", accepted, "--assignee", "work-host.cli", "--evidence", "这个交给 work-host 处理")
		k.apa(k.WRK, 0, "ask", accepted, "--question", "这个 Issue 先处理哪部分", "--option", "A=先排查", "--option", "B=先回复", "--next", "A=done", "--recommend", "A", "--free-text")
		eq(t, "ask 后阶段", k.item(accepted).Phase, "needs_input")
		k.until("渠道 ext 把决策发到机器人替身", func() bool {
			for _, m := range bot.Sent() {
				if strings.Contains(m.Text, "修复导出乱码") {
					return true
				}
			}
			return false
		})
	})

	k.step("ACT-GH-06 决定后回放完整来龙去脉", func(t *testing.T) {
		k.webOK("POST", "/items/"+accepted+"/decide", `{"choice":"A"}`, 200)
		k.until("事项进入 done", func() bool { return k.item(accepted).Phase == "done" })
		steps := k.why(accepted)
		created, ok := stepOf(steps, "item.created")
		if !ok || created.Basis.RuleID != "inbound-triage/source_rules/github-issue" {
			t.Errorf("回放应含命中规则 github-issue 的建立步骤: %+v", created)
		}
		item, _ := created.Payload["item"].(map[string]any)
		ext, _ := item["ext"].(map[string]any)
		srcEv, _ := ext["source_event"].(string)
		if srcEv != acceptedEvent {
			t.Errorf("回放应指向来源事件 %s，实际 %q", acceptedEvent, srcEv)
		}
		// 来源事件一侧：初筛的判断记录 id 与定案者。
		d, ok := stepOf(k.why(srcEv), "inbound.disposed")
		if !ok || d.Judgment == nil || d.Judgment.ID != acceptedJudgment || d.Judgment.DecidedBy != "classifier" {
			t.Errorf("来源事件回放应含初筛判断记录 %s 及定案者 classifier: %+v", acceptedJudgment, d)
		}
		acc, ok := stepOf(steps, "item.phase_changed")
		if !ok || acc.Actor.Principal != "owner" || acc.Reason != "proposal accepted" {
			t.Errorf("回放应含本人的接受: %+v", acc)
		}
		res, ok := stepOf(steps, "decision.resolved")
		if !ok || res.Actor.Principal != "owner" || res.Reason != "decision answered" {
			t.Errorf("回放应含本人的最终决定: %+v", res)
		}
		after, _ := res.Payload["item"].(map[string]any)
		if ask, _ := after["ask"].(map[string]any); ask["resolution"].(map[string]any)["choice"] != "A" {
			t.Errorf("最终决定应是 A: %v", ask)
		}
		if _, ok := stepOf(steps, "decision.requested"); !ok {
			t.Error("回放应含决策请求")
		}
		if !ordered(steps, "item.created", "item.phase_changed", "decision.requested", "decision.resolved") {
			t.Error("回放应依次为建立、接受、决定")
		}
	})

	k.step("ACT-GH-07 认证失败时找本人", func(t *testing.T) {
		k.clsYes("设置页保存后无提示", 0.05)
		before := k.show(src)
		k.SetToolCase("gh-auth-401")
		var s SrcState
		k.until("401 的拉取生成 needs_input 事项", func() bool {
			s = k.show(src)
			return s.Stats.NeedsInputItem != "" && s.Stats.Runs > before.Stats.Runs
		})
		if !strings.HasPrefix(s.Stats.LastError, exitConfig) {
			t.Errorf("拉取程序退出码应为 2: %+v", s.Stats)
		}
		if s.Cursor != before.Cursor {
			t.Errorf("游标不应前进: %q → %q", before.Cursor, s.Cursor)
		}
		ni := k.item(s.Stats.NeedsInputItem)
		if ni.Phase != "needs_input" {
			t.Errorf("应生成 needs_input 事项: %+v", ni)
		}
		// 恢复后，401 期间到期的事件入账，不漏。
		k.ghAt("gh-issue-opened-3", hOpened3)
		ev := k.settled(src, "issue-acc-org-acc-repo#14-opened")
		if ev.Disposition != "ignored" {
			t.Errorf("恢复后的事件应入账并按初筛处置: %+v", ev)
		}
	})

	k.step("ACT-GH-08 PR 合并与关闭未命中事项时按规则忽略，不调模型", func(t *testing.T) {
		lines, sug := k.modelLines(), len(k.suggestions())
		k.ghAt("gh-pr-closed", hPRClosed)
		merged := k.settledPrefix(src, "pr-acc-org-acc-repo#5-merged")
		closed := k.settledPrefix(src, "pr-acc-org-acc-repo#6-closed")
		for _, ev := range []SrcEvent{merged, closed} {
			if ev.Disposition != "ignored" || ev.Item != "" {
				t.Errorf("%s 应按规则 ignored: %+v", ev.DedupeKey.EventID, ev)
			}
			if !k.eventRule(ev, "pr-activity") {
				t.Errorf("%s 应命中 pr-activity", ev.DedupeKey.EventID)
			}
		}
		eq(t, "模型替身日志新增行数", k.modelLines()-lines, 0)
		eq(t, "建议流条数", len(k.suggestions()), sug)
	})

	k.step("ACT-GH-09 PR 更新命中已委派事项的关联键时挂靠", func(t *testing.T) {
		parent := k.idOf(k.apa(k.DEL, 0, "item", "add", "--kind", "task", "--title", "补充导出单测", "--phase", "open",
			"--project", k.Projects["acc-work"], "--evidence", "建父事项"))
		child := k.idOf(k.apa(k.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--evidence", "委派"))
		k.apa(k.WRK, 0, "report", child, "--outcome", "progress", "--summary", "PR 已开", "--pr", prRef)
		sug := len(k.suggestions())
		k.ghAt("gh-pr-updated-7", hPRUpdated7)
		ev := k.settledPrefix(src, "pr-acc-org-acc-repo#7-updated")
		if ev.Disposition != "attached" || ev.Item != child || ev.AttachState != "pending" {
			t.Errorf("事件应 attached 到子事项且 attach_state=pending: %+v", ev)
		}
		// 建父/子事项会触发与来源事件无关的"一句话摘要"请求，所以只数提到这条 PR 事件的请求。
		eq(t, "两份模型替身日志里提到该事件的请求", k.modelAbout("acc-org/acc-repo/pull/7"), 0)
		eq(t, "建议流条数", len(k.suggestions()), sug)
		for _, e := range k.events(src) {
			if e.Item == child && e.ID != ev.ID && strings.HasPrefix(e.DedupeKey.EventID, "pr-acc-org-acc-repo#7-") {
				t.Errorf("同一 PR 不应产生重复事件: %+v", e)
			}
		}
	})

	k.step("ACT-GH-10 规范文件变化事件被忽略并交规范检测", func(t *testing.T) {
		lines, sug := k.modelLines(), len(k.suggestions())
		k.ghAt("gh-agentsmd-changed", hAgentsMD)
		ev := k.settledPrefix(src, "repo-acc-org-acc-repo-agents-md-sha-b-")
		if ev.Type != "repo.agents_md_changed" || ev.Disposition != "ignored" || ev.Item != "" {
			t.Errorf("事件应 ignored: %+v", ev)
		}
		if !k.eventRule(ev, "agents-md-changed") {
			t.Error("应命中 agents-md-changed")
		}
		k.until("内核触发规范检测（agents-md trigger）", func() bool {
			return strings.Contains(k.ServiceLog(), "agents-md trigger repo=acc-org/acc-repo")
		})
		eq(t, "模型替身日志新增行数", k.modelLines()-lines, 0)
		eq(t, "建议流条数", len(k.suggestions()), sug)
	})
}
