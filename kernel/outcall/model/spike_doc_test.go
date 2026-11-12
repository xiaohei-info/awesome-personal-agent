package model

import (
	"os"
	"strings"
	"testing"
)

// ACT-GATE-05、ACT-GATE-06：SPIKE-CLS-BATCH、SPIKE-LOGPROB 的替身部分结论写回 §8.8、§21.3、附录 D.7，
// 默认值不得写成已在真实端点确认；设置页的协议选项不含 chat-logprobs。
func TestSpikeSubstituteConclusionsWrittenBack(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	design := read("../../../docs/solution-design.md")
	i := strings.Index(design, "**开发验收的替身结论**")
	if i < 0 {
		t.Fatal("§8.8 lacks substitute conclusions")
	}
	sec := design[i:]
	sec = sec[:strings.Index(sec, "**不算分类端点的**")]
	for _, w := range []string{"ACT-JDP-06", "ACT-JDP-07", "ACT-JDP-08", "TestChatLogprobsConversion", "尚未在真实端点上确认", "L8"} {
		if !strings.Contains(sec, w) {
			t.Errorf("§8.8 conclusions miss %q", w)
		}
	}
	for _, id := range []string{"SPIKE-LOGPROB", "SPIKE-CLS-BATCH"} {
		var row string
		for _, l := range strings.Split(design, "\n") {
			if strings.HasPrefix(l, "| `"+id+"`") {
				row = l
			}
		}
		if !strings.Contains(row, "替身部分已验证，见 §8.8") {
			t.Errorf("§21.3 row %s not updated", id)
		}
	}
	appx := read("../../../docs/appendix-d-adapters.md")
	if strings.Count(appx, "替身上验证（主文档 §8.8）") != 2 {
		t.Error("appendix D.7 not updated")
	}
	web := read("../../../web/src/features/settings-policy/SettingsPolicyPage.tsx")
	if !strings.Contains(web, `const JUDGE_PROTOCOLS = ["probability-judgment/1"];`) {
		t.Error("settings page must offer only probability-judgment/1")
	}
}
