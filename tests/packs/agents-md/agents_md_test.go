package agentsmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

// flow-agents-md / ACT-AGM-02：agents-md 包经内置 embed 加载，templates/agents.md
// 按 C.9 / §12.5 的顺序恰好使用五个占位符；包不含任何规则、提示词或 agent。
func TestAgentsMDPackShipsRenderTemplateWithFivePlaceholdersInOrder(t *testing.T) {
	ld := kp.New(kp.Options{Builtin: rootpacks.FS, DataDir: "", Kinds: items.NewKinds()})
	rej, err := ld.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "agents-md" {
			t.Fatalf("agents-md rejected by loader: %s", r.Reason)
		}
	}
	p, ok := ld.Get("agents-md")
	if !ok {
		t.Fatal("agents-md is not an active builtin pack")
	}
	tpl, ok := p.Templates["agents"]
	if !ok {
		t.Fatalf("templates/agents.md missing: %v", p.Templates)
	}
	var got []string
	for _, m := range regexp.MustCompile(`\{(\w+)\}`).FindAllStringSubmatch(tpl, -1) {
		got = append(got, m[1])
	}
	want := "managed_header,global_summary,project_sections,design_doc_index,fixed_guidance"
	if strings.Join(got, ",") != want {
		t.Fatalf("placeholders = %v, want %s", got, want)
	}
	if len(p.Prompts)+len(p.Views)+len(p.Kinds)+len(p.ClockRules)+len(p.SourceRules)+len(p.EventRules) != 0 || len(p.Templates) != 1 {
		t.Fatalf("agents-md must declare only the render template: %+v", p)
	}
}

// pack validate 用的同一个校验函数能通过磁盘上的包目录（C.11）。
func TestAgentsMDPackDirValidates(t *testing.T) {
	p, err := kp.Validate("../../../packs/agents-md", items.NewKinds().CheckSchema)
	if err != nil {
		t.Fatalf("pack rejected: %v", err)
	}
	if p.Name != "agents-md" {
		t.Fatalf("name = %q", p.Name)
	}
}
