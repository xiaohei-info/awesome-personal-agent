package executors

import (
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 执行者统一切换：旧卡成为同 id 的隔离声明与一个执行者，拉起命令与切换前一致；被选用的兼容卡成为同名执行者。
func TestUnifyKeepsTheCardsLaunchCommand(t *testing.T) {
	e := setup(t)
	d := decl("hermes")
	d.Headless.Effort = []string{"--reasoning", "{effort}"}
	d.InstanceOptions = &InstanceOptions{Fields: map[string]OptionField{"model": {Label: Label{"模型", "Model"}, Type: "string", Pattern: "[a-z0-9-]+", DisplayAs: "model"}},
		Args: map[string][]string{"model": {"--model", "{model}"}}}
	e.must(t, owner, "executor.register", d)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM executors_entry`) // 切换前的账本：程序声明下没有执行者
		if err == nil {
			_, err = tx.Exec(`INSERT INTO executors_agent (id,harness,name,options,enabled,rev) VALUES
				('agent_D','hermes','hermes-daily','{"model":"claude-haiku-5-5"}',1,4), ('agent_off','hermes','old','{}',0,1)`)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var u Unified
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { u, err = Unify(tx, []string{"legacy:hermes"}); return }); err != nil {
		t.Fatal(err)
	}
	if u.Map["agent_D"] != "hermes-daily" || u.Map["legacy:hermes"] != "hermes" || u.Skipped["agent_off"] == "" || !slices.Equal(u.Declarations, []string{"agent_D"}) {
		t.Fatalf("%+v", u)
	}
	pol := policy.Default()
	pol.BackgroundPersona = true
	pol.AgentCards = map[string]policy.Card{"agent_D": {Tool: "edit"}}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		tg, err := Resolve(tx, pol, Want{Executor: "hermes-daily", Runner: RunnerSourceBatch})
		if err != nil || tg.Reason != "" {
			t.Fatalf("%v %+v", err, tg)
		}
		b, err := tg.Build(pol, Spec{RunID: "r", RunDir: "/d", Prompt: "P"})
		if err != nil {
			return err
		}
		// 切换前：命令模板 + 卡的 --model + 思考强度 + 工具档。
		want := []string{"acme", "run", "P", "--dir", "/d", "--model", "claude-haiku-5-5", "--reasoning", "medium", "--rw"}
		if !slices.Equal(b.Command, want) || tg.Program() != "hermes" || b.Snapshot.AgentID != "agent_D" || b.Snapshot.ConfiguredModel != "claude-haiku-5-5" ||
			b.Snapshot.DeclarationRev != tg.Record.Rev {
			t.Fatalf("%v %+v", b.Command, b.Snapshot)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
