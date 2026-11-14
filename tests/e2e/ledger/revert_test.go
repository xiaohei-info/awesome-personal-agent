//go:build e2e

package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFlowAgentRevert 按验收文档 flow-agent-revert 顺序执行 ACT-REV-01..03。
func TestFlowAgentRevert(t *testing.T) {
	e := setup(t)
	target := addTask(t, e, "original-title")
	decided := addTask(t, e, "needs-a-call", "--assignee", "work-host.cli")
	ok(t, e.Apa(e.WRK, "", "ask", decided, "--question", "ship it?", "--option", "yes=Yes", "--option", "no=No"), "ask")
	before := audit(t, e, "")           // 只取一次：异步事件可能在两次读取之间追加，分别取长度和元素会越界
	cursor := before[len(before)-1].Seq // 上次访问：此后才是"新变更"

	var renameSeq int64
	t.Run("ACT-REV-01 查看 agent 变更视图", func(t *testing.T) {
		ok(t, e.Apa(e.DEL, "", "item", "update", target, "--title", "renamed-by-agent", "--reason", "clearer title", "--evidence", "owner: rename it"), "改标题")
		ok(t, e.Apa(e.DEL, "", "decide", decided, "--option", "yes", "--evidence", "owner: yes, ship it"), "代答")

		// 视图读 GET /v1/audit/events，按 actor.type=agent 过滤；自上次访问以来 = seq 大于游标。
		var mine []step
		for _, s := range audit(t, e, "after="+itoa(cursor)) {
			if s.Actor.Type == "agent" {
				mine = append(mine, s)
			}
		}
		var rename, decide *step
		for i, s := range mine {
			switch {
			case s.Type == "item.updated" && s.Object == target:
				rename = &mine[i]
			case s.Type == "decision.resolved" && s.Object == decided:
				decide = &mine[i]
			}
		}
		if rename == nil || decide == nil {
			t.Fatalf("agent 变更视图缺记录（改标题 %v，代答 %v）: %+v", rename != nil, decide != nil, mine)
		}
		for _, s := range []*step{rename, decide} {
			if s.Actor.Principal == "" || s.Actor.OnBehalfOf != "owner" || s.Evidence == "" {
				t.Errorf("应带操作者与证据: %+v", *s)
			}
		}
		if n := len(mine); n != 2 {
			t.Errorf("自上次访问以来应恰好两条 agent 变更，得 %d: %+v", n, mine)
		}
		// 范围正确：游标之前的 agent 事件（建事项、ask）不在其中。
		for _, s := range mine {
			if s.Seq <= cursor {
				t.Errorf("游标之前的记录混入: %+v", s)
			}
		}
		renameSeq = rename.Seq
	})

	t.Run("ACT-REV-02 一键撤销写反向命令", func(t *testing.T) {
		before := audit(t, e, "object="+target)
		r := e.Web("POST", "/v1/items/"+target+"/revert", `{"event":`+itoa(renameSeq)+`}`)
		if r.Code != 200 {
			t.Fatalf("撤销: %d %s", r.Code, r.Body)
		}
		if title := show(t, e, target)["title"]; title != "original-title" {
			t.Errorf("标题应恢复，实际 %v", title)
		}
		after := audit(t, e, "object="+target)
		if len(after) != len(before)+1 {
			t.Fatalf("时间线应多恰好一条撤销 Event: %d → %d", len(before), len(after))
		}
		for i := range before { // 历史不被改写：原 Event 逐条仍在、内容不变
			if !sameJSON(toAny(before[i]), toAny(after[i])) {
				t.Errorf("原 Event 被改写: %+v → %+v", before[i], after[i])
			}
		}
		rev := after[len(after)-1]
		if rev.Actor.Type != "human" || rev.Actor.Via != "web" || rev.Payload["op"] != "revert" || rev.Payload["reverts"] != float64(renameSeq) {
			t.Errorf("撤销 Event 应由本人经 web 写入并指向原 Event: %+v", rev)
		}
		// 界面提示：Web 页面文案随前端构建产物发布，必须含"外部副作用不会被撤销"的提示（撤销确认里的"已发出的消息、已提的 PR 不会收回"）。
		hit := false
		assets, _ := filepath.Glob(filepath.Join("..", "..", "..", "web", "dist", "assets", "*.js"))
		for _, f := range assets {
			if b, _ := os.ReadFile(f); strings.Contains(string(b), "已发出的消息、已提的 PR 不会收回") {
				hit = true
			}
		}
		if !hit {
			t.Error("agent 变更视图缺少外部副作用不会被撤销的提示（浏览器渲染不在本测试内验证）")
		}
	})

	t.Run("ACT-REV-03 撤销只在 Web", func(t *testing.T) {
		ok(t, e.Apa(e.DEL, "", "item", "update", target, "--title", "second-title", "--reason", "again", "--evidence", "owner: again"), "再改一次")
		evs := audit(t, e, "object="+target)
		last := evs[len(evs)-1]
		for name, tok := range map[string]string{"DEL": e.DEL, "WRK": e.WRK, "OBS": e.OBS} {
			r := e.APIRaw("POST", "/v1/items/"+target+"/revert", `{"event":`+itoa(last.Seq)+`}`, tok)
			if r.Code != 403 {
				t.Errorf("%s 请求撤销应被拒绝(403)，实际 %d %s", name, r.Code, r.Body)
			}
		}
		if title := show(t, e, target)["title"]; title != "second-title" {
			t.Errorf("事项应不变，实际 %v", title)
		}
		// 被拒绝的命令只留 command.rejected（§15.3 被拒绝的命令同样可查），不产生任何事项变更 Event。
		after := audit(t, e, "object="+target)
		if len(after) < len(evs) || len(after)-len(ofType(after, "command.rejected")) != len(evs)-len(ofType(evs, "command.rejected")) {
			t.Errorf("被拒绝的撤销不应写事项变更 Event: %d → %d", len(evs), len(after))
		}
	})
}
