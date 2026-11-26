package conformance

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
)

// TestSourceRuleLegacyExitsAndFieldsRejected 对照测试（附录 C.5、C.11）：
// 旧来源出口别名（screen/judge/extract）与旧规则级字段（screen/prescreen/draft/extract/notify_urgent）必须被加载器明确拒绝；
// 仅接受现行出口枚举（proposed/ignore/process/digest/alert）。
func TestSourceRuleLegacyExitsAndFieldsRejected(t *testing.T) {
	validate := func(rule string) error {
		dir := t.TempDir()
		yaml := "name: legacy-check\nversion: 1.0.0\ndescription: conformance check\nsource_rules:\n  - id: r1\n    match: {source_type: mail}\n" + rule
		if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := packs.Validate(dir, nil)
		return err
	}

	// 旧出口别名必须拒绝
	for _, oldExit := range []string{"screen", "judge", "extract"} {
		if err := validate("    then: " + oldExit + "\n"); err == nil {
			t.Errorf("legacy exit %q must be rejected by pack loader", oldExit)
		}
	}

	// 旧规则级字段必须拒绝
	legacyFields := []struct {
		name string
		yaml string
	}{
		{"screen", "    then: process\n    screen: custom-screen\n"},
		{"prescreen", "    then: process\n    prescreen: none\n"},
		{"draft", "    then: process\n    draft: custom-draft\n"},
		{"extract", "    then: process\n    extract: custom-extract\n"},
		{"notify_urgent", "    then: process\n    notify_urgent: true\n"},
	}
	for _, tc := range legacyFields {
		if err := validate(tc.yaml); err == nil {
			t.Errorf("legacy source_rule field %q must be rejected by pack loader", tc.name)
		}
	}

	// 现行合法出口必须接受
	valid := []string{
		"    then: proposed\n",
		"    then: ignore\n",
		"    then: process\n",
		"    then: digest\n",
		"    then: alert\n    where: [{field: sender, op: eq, value: boss}]\n",
	}
	for _, rule := range valid {
		if err := validate(rule); err != nil {
			t.Errorf("valid source rule %q was rejected: %v", rule, err)
		}
	}
}
