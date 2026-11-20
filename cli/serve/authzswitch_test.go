package serve

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 切换（§16.5）：预演不写账本；切换撤销有效授权单、去掉旧策略段、清掉问题上的超时默认，且只做一次。
func TestAuthzSwitchIsPreviewableAndRunsOnce(t *testing.T) {
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	deadline := now.Add(time.Hour)
	var item string
	if err := st.Update(ctx, func(tx *store.Tx) error {
		doc, _ := policy.Sections(policy.Default())
		doc["web_only"] = json.RawMessage(`{"categories":["funds"],"rules":[]}`)
		doc["screening_timeout_ms"] = json.RawMessage(`30000`)
		doc["material_write"] = json.RawMessage(`{"default":"proposed","kinds":{}}`)
		legacy, _ := json.Marshal(doc)
		if _, err := tx.Exec(`INSERT INTO policy_version (version, doc, at) VALUES (1, ?, ?)`, string(legacy), now.UnixNano()); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO policy_grant (id, credential, doc, expires_at, created_at) VALUES ('grt_1', 'cred_1', ?, 0, ?)`,
			`{"id":"grt_1","principal":"owner","grantee":{"agent":"hermes","background":true},"scope":{"all":true},"actions":["decision.answer"],"risk_ceiling":"normal"}`, now.UnixNano()); err != nil {
			return err
		}
		it, _, err := kitems.Create(tx, domain.Item{Kind: "task", Title: "t", Phase: domain.PhaseNeedsInput, Ask: &domain.Ask{AskInput: domain.AskInput{
			Question: "go?", Options: []domain.AskOption{{ID: "a", Label: "yes"}, {ID: "b", Label: "no"}}, Deadline: &deadline, DefaultOnTimeout: "a"}}}, "", now)
		item = it.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	head := func() (h int64) {
		_ = st.View(ctx, func(tx *store.Tx) (err error) { h, err = eventlog.Head(tx); return })
		return
	}
	before := head()
	pv, err := RunPreview(ctx, home, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	if pv.Switched || len(pv.Plan.Grants) != 1 || len(pv.Plan.Asks) != 1 || len(pv.Plan.LegacyPolicy) != 3 || head() != before {
		t.Fatalf("preview: %+v (head %d → %d)", pv.Plan, before, head())
	}
	for range 2 {
		if err := switchAuthz(ctx, st, clock.Now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.View(ctx, func(tx *store.Tx) error {
		es, _ := eventlog.Query(tx, eventlog.Filter{Type: domain.EvAuthzSwitched}, 0, 10)
		gs, _ := policy.ListGrants(tx)
		left, _ := policy.LegacyKeys(tx)
		it, err := kitems.Get(tx, item)
		if len(es) != 1 || len(gs) != 1 || gs[0].RevokedAt.IsZero() || len(left) != 0 || it.Ask.DefaultOnTimeout != "" {
			t.Fatalf("switch: events %d, grant revoked %v, legacy %v, default %q", len(es), !gs[0].RevokedAt.IsZero(), left, it.Ask.DefaultOnTimeout)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
