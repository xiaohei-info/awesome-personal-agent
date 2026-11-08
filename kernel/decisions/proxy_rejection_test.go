package decisions

import (
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 第二档依据不足（退出码 23）：按事项所处阶段处理，拒绝与转换同一事务（§14.5）。
func TestBasisRequiredTurnsOpenAndWaitingItemsToNeedsInput(t *testing.T) {
	e := setup(t)
	open := e.item(t, map[string]any{})
	waiting := e.item(t, map[string]any{"phase": "waiting", "waiting_on": map[string]any{"kind": "human", "ref": "bob"}})
	for _, id := range []string{open, waiting} {
		before := e.get(t, id)
		if _, err := e.do(delegate, "proposal.accept", "", map[string]any{"id": id}); exit(err) != domain.ExitBasisRequired {
			t.Fatalf("%s: want 23, got %v", id, err)
		}
		it := e.get(t, id)
		if it.Phase != domain.PhaseNeedsInput || it.WaitingOn != nil || it.Ask == nil {
			t.Fatalf("%s not turned to needs_input: %+v", id, it)
		}
		a := it.Ask
		if a.RiskClass != domain.RiskClassWebOnly || a.ScreeningPending || len(a.Options) != 1 || a.Options[0].Label != "继续" || a.Options[0].NextPhase() != domain.PhaseOpen {
			t.Fatalf("kernel ask shape: %+v", a)
		}
		if !strings.Contains(a.Question, "dlg") || !strings.Contains(a.Question, "proposal.accept") {
			t.Fatalf("ask must name operator and command: %q", a.Question)
		}
		if it.Version <= before.Version {
			t.Fatalf("version not bumped: %d -> %d", before.Version, it.Version)
		}
		rej, req := e.last(t, id, domain.EvCommandRejected), e.last(t, id, domain.EvDecisionRequested)
		if rej.Seq >= req.Seq || req.Actor.Type != domain.ActorSystem || rej.Actor.Principal != "dlg" {
			t.Fatalf("event order/actors: %+v %+v", rej, req)
		}
	}
	e.verify(t) // 投影与事件一致：折叠能重建转换后的事项
}

func TestKernelAskIsWebOnlyAndAnswerReturnsToOpen(t *testing.T) {
	e := setup(t)
	id := e.item(t, map[string]any{})
	_, _ = e.do(delegate, "proposal.accept", "", map[string]any{"id": id})
	// 代答（带原话）被 web_only 挡下：20，事项仍待本人。
	if _, err := e.do(delegate, "decision.answer", "owner: go", map[string]any{"id": id, "choice": "continue"}); exit(err) != domain.ExitWebOnly {
		t.Fatalf("delegate answer on kernel ask: %v", err)
	}
	if e.get(t, id).Phase != domain.PhaseNeedsInput {
		t.Fatal("item left needs_input")
	}
	e.must(t, owner, "decision.answer", "", map[string]any{"id": id, "choice": "continue"})
	it := e.get(t, id)
	if it.Phase != domain.PhaseOpen || it.Ask.Resolution == nil || it.Ask.Resolution.ResolvedVia != ViaWeb {
		t.Fatalf("after web answer: %+v", it)
	}
	e.verify(t)
}

func TestBasisRequiredLeavesOtherObjectsUnchanged(t *testing.T) {
	e := setup(t)
	asked := e.asked(t, nil)
	proposed := e.item(t, map[string]any{"phase": "proposed"})
	done := e.item(t, map[string]any{})
	e.must(t, owner, "item.transition", "", map[string]any{"id": done, "to": "done", "reason": "x"})
	for _, tc := range []struct {
		id, cmd string
		in      map[string]any
		phase   domain.Phase
	}{
		{asked, "decision.answer", map[string]any{"choice": "A"}, domain.PhaseNeedsInput},
		{proposed, "proposal.accept", nil, domain.PhaseProposed},
		{done, "proposal.accept", nil, domain.PhaseDone},
	} {
		in := map[string]any{"id": tc.id}
		for k, v := range tc.in {
			in[k] = v
		}
		before := e.get(t, tc.id)
		nEv := len(e.events(t, tc.id))
		if _, err := e.do(delegate, tc.cmd, "", in); exit(err) != domain.ExitBasisRequired {
			t.Fatalf("%s: want 23, got %v", tc.id, err)
		}
		after := e.get(t, tc.id)
		if after.Phase != tc.phase || after.Version != before.Version || (before.Ask == nil) != (after.Ask == nil) {
			t.Fatalf("%s changed: %+v -> %+v", tc.id, before, after)
		}
		// 只多一条 command.rejected：不产生 ask、不产生别的事件。
		if es := e.events(t, tc.id); len(es) != nEv+1 || es[len(es)-1].Type != domain.EvCommandRejected {
			t.Fatalf("%s events: %+v", tc.id, es[nEv:])
		}
	}
	// 不对应事项的命令（项目）：只拒绝和记录。
	if _, err := e.do(delegate, "project.create", "", map[string]any{"name": "p1"}); exit(err) != domain.ExitBasisRequired {
		t.Fatalf("project.create: %v", err)
	}
	e.verify(t)
}
