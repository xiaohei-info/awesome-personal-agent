package inbound

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 已取消的批量归类留下的"待归类"建议在启动时放回普通建议；再跑一次什么都不做。
func TestReleasePendingProposals(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_a", "ticket", ev("p1", "ticket.opened", "待归类的事", nil))
	id := e.events("src_a")[0].Item
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := items.Revise(tx, id, "update", e.now, func(x *domain.Item) error {
			p := *x.Proposal
			p.Sweep = &domain.ProposalSweep{State: "pending"}
			x.Proposal = &p
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1, 0} {
		if n, err := e.svc.ReleasePending(bg, e.x); err != nil || n != want {
			t.Fatalf("ReleasePending = %d %v, want %d", n, err, want)
		}
	}
	if it := e.item(id); it.Phase != domain.PhaseProposed || it.Proposal.Sweep != nil || len(it.Proposal.Sources) != 1 {
		t.Fatalf("released proposal = %+v %+v", it, it.Proposal)
	}
}
