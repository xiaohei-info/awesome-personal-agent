package decisioncard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

func loadPack(t *testing.T) *kp.Pack {
	t.Helper()
	l := kp.New(kp.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	rej, err := l.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "decision-card" {
			t.Fatalf("decision-card rejected by loader: %s", r.Reason)
		}
	}
	p, ok := l.Get("decision-card")
	if !ok {
		t.Fatal("decision-card is not an active builtin pack")
	}
	return p
}

// flow-screening：C.14 声明的三个生成型提示词，输出 schema 带"信息不足"标记。
func TestGenerativePromptsCarryInsufficientInfoMarker(t *testing.T) {
	p := loadPack(t)
	for _, name := range []string{"decision-card-copy", "one-line-summary", "revision-summary"} {
		pr, ok := p.Prompts[name]
		if !ok {
			t.Fatalf("missing generate prompt %s", name)
		}
		if pr.Meta.Agent != "generate" || pr.Meta.OutputSchema == nil {
			t.Fatalf("%s must be generate with output_schema, got %+v", name, pr.Meta)
		}
		b, _ := json.Marshal(pr.Meta.OutputSchema)
		if !strings.Contains(string(b), "insufficient_info") {
			t.Errorf("%s output_schema lacks the insufficient_info marker: %s", name, b)
		}
	}
}

// flow-screening：投递阈值与 web_only 初筛是判断型，初筛不声明 event_stats（C.7、§8.4）。
func TestJudgePromptsAndScreeningHasNoEventStats(t *testing.T) {
	p := loadPack(t)
	dd, ok := p.Prompts["delivery-defer"]
	if !ok || dd.Meta.Agent != "judge" || dd.Meta.QuestionType != "yes_no" || !dd.Meta.SingleShot || dd.Meta.OnUndecided != "no" || dd.Meta.Band == nil {
		t.Fatalf("delivery-defer must be a single_shot yes_no judge with on_undecided no: %+v", dd.Meta)
	}
	if _, old := p.Prompts["delivery-threshold"]; old {
		t.Error("v0.1 delivery-threshold must be removed")
	}
	sc, ok := p.Prompts["web-only-screening"]
	if !ok {
		t.Fatal("missing web-only-screening prompt")
	}
	if sc.Meta.Agent != "judge" || sc.Meta.QuestionType != "yes_no" || !sc.Meta.SingleShot || sc.Meta.OnUndecided != "yes" || sc.Meta.Band == nil {
		t.Fatalf("web-only-screening must be a single_shot yes_no judge with on_undecided yes: %+v", sc.Meta)
	}
	if sc.Meta.EventStats != "" {
		t.Errorf("web-only-screening must not declare event_stats, got %q", sc.Meta.EventStats)
	}
}

// 包内不含 harness 名、不声明 auto、无规则（纯提示词包）。
func TestPackIsDeclarativeAndHarnessNeutral(t *testing.T) {
	p := loadPack(t)
	if len(p.ClockRules)+len(p.EventRules)+len(p.SourceRules) != 0 {
		t.Error("decision-card declares prompts only, no rules")
	}
	for name, pr := range p.Prompts {
		for _, h := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), h) {
				t.Errorf("prompt %s mentions harness/tool name %q", name, h)
			}
		}
		if pr.Meta.RunAs != "" {
			t.Errorf("prompt %s must not be a main_session prompt", name)
		}
	}
}
