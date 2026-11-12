package conformance

import (
	"os"
	"strings"
	"testing"
)

// ACT-GATE-11：§8.9 写明放开四步与每日上限已在合成夹具上用替身验收，不确定带起始值与带内占比注明由 L5 在真实积压上得出，没有写成已校准。
func TestBacklogReleaseConclusionWrittenBackToSection89(t *testing.T) {
	b, err := os.ReadFile("../../docs/solution-design.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "### 8.9 ")
	j := strings.Index(s, "### 8.10 ")
	if i < 0 || j < i {
		t.Fatal("section 8.9 not found")
	}
	sec := s[i:j]
	for _, want := range []string{"合成夹具上用分类端点与对话端点的替身验收", "backlog_release_per_day", "0.15", "0.60", "L5", "没有"} {
		if !strings.Contains(sec, want) {
			t.Fatalf("§8.9 lacks %q", want)
		}
	}
	if strings.Contains(sec, "已校准") && !strings.Contains(sec, "没有") {
		t.Fatal("§8.9 must not claim the band is calibrated")
	}
}
