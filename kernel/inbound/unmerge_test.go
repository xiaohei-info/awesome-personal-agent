package inbound

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// TestUnmergeRestoresFoldedProposalAndRejectsSecond：历史上被建议合并折叠的建议仍可撤销合并（回到建议页、搬回来源与留痕、重复撤销返回冲突）。
func TestUnmergeRestoresFoldedProposalAndRejectsSecond(t *testing.T) {
	e := newEnv(t, false)
	// 先建两条来源事件，规则入队后经模拟批处理建两条建议。
	e.emit("src_a", "ticket", ev("a1", "ticket.opened", "事件 A", nil))
	e.emit("src_b", "ticket", ev("b1", "ticket.opened", "事件 B", nil))
	evA := e.events("src_a")[0]
	evB := e.events("src_b")[0]
	// 模拟历史折叠：folded(evB.Item) 被 merge 折叠进 carrier(evA.Item)。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		carrier, err := items.Get(tx, evA.Item)
		if err != nil {
			return err
		}
		folded, err := items.Get(tx, evB.Item)
		if err != nil {
			return err
		}
		cp := *carrier.Proposal
		cp.Sources = append(cp.Sources, evB.ID)
		cp.Merges = append(cp.Merges, domain.ProposalMerge{Event: evB.ID, Source: "src_b", Title: folded.Title, By: "generate", Batch: "mb_hist", Item: folded.ID, At: e.now})
		cp.Absorbed = append(cp.Absorbed, domain.ProposalAbsorbed{Item: folded.ID, Title: folded.Title, Events: []string{evB.ID}, Action: domain.SweepMerge, Batch: "mb_hist", At: e.now})
		if _, err := items.SetProposal(tx, carrier.ID, cp, nil, e.now); err != nil {
			return err
		}
		if err := runner.Repoint(tx, evB.ID, folded.ID, carrier.ID); err != nil {
			return err
		}
		fp := *folded.Proposal
		now := e.now
		fp.Sweep = &domain.ProposalSweep{State: "done", Batch: "mb_hist", Action: domain.SweepMerge, Target: carrier.ID, By: "generate", At: &now}
		_, _, _, err = items.Revise(tx, folded.ID, "merge.fold", e.now, func(x *domain.Item) error {
			x.Phase, x.Proposal = domain.PhaseDropped, &fp
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := e.svc.Unmerge(bg, owner, evB.Item); err != nil {
		t.Fatalf("Unmerge: %v", err)
	}
	back := e.item(evB.Item)
	if back.Phase != domain.PhaseProposed || back.Proposal.Sweep.Action != domain.SweepUndone {
		t.Fatalf("unmerged item = %+v", back)
	}
	if got := e.events("src_b")[0].Item; got != evB.Item {
		t.Fatalf("repointed event = %q, want %q", got, evB.Item)
	}
	carrier := e.item(evA.Item)
	if len(carrier.Proposal.Sources) != 1 || carrier.Proposal.Sources[0] != evA.ID || len(carrier.Proposal.Merges) != 0 {
		t.Fatalf("carrier after unmerge = %+v", carrier.Proposal)
	}
	// 第二次撤销返回冲突。
	if err := e.svc.Unmerge(bg, owner, evB.Item); err == nil {
		t.Fatal("second Unmerge must fail with conflict")
	}
}

// TestSplitExtractsMergedSourceEventIntoNewProposal：把并入同一建议的第二条来源事件拆出来另成新建议。
func TestSplitExtractsMergedSourceEventIntoNewProposal(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_a", "ticket", ev("s1", "ticket.opened", "原事件", nil))
	e.emit("src_b", "other", ev("s2", "mail.received", "被并入事件", nil))
	evA := e.events("src_a")[0]
	evB := e.events("src_b")[0]
	if _, err := e.svc.ProposeInto(bg, owner, evB.ID, evA.Item); err != nil {
		t.Fatal(err)
	}
	if p := e.item(evA.Item).Proposal; p == nil || len(p.Sources) != 2 {
		t.Fatalf("before split: %+v", p)
	}
	// 拆分第二条事件。
	res, err := e.svc.Split(bg, owner, evB.ID, evA.Item)
	if err != nil || res == "" || res == evA.Item {
		t.Fatalf("Split = %q %v", res, err)
	}
	orig := e.item(evA.Item)
	splitItem := e.item(res)
	if len(orig.Proposal.Sources) != 1 || orig.Proposal.Sources[0] != evA.ID {
		t.Fatalf("orig after split = %+v", orig.Proposal)
	}
	if splitItem.Phase != domain.PhaseProposed || splitItem.Title != "被并入事件" || splitItem.Proposal.SplitFrom != evA.Item {
		t.Fatalf("split item = %+v", splitItem)
	}
	if got := e.events("src_b")[0].Item; got != res {
		t.Fatalf("evB should point to split item %q, got %q", res, got)
	}
}
