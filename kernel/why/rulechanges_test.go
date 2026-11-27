package why

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

// 规矩改动清单（§14.3）：3a 的策略段与命令改动带前后值，能撤回的给出逆操作；之后又被改过的标 superseded，拉起运行的标 run_started。
func TestRuleChangesListAndReverse(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, policy.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	agent := domain.Actor{Type: domain.ActorAgent, Principal: "cred_a", Via: domain.ViaCLI}
	rules := &domain.Basis{Tier: "rules", Agent: "ag", Via: domain.BasisAllowance}
	at := time.Unix(1_800_000_000, 0)
	tog := func(on bool) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"id": "ins", "enabled": on})
		return b
	}
	var allowSeq, toggleSeq, runSeq int64
	if err := st.Update(ctx, func(tx *store.Tx) error {
		if _, err := policy.UpdateWith(tx, agent, rules, "t", func(p *policy.Policy) error {
			p.ForwardAllowlist = map[string][]string{"mail": {"a@example.invalid"}}
			return nil
		}); err != nil {
			return err
		}
		es, _ := eventlog.Query(tx, eventlog.Filter{Type: domain.EvPolicyChanged}, 0, 10)
		allowSeq = es[len(es)-1].Event.Seq
		cmd := func(c, typ string, payload json.RawMessage) int64 {
			b := *rules
			b.Command = c
			seq, err := eventlog.Append(tx, domain.Event{Type: typ, Object: "ins", Actor: agent, Basis: &b, Payload: payload, At: at})
			if err != nil {
				t.Fatal(err)
			}
			return seq
		}
		toggleSeq = cmd("instance.toggle", "instance.toggled", tog(true))
		runSeq = cmd("instance.run", "instance.fired", tog(true))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.Update(ctx, func(tx *store.Tx) error {
		list, err := RuleChanges(tx, "ag", 0, 0)
		if err != nil || len(list) != 3 {
			t.Fatalf("want 3 changes by ag, got %d %v", len(list), err)
		}
		if list[0].Seq != runSeq || list[0].Revert.Why != RevertRunStarted {
			t.Fatalf("run now cannot be undone: %+v", list[0])
		}
		_, rv, err := ReverseOf(tx, toggleSeq)
		if err != nil || rv == nil || rv.Command != "instance.toggle" || string(rv.Input) != `{"enabled":false,"id":"ins"}` {
			t.Fatalf("toggle reversal: %+v %v", rv, err)
		}
		rc, rv, err := ReverseOf(tx, allowSeq)
		if err != nil || rv == nil || len(rv.Sections) != 1 || rv.Sections[0] != "forward_allowlist" || string(rc.Before) != `{"forward_allowlist":{}}` {
			t.Fatalf("policy reversal: %+v %+v %v", rc, rv, err)
		}
		from, err := policy.At(tx, rv.From)
		if err != nil {
			return err
		}
		if _, err := policy.Update(tx, agent, "revert", func(p *policy.Policy) error { return policy.Restore(p, from, rv.Sections) }); err != nil {
			return err
		}
		_, p, err := policy.Stored(tx)
		if len(p.ForwardAllowlist) != 0 {
			t.Fatalf("allowlist not restored: %v", p.ForwardAllowlist)
		}
		// 之后又改过：再撤回就会盖掉后来的改动。
		if _, err := eventlog.Append(tx, domain.Event{Type: "instance.toggled", Object: "ins", Actor: agent, Payload: tog(false), At: at}); err != nil {
			return err
		}
		rc, rv, err = ReverseOf(tx, toggleSeq)
		if err != nil || rv != nil || rc.Revert.Why != RevertSuperseded {
			t.Fatalf("superseded toggle: %+v %+v %v", rc, rv, err)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
