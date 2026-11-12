package bands

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
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

// Apply 把策略的带与影子开关写进判断题；影子只看模板名。
func TestApplyFillsBandAndShadow(t *testing.T) {
	p := pol()
	c := direct.JudgeCall{Template: "triage-screen", Source: "src_a"}
	Apply(p, &c)
	if c.Band == nil || *c.Band != (record.Band{Low: 0.1, High: 0.3}) || c.Shadow {
		t.Fatalf("triage: %+v", c)
	}
	c = direct.JudgeCall{Template: "capture-prescreen"}
	Apply(p, &c)
	if c.Band != nil || !c.Shadow {
		t.Fatalf("capture: %+v", c)
	}
	// 调用方已显式给带时不覆盖。
	own := &record.Band{Low: 0.9, High: 0.95}
	c = direct.JudgeCall{Template: "triage-screen", Band: own}
	Apply(p, &c)
	if c.Band != own {
		t.Fatal("explicit band overwritten")
	}
}
