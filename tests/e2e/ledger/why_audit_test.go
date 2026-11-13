//go:build e2e

package ledger

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const secretEvidence = "owner: pick option x because the vendor is cheaper"

// decidedItem 造一个经历创建、决策请求、回答、转阶段的事项。
func decidedItem(t *testing.T, e *baseline.Env, title string) string {
	t.Helper()
	id := addTask(t, e, title, "--assignee", "work-host.cli")
	ok(t, e.Apa(e.WRK, "", "ask", id, "--question", "which vendor?", "--option", "x=Vendor X", "--option", "y=Vendor Y", "--reason", "need the owner's call"), "ask")
	// web_only 初筛是异步的：返回前代答被拒（退出码 22，§6.14 第 4 条），等它返回后重试。
	var r baseline.Result
	waitFor(t, 30*time.Second, "初筛返回后可代答", func() bool {
		r = e.Apa(e.DEL, "", "decide", id, "--option", "x", "--evidence", secretEvidence)
		return r.Code != 22
	})
	ok(t, r, "decide")
	ok(t, e.Apa(e.DEL, "", "item", "transition", id, "--to", "done", "--reason", "vendor chosen", "--evidence", "owner: done"), "done")
	return id
}

// TestFlowWhyAudit 按验收文档 flow-why-audit 顺序执行 ACT-WHY-01..03。
func TestFlowWhyAudit(t *testing.T) {
	e := setup(t)
	id := decidedItem(t, e, "why-item")

	t.Run("ACT-WHY-01 回放一个事项", func(t *testing.T) {
		_, steps := why(t, e, e.DEL, id)
		var types []string
		for _, s := range steps {
			types = append(types, s.Type)
			if s.Actor.Type == "" || s.Actor.Principal == "" || s.Actor.Via == "" {
				t.Errorf("每一步都应有操作者: %+v", s)
			}
		}
		// 业务步骤按序出现；内核自己的后台运行（决策卡片摘要等）也在同一条时间线上，不要求缺席。
		want := []string{"item.created", "decision.requested", "decision.resolved", "item.phase_changed"}
		idx := map[string]int{}
		for i, ty := range types {
			if _, seen := idx[ty]; !seen {
				idx[ty] = i
			}
		}
		for i, w := range want {
			if _, found := idx[w]; !found || (i > 0 && idx[w] <= idx[want[i-1]]) {
				t.Fatalf("步骤顺序 = %v，期望依次包含 %v", types, want)
			}
		}
		for i := 1; i < len(steps); i++ {
			if steps[i].Seq <= steps[i-1].Seq {
				t.Errorf("seq 应严格递增: %d 后是 %d", steps[i-1].Seq, steps[i].Seq)
			}
		}
		if steps[idx["decision.requested"]].Reason != "need the owner's call" || steps[idx["item.phase_changed"]].Reason != "vendor chosen" || steps[idx["decision.resolved"]].Evidence != secretEvidence {
			t.Errorf("原因或证据缺失: %+v", steps)
		}
		// Web 事项详情的时间线与 apa why 同源且一致。
		if web := webSteps(t, e, id); !sameJSON(toAny(web), toAny(steps)) {
			t.Errorf("Web 时间线与 apa why 不一致:\nweb=%+v\ncli=%+v", web, steps)
		}
		// --pretty：每步一行信封（操作者），原因和证据缩进其后，顺序与 JSON 相同。
		pretty, _ := why(t, e, e.DEL, id, "--pretty")
		last := -1
		for _, s := range steps {
			at := strings.Index(pretty, "#"+itoa(s.Seq)+" ")
			if at <= last || !strings.Contains(pretty, s.Type) {
				t.Fatalf("--pretty 缺步骤或乱序: %s\n%s", s.Type, pretty)
			}
			last = at
		}
		for _, w := range []string{"reason: vendor chosen", "evidence: " + secretEvidence, "agent/"} {
			if !strings.Contains(pretty, w) {
				t.Errorf("--pretty 缺 %q:\n%s", w, pretty)
			}
		}
	})

	t.Run("ACT-WHY-02 按权限裁剪", func(t *testing.T) {
		_, full := why(t, e, e.DEL, id)
		raw, cut := why(t, e, e.OBS, id)
		if len(cut) != len(full) {
			t.Fatalf("结构应完整: OBS %d 步，DEL %d 步", len(cut), len(full))
		}
		for i := range cut {
			if cut[i].Seq != full[i].Seq || cut[i].Type != full[i].Type || cut[i].Actor != full[i].Actor {
				t.Errorf("第 %d 步信封应与完整视图一致: %+v vs %+v", i, cut[i], full[i])
			}
			if cut[i].Evidence != "" {
				t.Errorf("observer 不应看到证据: %+v", cut[i])
			}
		}
		if strings.Contains(raw, secretEvidence) || strings.Contains(raw, "vendor chosen") {
			t.Errorf("observer 看到了本人原话或原因:\n%s", raw)
		}
		if r := e.Apa(e.DEL, "", "why", id); !strings.Contains(r.Stdout, secretEvidence) {
			t.Error("对照组 delegate 应能看到证据，否则裁剪断言没有意义")
		}
	})

	t.Run("ACT-WHY-03 审计页筛选全量 Event", func(t *testing.T) {
		other := decidedItem(t, e, "why-other")
		all := audit(t, e, "")
		if len(all) == 0 {
			t.Fatal("审计应返回全量 Event")
		}
		// 按操作者：本人经 web 的 Event，不含 agent 的。
		own := audit(t, e, "actor=owner")
		if len(own) == 0 {
			t.Fatal("按操作者 owner 应有结果")
		}
		for _, s := range own {
			if s.Actor.Principal != "owner" {
				t.Errorf("actor 筛选混入了 %+v", s)
			}
		}
		if n := countWhere(all, func(s step) bool { return s.Actor.Principal == "owner" }); n != len(own) {
			t.Errorf("owner 的 Event 应 %d 条，筛选得 %d", n, len(own))
		}
		// 按对象。
		byObj := audit(t, e, "object="+id)
		if _, tl := why(t, e, e.DEL, id); len(byObj) != len(tl) {
			t.Errorf("事项 %s 的 Event 应与 apa why 同为 %d 条，得 %d", id, len(tl), len(byObj))
		}
		for _, s := range byObj {
			if s.Object != id {
				t.Errorf("object 筛选混入 %+v", s)
			}
		}
		// 按事件类型。
		byType := audit(t, e, "type=decision.resolved")
		if len(byType) != 2 {
			t.Errorf("两个事项各一条 decision.resolved，得 %d", len(byType))
		}
		for _, s := range byType {
			if s.Type != "decision.resolved" || (s.Object != id && s.Object != other) {
				t.Errorf("type 筛选不对: %+v", s)
			}
		}
		// 只读：审计路由不接受任何写方法。
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			if r := e.Web(m, "/v1/audit/events", `{}`); r.Code < 400 {
				t.Errorf("审计页不应有修改入口: %s → %d", m, r.Code)
			}
		}
		if n := len(audit(t, e, "")); n != len(all) {
			t.Errorf("写探测不应改变账本: %d → %d", len(all), n)
		}
	})
}

func countWhere(ss []step, f func(step) bool) (n int) {
	for _, s := range ss {
		if f(s) {
			n++
		}
	}
	return
}
