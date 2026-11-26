package serve

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// 账本副本上留着已取消的批量归类的"待归类"建议：启动后一条不剩，建议本身还在（§16.5）。
func TestStartReleasesPendingProposals(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	latest, err := ledgerfixture.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledgerfixture.Generate(home, latest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen: 127.0.0.1:"+strconv.Itoa(freePort(t))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	proposed := func(seed bool) (total, pending int) {
		t.Helper()
		st, err := store.Open(home, reg)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.Update(context.Background(), func(tx *store.Tx) error {
			all, err := items.List(tx, items.Filter{})
			if err != nil {
				return err
			}
			for _, it := range all {
				if it.Phase != domain.PhaseProposed {
					continue
				}
				total++
				if seed {
					if _, _, _, err := items.Revise(tx, it.ID, "update", clock.Now(), func(x *domain.Item) error {
						p := domain.Proposal{}
						if x.Proposal != nil {
							p = *x.Proposal
						}
						p.Sweep = &domain.ProposalSweep{State: "pending"}
						x.Proposal = &p
						return nil
					}); err != nil {
						return err
					}
					pending++
				} else if it.Proposal != nil && it.Proposal.Sweep != nil && it.Proposal.Sweep.State == "pending" {
					pending++
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	before, seeded := proposed(true)
	if seeded == 0 {
		t.Fatal("fixture has no proposals to seed")
	}
	_, stop := start(t, home)
	stop()
	if total, pending := proposed(false); pending != 0 || total != before {
		t.Fatalf("after start: %d proposals (%d before), %d still pending", total, before, pending)
	}
}
