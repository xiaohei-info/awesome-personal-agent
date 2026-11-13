//go:build e2e

package agents

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	designDoc = "docs/design.md"
	designV1  = "# 设计\n托管正文独有词 zebra-ledger\n"
)

type hostedContent struct {
	State   string `json:"state"`
	Content string `json:"content"`
	Commit  string `json:"commit"`
	Reason  string `json:"reason"`
	Repo    string `json:"repo"`
	Path    string `json:"path"`
}

// read 以 Web 会话读 /materials/{id}/content：读取中时轮询到有结果（页面的做法），HTTP 必须是 200。
func (f *fx) read(id string) hostedContent {
	f.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		c := decode[hostedContent](f.t, f.webOK("GET", "/materials/"+id+"/content", "", http.StatusOK))
		if c.State != "reading" {
			return c
		}
	}
	f.t.Fatalf("材料 %s 一直是读取中", id)
	return hostedContent{}
}

type shownMaterial struct {
	Material struct {
		ID, Kind, Title, Project, Location string
	} `json:"material"`
	Revision map[string]any `json:"revision"`
	Hosted   map[string]any `json:"hosted"`
}

func (f *fx) showMaterial(id string) shownMaterial {
	f.t.Helper()
	return decode[shownMaterial](f.t, f.apa(f.OBS, 0, "material", "show", id).Stdout)
}

func (f *fx) materialID(stdout string) string {
	f.t.Helper()
	m := decode[map[string]any](f.t, stdout)
	for _, k := range []string{"material", "id"} {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	f.t.Fatalf("没有返回材料 id: %s", stdout)
	return ""
}

// TestFlowMaterialHosted 对应验收文档 flow-material-hosted（ACT-HST-01..08，本地仓库版）：
// 代码托管平台换成 LOCAL_REPO（本机 git 仓库），"PR 合并"由执行者在本机分支合并回默认分支代替。
func TestFlowMaterialHosted(t *testing.T) {
	useHostedReader(t)
	f := setup(t, "")
	work, dir, repo := f.Projects["acc-work"], f.repoDir(), f.LocalRepo
	writeFile(t, dir, designDoc, designV1)
	writeFile(t, dir, "docs/other.md", "# 别的文档\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "docs")
	f.registerCheckout()
	branch := git(t, dir, "symbolic-ref", "--short", "HEAD")
	head := git(t, dir, "rev-parse", "HEAD")
	var design string

	t.Run("ACT-HST-01 登记托管型材料", func(t *testing.T) {
		r := f.apa(f.WRK, 0, "material", "add", "--repo", repo, "--path", designDoc)
		design = f.materialID(r.Stdout)
		s := f.showMaterial(design)
		if s.Material.Location == "" || s.Hosted["repo"] != repo || s.Hosted["path"] != designDoc {
			t.Errorf("show 应只回仓库与路径: %+v", s)
		}
		eq(t, "项目", s.Material.Project, work)
		if s.Revision != nil {
			t.Errorf("托管型没有 Revision: %+v", s.Revision)
		}
		eq(t, "修订数", len(decode[struct{ Revisions []any }](t, f.webOK("GET", "/materials/"+design+"/revisions", "", 200)).Revisions), 0)
		lacks(t, "show 不存正文副本", f.apa(f.OBS, 0, "material", "show", design).Stdout, "zebra-ledger")
	})

	t.Run("ACT-HST-02 托管型不能 commit，退出码 6 并提示仓库路径", func(t *testing.T) {
		r := f.Apa(f.WRK, "", "material", "commit", design, "--from", f.tmpFile("x.md", "x"), "--reason", "x")
		eq(t, "退出码", r.Code, 6)
		contains(t, "提示", r.Stderr, designDoc)
	})

	t.Run("ACT-HST-03 Web 只读展示默认分支内容，不含未提交修改", func(t *testing.T) {
		writeFile(t, dir, designDoc, designV1+"UNCOMMITTED-EDIT\n")
		defer git(t, dir, "checkout", "--", ".")
		c := f.read(design)
		if c.State != "ready" {
			t.Fatalf("状态 %s，原因 %s", c.State, c.Reason)
		}
		eq(t, "内容是默认分支提交", c.Content, designV1)
		eq(t, "标明 commit", c.Commit, head)
		lacks(t, "不含未提交修改", c.Content, "UNCOMMITTED-EDIT")
		n := len(decode[struct{ Runs []any }](t, f.webOK("GET", "/runs?category=reader&target="+design, "", 200)).Runs)
		if n == 0 {
			t.Error("读取应有 reader 类 Run")
		}
		eq(t, "登记的最近 commit", f.showMaterial(design).Hosted["commit"], head)
	})

	t.Run("ACT-HST-07 托管型只索引元信息", func(t *testing.T) {
		type hits struct{ Hits []map[string]any }
		eq(t, "正文词不命中", len(decode[hits](t, f.apa(f.OBS, 0, "material", "search", "zebra-ledger").Stdout).Hits), 0)
		raw := f.apa(f.OBS, 0, "material", "search", "design.md").Stdout
		h := decode[hits](t, raw).Hits
		if len(h) != 1 || h[0]["id"] != design || h[0]["hosted"] != true {
			t.Fatalf("路径片段应命中该托管材料: %v", h)
		}
		contains(t, "命中回路径", raw, designDoc)
	})

	t.Run("ACT-HST-08 项目页集中查看，两型并列可区分", func(t *testing.T) {
		own := decode[struct{ Material string }](t, f.apa(f.WRK, 0, "material", "commit", "--kind", "minutes", "--title", "自有纪要",
			"--project", work, "--reason", "起", "--from", f.tmpFile("m.md", "自有型正文")).Stdout).Material
		other := decode[struct{ Material string }](t, f.apa(f.WRK, 0, "material", "commit", "--kind", "minutes", "--title", "别项目纪要",
			"--project", f.Projects["acc-open"], "--reason", "起", "--from", f.tmpFile("m.md", "别的项目正文")).Stdout).Material
		list := decode[struct {
			Materials []struct{ ID, Project, Location string }
		}](t, f.webOK("GET", "/materials?project="+work, "", 200)).Materials
		var sawHosted, sawOwn bool
		for _, m := range list {
			eq(t, "只显示该项目", m.Project, work)
			if m.ID == other {
				t.Error("混入了其他项目的材料")
			}
			sawHosted = sawHosted || (m.ID == design && m.Location != "")
			sawOwn = sawOwn || (m.ID == own && m.Location == "")
		}
		if !sawHosted || !sawOwn {
			t.Errorf("材料视图应并列托管型（有 location）与自有型（无 location）: %+v", list)
		}
		proj := f.webOK("GET", "/projects/"+work, "", 200)
		contains(t, "项目页", proj, repo, "work-host.cli")
		contains(t, "事项视图只含该项目", f.webOK("GET", "/items?project="+work, "", 200), `"items"`)
	})

	t.Run("ACT-HST-06 发起修改走委派和 PR，合并后最近 commit 刷新", func(t *testing.T) {
		if r := f.Web("POST", "/v1/materials/"+design+"/changes", `{"assignee":"work-host.cli"}`); r.Code != http.StatusBadRequest {
			t.Errorf("不填修改内容应被拒绝: %d %s", r.Code, r.Body)
		}
		if r := f.Web("POST", "/v1/materials/"+design+"/changes", `{"assignee":"other-host.cli","request":"x"}`); r.Code < 400 {
			t.Errorf("不在允许名单里的执行者应被拒绝: %d %s", r.Code, r.Body)
		}
		out := decode[struct{ Item, Parent string }](t, f.webOK("POST", "/materials/"+design+"/changes", `{"assignee":"work-host.cli","request":"补一节限流说明"}`, http.StatusCreated))
		child := f.item(out.Item)
		eq(t, "子事项项目", child["project"], work)
		eq(t, "子事项 assignee", child["assignee"], "work-host.cli")
		body, _ := child["body"].(string)
		contains(t, "子事项正文", body, repo, designDoc, "补一节限流说明")

		// 执行者在 LOCAL_REPO 开分支提交，合并回默认分支（本地仓库没有托管平台，以本地合并代替 PR 合并）。
		git(t, dir, "checkout", "-b", "docs/rate-limit")
		writeFile(t, dir, designDoc, designV1+"## 限流\n令牌桶\n")
		git(t, dir, "commit", "-am", "docs: 限流说明")
		git(t, dir, "checkout", branch)
		git(t, dir, "merge", "--no-ff", "-m", "Merge PR #13", "docs/rate-limit")
		merged := git(t, dir, "rev-parse", "HEAD")
		f.apa(f.WRK, 0, "report", out.Item, "--outcome", "done", "--summary", "已合并", "--pr", "https://example.invalid/pr/13", "--commit", merged)
		eq(t, "子事项阶段", f.phase(out.Item), "done")
		f.waitFor("最近 commit 刷新", 10*time.Second, func() bool { return f.showMaterial(design).Hosted["commit"] == merged })
		contains(t, "Web 展示合并后的内容", f.read(design).Content, "令牌桶")
	})

	t.Run("ACT-HST-05 路径被删时生成 needs_input 修正登记，AGENTS.md 索引重新渲染", func(t *testing.T) {
		other := f.materialID(f.apa(f.WRK, 0, "material", "add", "--repo", repo, "--path", "docs/other.md", "--kind", "reference", "--project", work).Stdout)
		agentsMD := func() string {
			return decode[struct{ Content string }](t, f.webOK("GET", "/agents-md?repo="+repo, "", 200)).Content
		}
		contains(t, "删除前的 AGENTS.md 索引", agentsMD(), "docs/other.md")
		git(t, dir, "rm", "-q", "docs/other.md")
		git(t, dir, "commit", "-m", "remove other")
		f.Apa(f.WRK, "", "agents", "check", "--repo", repo, "--file", filepath.Join(dir, "AGENTS.md"))
		var fix any
		f.waitFor("生成修正登记事项", 10*time.Second, func() bool { fix = f.showMaterial(other).Hosted["fix_item"]; return fix != nil })
		eq(t, "修正事项阶段", f.phase(fix.(string)), "needs_input")
		lacks(t, "索引随之重新渲染", agentsMD(), "docs/other.md")
	})

	t.Run("ACT-HST-04 读不到时降级显示", func(t *testing.T) {
		lost := f.materialID(f.apa(f.WRK, 0, "material", "add", "--repo", repo, "--path", "docs/never-read.md", "--kind", "design", "--project", work).Stdout)
		// 把 LOCAL_REPO 目录（也是本机检出）临时改名：检出不存在、远程不可达。
		gone := dir + ".gone"
		if err := os.Rename(dir, gone); err != nil {
			t.Fatal(err)
		}
		restored := false
		restore := func() {
			if !restored {
				restored = true
				if err := os.Rename(gone, dir); err != nil {
					t.Fatal(err)
				}
			}
		}
		defer restore()
		c := f.read(lost) // read 断言 HTTP 200：页面不报错
		eq(t, "状态", c.State, "unavailable")
		if c.Reason == "" || c.Repo != repo || c.Path != "docs/never-read.md" {
			t.Errorf("应带原因并照常显示登记信息: %+v", c)
		}
		eq(t, "登记信息仍可读", f.showMaterial(lost).Hosted["path"], "docs/never-read.md")
		restore()
	})
}
