//go:build e2e

package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/packs"
)

const draftRule = "guideline-draft%2Fclock_rules%2Fguideline-check"

type projItem struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Phase    string `json:"phase"`
	Assignee string `json:"assignee"`
	Title    string `json:"title"`
}

func (f *fx) projectItems(project string) []projItem {
	f.t.Helper()
	return decode[struct{ Items []projItem }](f.t, f.webOK("GET", "/items?project="+f.Projects[project]+"&limit=200", "", 200)).Items
}

type gmaterial struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	CurrentRev string `json:"current_rev"`
}

type grevision struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Content string `json:"content"`
}

func (f *fx) guidelines(project string) []gmaterial {
	f.t.Helper()
	var out []gmaterial
	for _, m := range decode[struct{ Materials []gmaterial }](f.t, f.webOK("GET", "/materials?project="+f.Projects[project], "", 200)).Materials {
		if m.Kind == "guideline" {
			out = append(out, m)
		}
	}
	return out
}

func (f *fx) grevisions(material string) []grevision {
	f.t.Helper()
	return decode[struct{ Revisions []grevision }](f.t, f.webOK("GET", "/materials/"+material+"/revisions", "", 200)).Revisions
}

// shortDraftCycle 用数据目录下的同名包整体覆盖内置 guideline-draft 包，把检测周期从 24h 改成 1 分钟（附录 C.1）；
// 之后重启内核使其生效，周期的"到期"由测试时钟推进。
func (f *fx) shortDraftCycle() {
	f.t.Helper()
	yml, err := packs.FS.ReadFile("guideline-draft/pack.yaml")
	if err != nil {
		f.t.Fatal(err)
	}
	if !strings.Contains(string(yml), "schedule: 24h") {
		f.t.Fatal("guideline-draft 内置包的检测周期不是 24h，覆盖包需要同步")
	}
	tmpl, err := packs.FS.ReadFile("guideline-draft/prompts/draft-guideline.md")
	if err != nil {
		f.t.Fatal(err)
	}
	pk := filepath.Join(f.Home, "packs", "guideline-draft")
	writeFile(f.t, pk, "pack.yaml", strings.Replace(string(yml), "schedule: 24h", "schedule: 1m", 1))
	writeFile(f.t, pk, "prompts/draft-guideline.md", string(tmpl))
	f.StopKernel()
	f.StartKernelAgain()
}

// TestFlowGuidelineDraft 对应验收文档 flow-guideline-draft（ACT-DRF-01..04，替身版）：
// 起草的任务型运行由 STUB_HARNESS 脚本代替真实宿主，用运行凭据 `apa material commit --kind guideline` 提交。
func TestFlowGuidelineDraft(t *testing.T) {
	f := setup(t, "guideline_draft_executor: work-host.cli\n")
	f.shortDraftCycle()

	// DRAFT_REPO：git init 的本地仓库，并有一份本机检出（origin 就是它）。
	root := t.TempDir()
	repoDir, co := filepath.Join(root, "draft-repo"), filepath.Join(root, "draft-co")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repoDir, "init", "-q")
	writeFile(t, repoDir, "go.mod", "module example.com/hello\n\ngo 1.22\n")
	writeFile(t, repoDir, "hello_test.go", "package main\n")
	git(t, repoDir, "add", ".")
	git(t, repoDir, "commit", "-q", "-m", "init")
	git(t, root, "clone", "-q", "file://"+repoDir, co)
	host, _ := os.Hostname()

	project := func(name string, repos []string, executors string) {
		var out struct{ ID string }
		out = decode[struct{ ID string }](t, f.webOK("POST", "/projects", `{"name":"`+name+`"}`, 201))
		f.Projects[name] = out.ID
		for _, r := range repos {
			f.webOK("POST", "/projects/"+out.ID+"/repos", `{"op":"add_remote","remote":"`+r+`"}`, 200)
		}
		if executors != "" {
			f.webOK("PUT", "/settings/projects/"+out.ID+"/allowed-executors", `{"executors":[`+executors+`]}`, 200)
		}
	}
	project("acc-draft", []string{"file://" + repoDir}, "")
	f.webOK("POST", "/projects/"+f.Projects["acc-draft"]+"/repos", `{"op":"set_checkout","remote":"file://`+repoDir+`","host":"`+host+`","path":"`+co+`"}`, 200)
	project("acc-draft2", []string{"https://example.invalid/acc/draft2.git"}, `"work-host.cli"`)
	project("acc-draft3", []string{"https://example.invalid/acc/draft3.git"}, `"other-host.cli"`)

	// STUB_HARNESS 脚本：用运行凭据提交规范草稿（回执命令由包装脚本传入目标事项 id）。
	script := fmt.Sprintf(`#!/bin/sh
set -e
proj=$(%[1]q item show "$1" | python3 -c 'import sys,json;print(json.load(sys.stdin)["item"]["project"])')
cat > draft.md <<'MD'
## 约定

- 模块是 example.com/hello，构建用 go.mod 描述。

## 验证方法

- 运行 go test ./...（仓库里有 hello_test.go）。
MD
exec %[1]q material commit --kind guideline --title "acc-draft 规范" --project "$proj" --from draft.md --reason 起草
`, f.Bin["apa"])
	if err := os.WriteFile(filepath.Join(f.Root, "acc-receipt.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mode("receipt")
	f.webOK("PATCH", "/rules/"+draftRule+"/mode", `{"mode":"auto"}`, 200)
	head := git(t, co, "rev-parse", "HEAD")

	var mat, rev string
	t.Run("ACT-DRF-01/02 一个检测周期：有检出任务型起草，无检出委派起草并受名单校验", func(t *testing.T) {
		f.TCAdvance(61)
		f.waitFor("任务型起草出一条 proposed 的规范修订", 60*time.Second, func() bool {
			for _, m := range f.guidelines("acc-draft") {
				for _, r := range f.grevisions(m.ID) {
					if r.State == "proposed" {
						mat, rev = m.ID, r.ID
						return true
					}
				}
			}
			return false
		})
		for _, m := range f.guidelines("acc-draft") {
			eq(t, "起草后规范的当前修订（不得自动生效）", m.CurrentRev, "")
		}
		eq(t, "没有 guideline.activated", len(f.events(mat, "guideline.activated")), 0)
		var run *run
		for _, r := range f.taskRuns() {
			if r.Harness == harness {
				r := r
				run = &r
			}
		}
		if run == nil {
			t.Fatalf("起草应拉起任务型 Run: %+v", f.runs())
		}
		f.waitFor("起草运行成功收尾", 30*time.Second, func() bool {
			for _, r := range f.taskRuns() {
				if r.State == "succeeded" {
					return true
				}
			}
			return false
		})

		// DRF-02：无本机检出 → 委派子事项给 work-host.cli。
		f.waitFor("acc-draft2 出现委派给 work-host.cli 的起草子事项", 60*time.Second, func() bool {
			for _, it := range f.projectItems("acc-draft2") {
				if it.Kind == "delegation" && it.Assignee == "work-host.cli" {
					return true
				}
			}
			return false
		})
		// 名单不含 work-host.cli 的项目：不得被绕过，失败暴露为 command.rejected。
		f.waitFor("acc-draft3 的名单校验失败暴露为 command.rejected", 60*time.Second, func() bool {
			ids := map[string]bool{}
			for _, it := range f.projectItems("acc-draft3") {
				ids[it.ID] = true
			}
			for _, e := range f.events("", "command.rejected") {
				if ids[e.Object] && e.Payload["action"] == "delegate" && e.Payload["code"] == "executor_not_allowed" {
					return true
				}
			}
			return false
		})
		for _, it := range f.projectItems("acc-draft3") {
			if it.Assignee == "work-host.cli" {
				t.Errorf("绕过了允许的执行者名单：%+v", it)
			}
		}
	})

	t.Run("ACT-DRF-03 确认后下一次同步写入", func(t *testing.T) {
		f.webOK("POST", "/revisions/"+rev+"/confirm", "{}", 200)
		if len(f.events("", "guideline.activated")) == 0 {
			t.Error("确认后应有 guideline.activated")
		}
		code, out, errs := f.apaEnv(f.OBS, co, nil, "agents", "sync")
		if code != 0 {
			t.Fatalf("apa agents sync 退出码 %d: %s%s", code, out, errs)
		}
		contains(t, "写入的 AGENTS.md", readFile(t, filepath.Join(co, "AGENTS.md")), "apa:managed", "go test ./...")
		eq(t, "不产生提交", git(t, co, "rev-parse", "HEAD"), head)
		contains(t, "git status", git(t, co, "status", "--porcelain"), "AGENTS.md")
		if code, out, errs := f.apaEnv(f.OBS, co, nil, "agents", "check"); code != 0 {
			t.Fatalf("sync 后 check 退出码 %d: %s%s", code, out, errs)
		}
	})

	t.Run("ACT-DRF-04 已有规范时不重复起草", func(t *testing.T) {
		before := len(f.projectItems("acc-draft"))
		runs := len(f.runs())
		f.TCAdvance(61)
		time.Sleep(3 * time.Second)
		f.TCAdvance(61)
		time.Sleep(3 * time.Second)
		eq(t, "acc-draft 的事项数", len(f.projectItems("acc-draft")), before)
		eq(t, "没有新的运行", len(f.runs()), runs)
		eq(t, "acc-draft 的规范材料数", len(f.guidelines("acc-draft")), 1)
		eq(t, "material list 中 guideline 的出现次数",
			strings.Count(f.apa(f.OBS, 0, "material", "list", "--project", f.Projects["acc-draft"]).Stdout, `"kind":"guideline"`), 1)
		proposed := 0
		for _, r := range f.grevisions(mat) {
			if r.State == "proposed" {
				proposed++
			}
		}
		eq(t, "proposed 修订数", proposed, 0)
	})
}
