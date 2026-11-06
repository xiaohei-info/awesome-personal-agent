//go:build e2e

package materials

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	designV1  = "# 设计\n托管正文独有词 zebra-ledger\n"
	designDoc = "docs/design.md"
)

// repoFixture 是一个本地 git 远程（bare）、一份 APA 主机检出和一份"执行者"工作副本；远程默认分支 main。
type repoFixture struct {
	origin, checkout, work string
}

func newRepo(t *testing.T) repoFixture {
	t.Helper()
	root := t.TempDir()
	r := repoFixture{origin: filepath.Join(root, "origin.git"), checkout: filepath.Join(root, "apa-checkout"), work: filepath.Join(root, "worker-copy")}
	git(t, root, "init", "--bare", "-b", "main", r.origin)
	git(t, root, "clone", r.origin, r.work)
	git(t, r.work, "checkout", "-b", "main")
	write(t, r.work, designDoc, designV1)
	write(t, r.work, "docs/other.md", "# 别的文档\n")
	git(t, r.work, "add", ".")
	git(t, r.work, "commit", "-m", "init")
	git(t, r.work, "push", "origin", "main")
	git(t, root, "clone", r.origin, r.checkout)
	return r
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type content struct {
	State   string `json:"state"`
	Content string `json:"content"`
	Commit  string `json:"commit"`
	Reason  string `json:"reason"`
	Repo    string `json:"repo"`
	Path    string `json:"path"`
}

// read 以 Web 会话读 /materials/{id}/content：不阻塞，"读取中"时轮询到有结果（页面的做法）。
func (f *fx) read(id string) content {
	f.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		c := decode[content](f.t, f.webOK("GET", "/materials/"+id+"/content", "", http.StatusOK))
		if c.State != "reading" {
			return c
		}
	}
	f.t.Fatalf("材料 %s 一直是读取中", id)
	return content{}
}

func (f *fx) readerRuns(id string) int {
	f.t.Helper()
	return len(decode[struct {
		Runs []map[string]any `json:"runs"`
	}](f.t, f.webOK("GET", "/runs?category=reader&target="+id, "", http.StatusOK)).Runs)
}

// TestFlowMaterialHosted 对应验收文档 flow-material-hosted（ACT-HST-01..08）。
// <TEST_REPO> 是本地 bare 仓库；"PR 合并"由执行者副本向 main 推提交代替（没有 GitHub，合并结果在 git 上等价）。
func TestFlowMaterialHosted(t *testing.T) {
	rp := newRepo(t)
	f := &fx{newEnv(t, rp.origin), t}
	work := f.Projects["acc-work"]
	host, _ := os.Hostname()
	head := git(t, rp.checkout, "rev-parse", "HEAD")
	var design string

	// 本机检出登记到项目 repos（§12.3：APA 主机有检出时读默认分支）。
	f.webOK("POST", "/projects/"+work+"/repos", `{"op":"set_checkout","remote":"`+rp.origin+`","host":"`+host+`","path":"`+rp.checkout+`"}`, http.StatusOK)

	t.Run("ACT-HST-01 登记托管型材料", func(t *testing.T) {
		// 验收文档给出的命令不带 --kind/--project（附录 B：`apa material add --repo --path`）。
		r := f.Apa(f.WRK, "", "material", "add", "--repo", rp.origin, "--path", designDoc)
		if r.Code != 0 {
			t.Errorf("按验收文档的命令登记失败，退出码 %d: %s", r.Code, r.Stderr)
			// 后续步骤依赖这条登记：补全 kind 与 project 再登记，保证其余动作仍被真实执行。
			r = f.apa(f.WRK, 0, "material", "add", "--repo", rp.origin, "--path", designDoc, "--kind", "design", "--project", work)
		}
		out := decode[map[string]any](t, r.Stdout)
		design, _ = out["material"].(string)
		if design == "" {
			design, _ = out["id"].(string)
		}
		if design == "" {
			t.Fatalf("没有返回材料 id: %v", out)
		}
		s := f.show(design)
		if s.Material.Location == "" || s.Hosted["repo"] != rp.origin || s.Hosted["path"] != designDoc {
			t.Errorf("show 应只回仓库与路径: %+v", s)
		}
		if s.Revision != nil {
			t.Errorf("托管型没有 Revision: %+v", s.Revision)
		}
		eq(t, "kind", s.Material.Kind, "design")
		eq(t, "修订数", len(f.revisions(design)), 0)
		if raw := f.apa(f.OBS, 0, "material", "show", design).Stdout; strings.Contains(raw, "zebra-ledger") {
			t.Errorf("show 不应存正文副本: %s", raw)
		}
	})

	t.Run("ACT-HST-02 托管型不能 commit，退出码 6 并提示仓库路径", func(t *testing.T) {
		r := f.Apa(f.WRK, "", "material", "commit", design, "--from", f.file("x.md", "x"), "--reason", "x")
		eq(t, "退出码", r.Code, 6)
		if !strings.Contains(r.Stderr, designDoc) {
			t.Errorf("应提示其仓库路径 %s: %s", designDoc, r.Stderr)
		}
		eq(t, "修订数", len(f.revisions(design)), 0)
	})

	t.Run("ACT-HST-03 Web 只读展示默认分支内容，不含未提交修改", func(t *testing.T) {
		write(t, rp.checkout, designDoc, designV1+"UNCOMMITTED-EDIT\n")
		c := f.read(design)
		eq(t, "状态", c.State, "ready")
		eq(t, "内容是默认分支提交", c.Content, designV1)
		eq(t, "标明 commit", c.Commit, head)
		if strings.Contains(c.Content, "UNCOMMITTED-EDIT") {
			t.Error("看到了工作区未提交的修改")
		}
		if f.readerRuns(design) == 0 {
			t.Error("读取应有 reader 类 Run")
		}
		if got := f.show(design).Hosted["commit"]; got != head {
			t.Errorf("登记的最近 commit = %v，期望 %s", got, head)
		}
		git(t, rp.checkout, "checkout", "--", ".")
	})

	t.Run("ACT-HST-07 托管型只索引元信息", func(t *testing.T) {
		type hits struct {
			Hits []map[string]any `json:"hits"`
		}
		body := decode[hits](t, f.apa(f.OBS, 0, "material", "search", "zebra-ledger").Stdout)
		eq(t, "正文词不命中", len(body.Hits), 0)
		path := decode[hits](t, f.apa(f.OBS, 0, "material", "search", "design.md").Stdout)
		if len(path.Hits) != 1 || path.Hits[0]["id"] != design || path.Hits[0]["hosted"] != true {
			t.Fatalf("路径片段应命中该托管材料: %v", path.Hits)
		}
		if raw := f.apa(f.OBS, 0, "material", "search", "design.md").Stdout; !strings.Contains(raw, designDoc) {
			t.Errorf("命中应回路径: %s", raw)
		}
	})

	t.Run("ACT-HST-08 项目页集中查看，两型并列可区分", func(t *testing.T) {
		own := f.commit(f.WRK, "自有型正文", "--reason", "起", "--kind", "minutes", "--title", "自有纪要", "--project", work)
		other := f.commit(f.WRK, "别的项目正文", "--reason", "起", "--kind", "minutes", "--title", "别项目纪要", "--project", f.Projects["acc-open"])
		list := decode[struct {
			Materials []material `json:"materials"`
		}](t, f.webOK("GET", "/materials?project="+work, "", http.StatusOK)).Materials
		var sawHosted, sawOwn bool
		for _, m := range list {
			eq(t, "只显示该项目", m.Project, work)
			if m.ID == other.Material {
				t.Error("混入了其他项目的材料")
			}
			sawHosted = sawHosted || (m.ID == design && m.Location != "")
			sawOwn = sawOwn || (m.ID == own.Material && m.Location == "")
		}
		if !sawHosted || !sawOwn {
			t.Errorf("材料视图应并列托管型（有 location）与自有型（无 location）: %+v", list)
		}
		proj := f.webOK("GET", "/projects/"+work, "", http.StatusOK)
		if !strings.Contains(proj, rp.origin) || !strings.Contains(proj, "work-host.cli") {
			t.Errorf("项目应带 repos 属性与允许的执行者: %s", proj)
		}
	})

	t.Run("ACT-HST-06 发起修改走委派和 PR，合并后最近 commit 刷新", func(t *testing.T) {
		body := `{"assignee":"work-host.cli","request":"补一节限流说明"}`
		if r := f.Web("POST", "/v1/materials/"+design+"/changes", `{"assignee":"work-host.cli"}`); r.Code != http.StatusBadRequest {
			t.Errorf("不填修改内容应被拒绝: %d %s", r.Code, r.Body)
		}
		if r := f.Web("POST", "/v1/materials/"+design+"/changes", `{"assignee":"other-host.cli","request":"x"}`); r.Code < 400 {
			t.Errorf("不在允许名单里的执行者应被拒绝: %d %s", r.Code, r.Body)
		}
		out := decode[struct {
			Item   string `json:"item"`
			Parent string `json:"parent"`
		}](t, f.webOK("POST", "/materials/"+design+"/changes", body, http.StatusCreated))
		child := f.itemShow(out.Item)
		if child["project"] != work || child["assignee"] != "work-host.cli" || !strings.Contains(child["body"].(string), rp.origin) || !strings.Contains(child["body"].(string), designDoc) || !strings.Contains(child["body"].(string), "补一节限流说明") {
			t.Errorf("子事项应带 project、仓库、路径和修改要求: %v", child)
		}
		parent := f.itemShow(out.Parent)
		eq(t, "父事项项目", parent["project"], work)
		eq(t, "父事项归本人", parent["assignee"], "owner")

		// 执行者开分支提 PR，评审合并（在 git 上以推送到 main 等价）。
		git(t, rp.work, "checkout", "-b", "docs/rate-limit")
		write(t, rp.work, designDoc, designV1+"## 限流\n令牌桶\n")
		git(t, rp.work, "commit", "-am", "docs: 限流说明")
		git(t, rp.work, "checkout", "main")
		git(t, rp.work, "merge", "--no-ff", "-m", "Merge PR #1", "docs/rate-limit")
		git(t, rp.work, "push", "origin", "main")
		merged := git(t, rp.work, "rev-parse", "HEAD")
		git(t, rp.checkout, "fetch", "origin") // APA 主机的检出随合并更新，读取器读它的默认分支
		f.apa(f.WRK, 0, "report", out.Item, "--outcome", "done", "--summary", "已合并", "--pr", "https://example.invalid/acc/test-repo/pull/1", "--commit", merged)
		eq(t, "子事项阶段", f.itemShow(out.Item)["phase"], "done")
		var got any
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if got = f.show(design).Hosted["commit"]; got == merged {
				break
			}
		}
		if got != merged {
			t.Errorf("合并并回执后，登记的最近 commit = %v，期望刷新为 %s", got, merged)
		}
	})

	t.Run("ACT-HST-05 路径被删时生成 needs_input 修正登记，AGENTS.md 索引重新渲染", func(t *testing.T) {
		other := decode[map[string]any](t, f.apa(f.WRK, 0, "material", "add", "--repo", rp.origin, "--path", "docs/other.md", "--kind", "reference", "--project", work).Stdout)["material"].(string)
		if c := f.read(other); c.State != "ready" {
			t.Fatalf("删除前应能读到: %+v", c)
		}
		before := decode[struct {
			Content string `json:"content"`
		}](t, f.webOK("GET", "/agents-md?repo="+rp.origin, "", http.StatusOK)).Content
		if !strings.Contains(before, "docs/other.md") {
			t.Fatalf("删除前 AGENTS.md 索引应含该路径: %s", before)
		}
		git(t, rp.work, "rm", "docs/other.md")
		git(t, rp.work, "commit", "-m", "remove other")
		git(t, rp.work, "push", "origin", "main")
		git(t, rp.checkout, "fetch", "origin")
		f.Apa(f.WRK, "", "agents", "check", "--repo", rp.origin, "--file", filepath.Join(rp.checkout, "AGENTS.md"))
		var fix any
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if fix = f.show(other).Hosted["fix_item"]; fix != nil {
				break
			}
		}
		if fix == nil {
			t.Fatal("agents check 之后应生成修正登记事项（needs_input）")
		}
		eq(t, "修正事项阶段", f.itemShow(fix.(string))["phase"], "needs_input")
		after := decode[struct {
			Content string `json:"content"`
		}](t, f.webOK("GET", "/agents-md?repo="+rp.origin, "", http.StatusOK)).Content
		if strings.Contains(after, "docs/other.md") {
			t.Errorf("索引应随之重新渲染，不再列出已删除的路径: %s", after)
		}
	})

	t.Run("ACT-HST-04 读不到时降级显示", func(t *testing.T) {
		lost := decode[map[string]any](t, f.apa(f.WRK, 0, "material", "add", "--repo", rp.origin, "--path", "docs/never-read.md", "--kind", "design", "--project", work).Stdout)["material"].(string)
		if err := os.RemoveAll(rp.checkout); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(rp.origin, rp.origin+".gone"); err != nil {
			t.Fatal(err)
		}
		c := f.read(lost) // read 断言 HTTP 200：页面不报错
		eq(t, "状态", c.State, "unavailable")
		if c.Reason == "" || c.Repo != rp.origin || c.Path != "docs/never-read.md" {
			t.Errorf("应带原因并照常显示登记信息: %+v", c)
		}
		eq(t, "登记信息仍可读", f.show(lost).Hosted["path"], "docs/never-read.md")
	})
}

func (f *fx) itemShow(id string) map[string]any {
	f.t.Helper()
	return decode[struct {
		Item map[string]any `json:"item"`
	}](f.t, f.apa(f.OBS, 0, "item", "show", id).Stdout).Item
}
