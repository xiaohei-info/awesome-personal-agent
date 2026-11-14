//go:build e2e

package decisions

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestFlowAgentRevert 对应 flow-agent-revert（ACT-REV-01..03）：本人看到 agent 做了什么，并只在 Web 撤销。
func TestFlowAgentRevert(t *testing.T) {
	f := setup(t)
	target := f.task("original-title", "acc-work")
	decided := f.task("needs-a-call", "acc-work")
	f.ask(decided)
	all := f.events("", "")
	cursor := all[len(all)-1].Seq // 上次访问：此后才是"新变更"
	var renameSeq int64

	t.Run("ACT-REV-01 查看 agent 变更视图", func(t *testing.T) {
		f.apa(f.DEL, 0, "item", "update", target, "--title", "renamed-by-agent", "--reason", "clearer title", "--evidence", "owner: rename it")
		f.apa(f.DEL, 0, "decide", decided, "--option", "A", "--evidence", "owner: yes, ship it")

		var mine []step
		for _, s := range f.agentChanges("") {
			if s.Seq > cursor {
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
			if s.Actor.Principal == "" || s.Actor.OnBehalfOf != "owner" || s.Ev == "" {
				t.Errorf("应带操作者与证据: %+v", *s)
			}
		}
		if len(mine) != 2 {
			t.Errorf("自上次访问以来应恰好两条 agent 变更，得 %d: %+v", len(mine), mine)
		}
		renameSeq = rename.Seq
	})

	t.Run("ACT-REV-02 一键撤销写反向命令", func(t *testing.T) {
		before := f.events(target, "")
		f.webOK("POST", "/items/"+target+"/revert", `{"event":`+strconv.FormatInt(renameSeq, 10)+`}`, 200)
		eq(t, "标题", f.item(target)["title"], "original-title")
		after := f.events(target, "")
		if len(after) != len(before)+1 {
			t.Fatalf("时间线应多恰好一条撤销 Event: %d → %d", len(before), len(after))
		}
		for i := range before { // 历史不被改写
			if !reflect.DeepEqual(before[i], after[i]) {
				t.Errorf("原 Event 被改写: %+v → %+v", before[i], after[i])
			}
		}
		rev := after[len(after)-1]
		if rev.Actor.Type != "human" || rev.Actor.Via != "web" || rev.Payload["op"] != "revert" || rev.Payload["reverts"] != float64(renameSeq) {
			t.Errorf("撤销 Event 应由本人经 web 写入并指向原 Event: %+v", rev)
		}
		// 浏览器渲染不在本测试内验证：这里只确认发布的前端产物含"外部副作用不会被撤销"提示（撤销确认里的"已发出的消息、已提的 PR 不会收回"）。
		hit := false
		assets, _ := filepath.Glob(filepath.Join("..", "..", "..", "web", "dist", "assets", "*.js"))
		for _, a := range assets {
			if b, _ := os.ReadFile(a); strings.Contains(string(b), "已发出的消息、已提的 PR 不会收回") {
				hit = true
			}
		}
		if !hit {
			t.Error("agent 变更视图缺少外部副作用不会被撤销的提示")
		}
	})

	t.Run("ACT-REV-03 撤销只在 Web", func(t *testing.T) {
		f.apa(f.DEL, 0, "item", "update", target, "--title", "second-title", "--reason", "again", "--evidence", "owner: again")
		last := f.last(target, "item.updated")
		changes := func() int { return len(f.events(target, "")) - len(f.events(target, "command.rejected")) }
		n := changes()
		for name, tok := range map[string]string{"DEL": f.DEL, "WRK": f.WRK, "OBS": f.OBS} {
			if r := f.APIRaw("POST", "/v1/items/"+target+"/revert", `{"event":`+strconv.FormatInt(last.Seq, 10)+`}`, tok); r.Code != 403 {
				t.Errorf("%s 请求撤销应被拒绝(403)，实际 %d %s", name, r.Code, r.Body)
			}
		}
		eq(t, "标题", f.item(target)["title"], "second-title")
		eq(t, "事项变更 Event 数", changes(), n)
	})
}
