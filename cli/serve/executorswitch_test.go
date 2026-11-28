package serve

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 执行者统一切换（§16.5）：线上形态的账本（程序声明 hermes、一张带模型的卡、两张不同的权限卡、按卡的分工与拉起方式、
// 绑卡的钥匙）切换后，选人都是执行者，权限卡、钥匙绑定的判定与拉起命令不变；只做一次。
func TestExecutorSwitchKeepsBehaviour(t *testing.T) {
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, now := context.Background(), time.Now()
	decl := `{"harness":"hermes","name":{"zh":"Hermes","en":"Hermes"},"headless":{"command":["hermes","chat","-q","{prompt}"],"session":{"extract":"$.session_id"},` +
		`"access":{"read":["-r"],"edit":["-e"],"full":["-f"]},"effort":["--reasoning","{effort}"]},"main_session":{"wake":true},` +
		`"instance_options":{"fields":{"model":{"label":{"zh":"模型","en":"Model"},"type":"string","pattern":"[a-z0-9-]+","display_as":"model"}},"args_if_set":{"model":["--model","{model}"]}}}`
	var bound auth.Credential
	if err := st.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO executors_declaration (harness, rev, doc, state) VALUES ('hermes', 8, ?, 'active')`, decl); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO executors_agent (id,harness,name,options,enabled,rev) VALUES ('agent_Z','hermes','hermes-daily','{"model":"claude-haiku-5-5"}',1,4)`); err != nil {
			return err
		}
		var err error
		if bound, _, err = auth.New(nil).Issue(tx, auth.Spec{Kind: auth.Delegate, Label: "daily", Harness: "hermes", AgentID: "agent_Z"}); err != nil {
			return err
		}
		doc, _ := policy.Sections(policy.Default())
		for k, v := range map[string]string{
			"agent_cards":         `{"agent_Z":{"groups":{"view":{},"organize":{}},"tool":"edit"},"legacy:hermes":{"groups":{"view":{},"decide":{}},"tool":"full"}}`,
			"default_agent_id":    `"agent_Z"`,
			"main_agent_id":       `"legacy:hermes"`,
			"default_harness":     `"hermes"`,
			"rule_agent":          `{"guideline-draft/clock_rules/guideline-check":"agent_Z"}`,
			"main_session_launch": `{"by_agent":{"agent_Z":"headless"},"by_harness":{"hermes":"wake"},"by_rule":{}}`,
			"source_pipeline":     `{"agent":"agent_Z"}`,
			"background_persona":  `true`,
		} {
			doc[k] = json.RawMessage(v)
		}
		raw, _ := json.Marshal(doc)
		if _, err := tx.Exec(`INSERT INTO policy_version (version, doc, at) VALUES (1, ?, ?)`, string(raw), now.UnixNano()); err != nil {
			return err
		}
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvAuthzSwitched, Object: policy.SwitchObject, Actor: authzSwitchActor,
			Payload: json.RawMessage(`{"model":"cards"}`), At: now})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := switchExecutors(ctx, st, clock.Now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.View(ctx, func(tx *store.Tx) error {
		es, _ := eventlog.Query(tx, eventlog.Filter{Type: domain.EvAuthzSwitched}, 0, 10)
		if len(es) != 2 {
			t.Fatalf("switch ran %d times", len(es)-1)
		}
		_, p, err := policy.Stored(tx)
		if err != nil {
			return err
		}
		if p.DefaultExecutor != "hermes-daily" || p.MainExecutor != "hermes" || p.SourcePipeline.Executor != "hermes-daily" ||
			p.RuleExecutor["guideline-draft/clock_rules/guideline-check"] != "hermes-daily" || p.DefaultAgentID != nil || p.MainAgentID != nil ||
			p.DefaultHarness != "" || len(p.RuleAgent) != 0 || p.SourcePipeline.Agent != "" || len(p.MainSessionLaunch.ByAgent) != 0 ||
			p.MainSessionLaunch.ByHarness["agent_Z"] != "headless" || p.MainSessionLaunch.ByHarness["hermes"] != "wake" {
			t.Fatalf("policy: %+v", p)
		}
		if _, old := p.AgentCards["legacy:hermes"]; old || p.Card("hermes").Tool != "full" || p.Card("agent_Z").Tool != "edit" {
			t.Fatalf("cards: %+v", p.AgentCards)
		}
		// 绑卡的钥匙仍按那张卡判；没绑的 hermes 钥匙按程序的卡判。
		if a := command.CallerOf(bound, auth.Origin{}).Agent(); a != "agent_Z" {
			t.Fatalf("bound key agent = %s", a)
		}
		daily, err := executors.Resolve(tx, p, executors.Want{Executor: "hermes-daily", Runner: executors.RunnerSourceBatch})
		if err != nil || daily.Reason != "" {
			t.Fatalf("%v %+v", err, daily)
		}
		b, err := daily.Build(p, executors.Spec{RunID: "r", Prompt: "P"})
		if err != nil {
			return err
		}
		if want := []string{"hermes", "chat", "-q", "P", "--model", "claude-haiku-5-5", "--reasoning", "medium", "-e"}; !slices.Equal(b.Command, want) ||
			daily.Program() != "hermes" || b.Snapshot.DeclarationRev != 8 {
			t.Fatalf("daily command %v rev %d", b.Command, b.Snapshot.DeclarationRev)
		}
		main, err := executors.Resolve(tx, p, executors.Want{Executor: "hermes", Runner: executors.RunnerMainSession})
		if err != nil || main.Reason != "" || main.Access != "full" {
			t.Fatalf("%v %+v", err, main)
		}
		if route, why, err := executors.MainWake(tx, p); err != nil || why != "" || route != "" {
			t.Fatalf("main wake %q %q %v", route, why, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
