package sources

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 合并进别处的建议，带几条消息按去处上从它搬来的留痕数（含挂靠消息）；没合并的、撤销了的按它自己的 sources。
func TestCarried(t *testing.T) {
	folded := domain.Item{ID: "a", Proposal: &domain.Proposal{Sources: []string{"e1"}, Sweep: &domain.ProposalSweep{Action: domain.SweepMerge, Target: "k"}}}
	keep := domain.Item{ID: "k", Proposal: &domain.Proposal{Merges: []domain.ProposalMerge{{Event: "e1", Item: "a"}, {Event: "e2", Item: "a"}, {Event: "e3", Item: "a"}, {Event: "e4", Item: "b"}, {Event: "e5"}}}}
	if n := carried(folded, keep); n != 3 {
		t.Fatalf("merged: %d", n)
	}
	undone := folded
	undone.Proposal = &domain.Proposal{Sources: []string{"e1"}, Sweep: &domain.ProposalSweep{Action: domain.SweepUndone, Target: "k"}}
	if n := carried(undone, keep); n != 1 {
		t.Fatalf("undone: %d", n)
	}
	if n := carried(folded, domain.Item{ID: "k"}); n != 1 {
		t.Fatalf("target without proposal: %d", n)
	}
}
