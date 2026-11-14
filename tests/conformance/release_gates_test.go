package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 发布门槛 4（主文档 §20.4）：两份缺陷账本里没有未关闭、无人负责的条目；已修复的必须能指出修复节点或已合并的修复 PR。
func TestDefectLedgersHaveNoOpenEntries(t *testing.T) {
	for _, f := range []string{".omac/apa.state/_dag.yaml", ".omac/apa-v02.state/_dag.yaml"} {
		b, err := os.ReadFile(filepath.Join(repoRootDir(t), f))
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Defects struct {
				Entries map[string]struct {
					State       string `yaml:"state"`
					FixNode     string `yaml:"fix_node"`
					ExternalFix string `yaml:"external_fix"`
					Owner       string `yaml:"owner"`
				} `yaml:"entries"`
			} `yaml:"defects"`
		}
		if err := yaml.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(d.Defects.Entries) == 0 {
			t.Fatalf("%s: 账本没有条目，读取方式可能已失效", f)
		}
		for id, e := range d.Defects.Entries {
			if e.State != "fixed" && e.Owner == "" {
				t.Errorf("%s %s: 状态 %q 且无人负责", f, id, e.State)
			}
			if e.State == "fixed" && e.FixNode == "" && e.ExternalFix == "" {
				t.Errorf("%s %s: 已修复但没有修复节点或外部修复 PR", f, id)
			}
		}
	}
}

// 发布门槛 3：§21.3 每一行都说明了由谁证实，且开发验收或本机升级验证的条目有结论或对应的清单编号（L1–L12）。
func TestRealMeasurementRowsHaveConclusionOrChecklistNumber(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRootDir(t), "docs", "solution-design.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "### 21.3 需实测的事项")
	if i < 0 {
		t.Fatal("找不到 §21.3")
	}
	sec := doc[i:]
	if j := strings.Index(sec[5:], "\n#"); j >= 0 {
		sec = sec[:j+5]
	}
	ok := regexp.MustCompile(`已关闭|已证实|L\d+|接入时`)
	rows := 0
	for _, line := range strings.Split(sec, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 6 || !strings.HasPrefix(cells[1], " ") || strings.Contains(cells[1], "编号") || strings.Contains(cells[1], "---") {
			continue
		}
		rows++
		if !ok.MatchString(cells[3]) {
			t.Errorf("%s: 由谁证实一栏没有结论、清单编号或接入时验收: %s", strings.TrimSpace(cells[1]), strings.TrimSpace(cells[3]))
		}
	}
	if rows < 10 {
		t.Fatalf("只读到 %d 行，§21.3 的表格格式可能已变", rows)
	}
}

func repoRootDir(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			t.Fatal("找不到仓库根")
		}
		d = p
	}
}
