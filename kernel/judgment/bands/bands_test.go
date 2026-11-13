package bands

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

func pol() policy.Policy {
	p := policy.Default()
	p.Judgment.Bands = []policy.JudgmentBand{
		{Template: "triage-screen", Low: 0.25, High: 0.5},
		{Template: "triage-screen", Source: "src_a", Low: 0.1, High: 0.3},
	}
	p.Judgment.Shadow = []string{"capture-prescreen"}
	return p
}

// 带 source 的条目优先于模板级条目；没有覆盖时返回 nil（取模板声明的带）。
func TestResolvePrefersSourceOverride(t *testing.T) {
	p := pol()
	cases := []struct {
		tmpl, src string
		want      *record.Band
	}{
		{"triage-screen", "src_a", &record.Band{Low: 0.1, High: 0.3}},
		{"triage-screen", "src_b", &record.Band{Low: 0.25, High: 0.5}},
		{"triage-screen", "", &record.Band{Low: 0.25, High: 0.5}},
		{"capture-prescreen", "src_a", nil},
	}
	for _, c := range cases {
		got := Resolve(p, c.tmpl, c.src)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("Resolve(%s,%s) = %v want %v", c.tmpl, c.src, got, c.want)
		}
	}
}

// 影子只看模板名，且只出现在 shadow 列表里的模板才开。
func TestShadow(t *testing.T) {
	p := pol()
	if !Shadow(p, "capture-prescreen") || Shadow(p, "triage-screen") {
		t.Fatal("shadow list not honoured")
	}
}
