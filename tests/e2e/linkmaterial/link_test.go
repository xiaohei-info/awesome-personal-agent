//go:build e2e

package linkmaterial

import (
	"path/filepath"
	"testing"
)

// TestFlowLinkMaterial 对应验收 flow-link-material（§12.8）：笔记库登记后登记链接型材料；
// 同一项目同一位置重复登记返回同一条；未登记的库名与本机绝对路径被拒。
func TestFlowLinkMaterial(t *testing.T) {
	f := setup(t)
	open, work := f.Projects["acc-open"], f.Projects["acc-work"]

	t.Run("库名经 GET /note-vaults 读到（observer）", func(t *testing.T) {
		r := f.APIRaw("GET", "/v1/note-vaults", "", f.OBS)
		if r.Code != 200 {
			t.Fatalf("HTTP %d %s", r.Code, r.Body)
		}
		got := decode[struct {
			Vaults []struct {
				Name    string `json:"name"`
				OpenURL string `json:"open_url"`
			} `json:"vaults"`
		}](t, r.Body)
		if len(got.Vaults) != 1 || got.Vaults[0].Name != "notes" || got.Vaults[0].OpenURL != "notes://open?vault={vault}&file={path}" {
			t.Fatalf("笔记库 = %+v", got.Vaults)
		}
	})

	var first string
	t.Run("登记一条库内链接：链接型、kind 缺省 reference、带打开方式", func(t *testing.T) {
		a := f.addLink(0, open, "vault:notes/docs/a.md", "文档 A")
		if !a.Created || a.Material == "" {
			t.Fatalf("登记结果 = %+v", a)
		}
		first = a.Material
		ms := f.materials(open)
		if len(ms) != 1 || ms[0].ID != first || ms[0].Form != "link" || ms[0].Kind != "reference" || ms[0].Title != "文档 A" || ms[0].Link != "vault:notes/docs/a.md" {
			t.Fatalf("项目材料 = %+v", ms)
		}
		if body := f.webOK("GET", "/materials?project="+open, "", 200); !contains(body, "notes://open?vault=notes\\u0026file=docs%2Fa.md") {
			t.Errorf("材料应带渲染后的打开方式（占位符做 URL 编码）: %s", body)
		}
	})

	t.Run("同一项目同一位置重复登记返回同一条，不改标题与 kind", func(t *testing.T) {
		a := f.addLink(0, open, "vault:notes/docs/a.md", "改名想法", "--kind", "design")
		if a.Created || a.Material != first {
			t.Fatalf("重复登记 = %+v，期望 created=false、material=%s", a, first)
		}
		ms := f.materials(open)
		if len(ms) != 1 || ms[0].Title != "文档 A" || ms[0].Kind != "reference" {
			t.Fatalf("重复登记改动了已有材料: %+v", ms)
		}
	})

	t.Run("网址的协议与主机名转小写后同一位置", func(t *testing.T) {
		a := f.addLink(0, open, "https://Example.COM/Doc", "网页")
		b := f.addLink(0, open, "https://example.com/Doc", "网页二")
		if !a.Created || b.Created || a.Material != b.Material {
			t.Fatalf("a=%+v b=%+v", a, b)
		}
	})

	t.Run("同一位置在另一个项目是另一条", func(t *testing.T) {
		a := f.addLink(0, work, "vault:notes/docs/a.md", "文档 A")
		if !a.Created || a.Material == first {
			t.Fatalf("另一个项目应新登记一条: %+v（第一条 %s）", a, first)
		}
	})

	t.Run("未登记的库名与本机绝对路径被拒（退出码 2），账本不增", func(t *testing.T) {
		before := len(f.materials(open))
		for name, link := range map[string]string{
			"未登记的库名":   "vault:nope/docs/x.md",
			"本机绝对路径":   string(filepath.Separator) + filepath.Join("abs", "notes", "x.md"),
			"file 协议":  "file:///etc/hosts",
			"库内路径含 ..": "vault:notes/../x.md",
		} {
			r := f.Apa(f.WRK, "", "material", "add", "--link", link, "--title", name, "--project", open)
			if r.Code != 2 {
				t.Errorf("%s（%s）：退出码 %d，期望 2\n%s%s", name, link, r.Code, r.Stdout, r.Stderr)
			}
		}
		if after := len(f.materials(open)); after != before {
			t.Errorf("被拒的登记写入了账本: %d → %d", before, after)
		}
	})
}
