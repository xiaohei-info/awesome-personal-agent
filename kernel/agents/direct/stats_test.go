package direct

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 导入的事件不进接受、丢弃统计（§6.7 "现场"读法）：同样的本人决定，带导入标记写入不计数。
func TestProposalStatsSkipImportedEvents(t *testing.T) {
	reg := store.NewRegistry()
	if err := reg.Register(eventlog.Module); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	payload := json.RawMessage(`{"before":{"phase":"proposed"},"item":{"phase":"open","kind":"task"}}`)
	decide := func(batch string) domain.Event {
		return domain.Event{Type: domain.EvItemPhaseChanged, Object: "itm_1", Payload: payload, ResolvedVia: decisions.ViaWeb, Imported: batch,
			Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, At: time.Now()}
	}
	if err := st.Update(context.Background(), func(tx *store.Tx) error {
		for _, b := range []string{"imp_1", "", "imp_1"} {
			if _, err := eventlog.Append(tx, decide(b)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got Stats
	if err := st.View(context.Background(), func(tx *store.Tx) (err error) {
		got, err = proposalStats(tx, Scope{}, time.Now(), time.Hour)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if got.Accepted != 1 {
		t.Fatalf("only the live decision counts, got %+v", got)
	}
}
