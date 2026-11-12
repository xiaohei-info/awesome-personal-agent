package release

// flow-release 的 ACT-REL-10（用户文档齐全）与 ACT-REL-11（变更记录写明界面语言限制与需迁移的变更）。
// 文档是发布物，所以这里直接读 docs/user/，并与设计文档（§2.4、§16.5、§20.4、B.6）对账。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readDoc(t *testing.T, root string, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
	if err != nil {
		t.Fatalf("缺文件：%v", err)
	}
	return string(b)
}

// tableRows 返回 md 中以 heading 开头的小节里表格的各数据行（按 | 拆成单元格）。
func tableRows(md, heading string) [][]string {
	i := strings.Index(md, heading)
	if i < 0 {
		return nil
	}
	var rows [][]string
	for _, l := range strings.Split(md[i+len(heading):], "\n")[1:] {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") || strings.HasPrefix(l, "---") {
			break
		}
		if !strings.HasPrefix(l, "|") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		for k := range cells {
			cells[k] = strings.TrimSpace(cells[k])
		}
		if strings.HasPrefix(cells[0], "---") || cells[0] == "形态" || cells[0] == "#" {
			continue
		}
		rows = append(rows, cells)
	}
	return rows
}

// ACT-REL-10：七篇用户文档齐全，docs/README.md 登记了 docs/user/。
func TestUserDocsComplete(t *testing.T) {
	root := repoRoot(t)
	for _, f := range []string{"README.md", "quickstart.md", "run-tiers.md", "troubleshooting.md", "security.md", "upgrade.md", "uninstall.md", "changelog.md"} {
		if b := readDoc(t, root, "docs", "user", f); len(strings.TrimSpace(b)) < 300 {
			t.Errorf("docs/user/%s 内容过少", f)
		}
	}
	idx := readDoc(t, root, "docs", "user", "README.md")
	for _, f := range []string{"quickstart.md", "run-tiers.md", "troubleshooting.md", "security.md", "upgrade.md", "uninstall.md", "changelog.md"} {
		if !strings.Contains(idx, "("+f+")") {
			t.Errorf("docs/user/README.md 没有链接 %s", f)
		}
	}
	if !strings.Contains(readDoc(t, root, "docs", "README.md"), "user/") {
		t.Error("docs/README.md 没有登记 docs/user/")
	}
	// 安全说明含远程访问与外发范围；快速开始含安装步骤；故障排查指向 doctor。
	sec := readDoc(t, root, "docs", "user", "security.md")
	for _, k := range []string{"public_url", "HTTPS", "外发范围", "outbound_unset", "12 个字符"} {
		if !strings.Contains(sec, k) {
			t.Errorf("security.md 缺少 %q", k)
		}
	}
	for _, k := range []string{"apa init", "apa serve", "apa doctor"} {
		if !strings.Contains(readDoc(t, root, "docs", "user", "quickstart.md"), k) {
			t.Errorf("quickstart.md 缺少 %q", k)
		}
	}
	if !strings.Contains(readDoc(t, root, "docs", "user", "troubleshooting.md"), "apa doctor") {
		t.Error("troubleshooting.md 没有指向 apa doctor")
	}
}

// ACT-REL-10：三档运行形态说明与设计 §2.4 逐格一致（各档可用与不可用的能力）。
func TestRunTiersMatchDesign(t *testing.T) {
	root := repoRoot(t)
	design := readDoc(t, root, "docs", "solution-design.md")
	doc := readDoc(t, root, "docs", "user", "run-tiers.md")
	rows := tableRows(design, "### 2.4 三档运行形态")
	if len(rows) != 3 {
		t.Fatalf("设计 §2.4 应有三档，解析到 %d", len(rows))
	}
	for _, r := range rows {
		if len(r) != 4 {
			t.Fatalf("§2.4 表列数异常：%v", r)
		}
		for i, name := range []string{"形态", "需要", "可用", "不可用"} {
			if r[i] == "—" {
				continue
			}
			if !strings.Contains(doc, r[i]) {
				t.Errorf("run-tiers.md 与 §2.4 不一致：缺 %s「%s」（档：%s）", name, r[i], r[0])
			}
		}
	}
}

// ACT-REL-10：升级说明含自动快照、回退只有恢复快照一条路（§16.5），并登记升级后本机验证清单 L1–L12（§20.4）。
func TestUpgradeDocMatchesDesign(t *testing.T) {
	root := repoRoot(t)
	doc := readDoc(t, root, "docs", "user", "upgrade.md")
	for _, k := range []string{"自动快照", "<APA_HOME>/backups/", "apa doctor --upgrade-check", "apa backup create", "apa backup verify", "apa backup restore", "只有“恢复快照”这一条路", "拒绝打开"} {
		if !strings.Contains(doc, k) {
			t.Errorf("upgrade.md 缺少 %q", k)
		}
	}
	design := readDoc(t, root, "docs", "solution-design.md")
	rows := tableRows(design, "**升级后本机验证清单**")
	if len(rows) != 12 {
		t.Fatalf("设计 §20.4 清单应有 12 项，解析到 %d", len(rows))
	}
	for i, r := range rows {
		id := fmt.Sprintf("L%d", i+1)
		if r[0] != id {
			t.Fatalf("清单第 %d 行编号为 %s", i+1, r[0])
		}
		if !strings.Contains(doc, "| "+id+" | "+r[1]+" |") {
			t.Errorf("upgrade.md 清单缺 %s「%s」或与设计不一致", id, r[1])
		}
	}
}

var migrationItem = regexp.MustCompile(`(?m)^\| M-\d+ \|`)

// ACT-REL-11：变更记录本版一节写明界面语言限制，逐条列出必须迁移的变更。
func TestChangelogLanguageAndMigrations(t *testing.T) {
	root := repoRoot(t)
	doc := readDoc(t, root, "docs", "user", "changelog.md")
	i := strings.Index(doc, "## 0.2.0")
	if i < 0 {
		t.Fatal("changelog.md 没有 0.2.0 一节")
	}
	sec := doc[i:]
	for _, k := range []string{"Web 界面", "内置提示词", "中文", "CLI 的命令", "输出键名", "错误码", "英文"} {
		if !strings.Contains(sec, k) {
			t.Errorf("语言限制缺少 %q", k)
		}
	}
	// 至少四项必须迁移的变更，各自要有可检索的特征词。
	for name, kw := range map[string][]string{
		"外发范围需在 Web 选择":  {"外发范围", "Web", "outbound_unset"},
		"宿主插件须与内核同时升级": {"宿主插件", "同时升级", "session_not_direct"},
		"升级后登录会话失效":     {"登录会话", "失效", "重新登录"},
		"日上限默认生效":       {"disturb.daily_cap", "20", "0"},
	} {
		found := false
		for _, l := range strings.Split(sec, "\n") {
			if !migrationItem.MatchString(l) {
				continue
			}
			ok := true
			for _, k := range kw {
				ok = ok && strings.Contains(l, k)
			}
			found = found || ok
		}
		if !found {
			t.Errorf("需迁移的变更表漏列：%s", name)
		}
	}
}
