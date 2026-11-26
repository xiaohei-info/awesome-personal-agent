package serve

import (
	"context"
	"encoding/json"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
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

// 权限卡切换（§16.5）：已做过第二版切换的账本只做换算——兼容卡与名录里的 Agent 按映射表写卡（没设过档位的只整理），
// 工具档位原值搬入，旧段去掉，不再撤授权单；只做一次，没有任何一组被自动放开。
func TestCardsSwitchMapsTiersOnce(t *testing.T) {
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	if err := st.Update(ctx, func(tx *store.Tx) error {
		doc, _ := policy.Sections(policy.Default())
		delete(doc, "agent_cards")
		doc["agent_tier"] = json.RawMessage(`{"by_agent":{"agent_a":{"tier":"organize"},"legacy:hermes":{"tier":"rules"}}}`)
		doc["permission_cap"] = json.RawMessage(`{"by_agent":{"agent_a":"edit"},"default":"read","by_harness":{"hermes":"full"}}`)
		legacy, _ := json.Marshal(doc)
		if _, err := tx.Exec(`INSERT INTO policy_version (version, doc, at) VALUES (1, ?, ?)`, string(legacy), now.UnixNano()); err != nil {
			return err
		}
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvAuthzSwitched, Object: policy.SwitchObject, Actor: authzSwitchActor, Payload: json.RawMessage(`{}`), At: now})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := switchAuthz(ctx, st, clock.Now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.View(ctx, func(tx *store.Tx) error {
		es, _ := eventlog.Query(tx, eventlog.Filter{Type: domain.EvAuthzSwitched}, 0, 10)
		left, _ := policy.LegacyKeys(tx)
		_, p, err := policy.Stored(tx)
		a, h := p.Card("agent_a"), p.Card("legacy:hermes")
		if len(es) != 2 || len(left) != 0 || a.Tool != policy.PermEdit || a.Allows(policy.GroupDecide, now) || !a.Allows(policy.GroupOrganize, now) ||
			h.Tool != policy.PermFull || !h.Allows(policy.GroupRules, now) || h.Allows(policy.GroupConnect, now) || h.RunsAllMessages {
			t.Fatalf("switch: events %d legacy %v cards %+v", len(es), left, p.AgentCards)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 预演（§16.5）：在临时数据目录上，换算出的权限卡等于映射表（含没设过档位的 Agent），最近的命令按卡重判，不写账本。
func TestPreviewShowsMappedCardsAndChangedOutcomes(t *testing.T) {
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
	agent := domain.Actor{Type: domain.ActorAgent, Via: domain.ViaCLI}
	if err := st.Update(ctx, func(tx *store.Tx) error {
		key, _, err := auth.New(nil).Issue(tx, auth.Spec{Kind: auth.Delegate, Harness: "hermes"}) // 没绑 Agent 的宿主钥匙
		if err != nil {
			return err
		}
		agent.Principal = key.ID
		run, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{AgentSnapshot: &domain.AgentSnapshot{AgentID: "agent_r", ConfigRev: 1},
			Category: domain.RunBackground, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, now)
		if err != nil {
			return err
		}
		// 旧事件没记依据、发起者是运行凭据：按运行记录归到 agent_r（拍板开着），代答照旧生效。
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvDecisionResolved, Object: "itm_5", At: now, Payload: json.RawMessage(`{}`),
			Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run:" + run.ID, Via: domain.ViaCLI}}); err != nil {
			return err
		}
		doc, _ := policy.Sections(policy.Default())
		delete(doc, "agent_cards")
		doc["agent_tier"] = json.RawMessage(`{"by_agent":{"agent_a":{"tier":"organize"},"agent_r":{"tier":"decide"},"legacy:hermes":{"tier":"rules"}}}`)
		doc["permission_cap"] = json.RawMessage(`{"by_agent":{},"default":"read","by_harness":{"hermes":"full"}}`)
		raw, _ := json.Marshal(doc)
		if _, err := tx.Exec(`INSERT INTO policy_version (version, doc, at) VALUES (1, ?, ?)`, string(raw), now.UnixNano()); err != nil {
			return err
		}
		for _, ev := range []domain.Event{
			{Type: domain.EvDecisionResolved, Object: "itm_1", Basis: &domain.Basis{Agent: "legacy:hermes"}},
			// 旧事件没记依据：按发起的钥匙归到 legacy:hermes，接受建议照旧生效。
			{Type: domain.EvItemPhaseChanged, Object: "itm_4", Payload: json.RawMessage(`{"before":{"phase":"proposed"}}`)},
			{Type: domain.EvCommandRejected, Object: "itm_2", Payload: json.RawMessage(`{"action":"answer_decision","tier":"decide","code":"tier_closed","agent":"legacy:hermes"}`)},
			{Type: domain.EvCommandRejected, Object: "source.register", Payload: json.RawMessage(`{"action":"manage_source","tier":"power","code":"web_only","agent":"legacy:hermes"}`)},
			{Type: domain.EvCommandRejected, Object: "itm_3", Payload: json.RawMessage(`{"action":"change_settings","tier":"rules","code":"tier_closed","agent":"agent_a"}`)},
		} {
			ev.Actor, ev.At = agent, now
			if ev.Payload == nil {
				ev.Payload = json.RawMessage(`{}`)
			}
			if _, err := eventlog.Append(tx, ev); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pv, err := RunPreview(ctx, home, 30, now)
	if err != nil {
		t.Fatal(err)
	}
	h, a := pv.Plan.Cards["legacy:hermes"], pv.Plan.Cards["agent_a"]
	if pv.Switched || h.Tool != policy.PermFull || !h.Allows(policy.GroupRules, now) || h.Allows(policy.GroupConnect, now) ||
		a.Tool != policy.PermRead || a.Allows(policy.GroupDecide, now) || !a.Allows(policy.GroupSourceEvents, now) {
		t.Fatalf("cards: %+v", pv.Plan.Cards)
	}
	if pv.Rejudge["unchanged"] != 5 || pv.Rejudge["now_allowed"] != 1 || len(pv.Details) != 1 || pv.Details[0].Command != "rejected:answer_decision" {
		t.Fatalf("rejudge: %v %+v", pv.Rejudge, pv.Details)
	}
	if err := st.View(ctx, func(tx *store.Tx) error {
		done, err := cardsSwitched(tx)
		if done {
			t.Fatal("preview must not switch")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
