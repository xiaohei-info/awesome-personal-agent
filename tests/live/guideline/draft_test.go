//go:build live

package guideline

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const draftRule = "guideline-draft%2Fclock_rules%2Fguideline-check"

type itemRow struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
	Parent   string `json:"parent"`
	Phase    string `json:"phase"`
}

func (f *fx) projectItems(project string) []itemRow {
	f.t.Helper()
	return jsonOf[struct {
		Items []itemRow `json:"items"`
	}](f.t, f.webOK("GET", "/items?project="+f.Projects[project]+"&limit=500", "", 200)).Items
}

func (f *fx) newProject(name string) {
	f.t.Helper()
	f.Projects[name] = jsonOf[struct {
		ID string `json:"id"`
	}](f.t, f.webOK("POST", "/projects", `{"name":"`+name+`"}`, 201)).ID
}

// TestFlowGuidelineDraft 对应 flow-guideline-draft（ACT-DRF-01..04）：任务型起草由真实 Hermes 执行。
func TestFlowGuidelineDraft(t *testing.T) {
	f := setup(t)
	host := hostname()
	remote, co := bareRepo(t, f.Root, "draftrepo", map[string]string{
		"go.mod":               "module example.com/hello\n\ngo 1.22\n",
		"hello.go":             "package main\n\nfunc main() { println(\"hello\") }\n",
		"hello_test.go":        "package main\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {}\n",
		"docs/architecture.md": "# 架构\n\n单个 main 包。\n",
	})
	remote2, _ := bareRepo(t, f.Root, "draftrepo2", map[string]string{"README.md": "# draft2\n"})
	remote3, _ := bareRepo(t, f.Root, "draftrepo3", map[string]string{"README.md": "# draft3\n"})
	for _, p := range []string{"acc-draft", "acc-draft2", "acc-draft3"} {
		f.newProject(p)
	}
	f.webOK("POST", "/projects/"+f.Projects["acc-draft"]+"/repos", `{"op":"add_remote","remote":"`+remote+`"}`, 200)
	f.webOK("POST", "/projects/"+f.Projects["acc-draft"]+"/repos", `{"op":"set_checkout","remote":"`+remote+`","host":"`+host+`","path":"`+co+`"}`, 200)
	f.webOK("POST", "/projects/"+f.Projects["acc-draft2"]+"/repos", `{"op":"add_remote","remote":"`+remote2+`"}`, 200)
	f.webOK("PUT", "/settings/projects/"+f.Projects["acc-draft2"]+"/allowed-executors", `{"executors":["work-host.cli"]}`, 200)
	f.webOK("POST", "/projects/"+f.Projects["acc-draft3"]+"/repos", `{"op":"add_remote","remote":"`+remote3+`"}`, 200)
	f.webOK("PUT", "/settings/projects/"+f.Projects["acc-draft3"]+"/allowed-executors", `{"executors":["other-host.cli"]}`, 200)

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
		f.evidence[strings.SplitN(name, " ", 2)[0]] = "passed"
	}
	var draftMat, draftRev string
	step("ACT-DRF-02 本机无检出时委派起草，受名单校验", func(t *testing.T) {
		f.webOK("PATCH", "/rules/"+draftRule+"/mode", `{"mode":"auto"}`, 200)
		defer func() {
			if f.t.Failed() {
				for _, n := range []string{"acc-draft", "acc-draft2", "acc-draft3"} {
					t.Logf("%s 事项：%.1500s", n, mustJSON(f.projectItems(n)))
				}
				t.Logf("health.alert：%.3000s", mustJSON(f.events("", "health.alert")))
				t.Logf("运行：%.1500s", f.webOK("GET", "/runs?limit=20", "", 200))
			}
		}()
		f.waitFor("acc-draft2 出现委派给 work-host.cli 的起草子事项", draftWait(), func() bool {
			for _, it := range f.projectItems("acc-draft2") {
				if it.Kind == "delegation" && it.Assignee == "work-host.cli" {
					return true
				}
			}
			return false
		})
		for _, it := range f.projectItems("acc-draft2") {
			if it.Assignee == "work-host.cli" {
				f.evidence["ACT-DRF-02-delegated"] = it
			}
		}
		ids3 := map[string]bool{}
		for _, it := range f.projectItems("acc-draft3") {
			ids3[it.ID] = true
		}
		// 名单不含 work-host.cli 的项目：不得被绕过（不出现派给它的子事项），失败要暴露。
		time.Sleep(70 * time.Second) // 至少再过一个检测周期
		for _, it := range f.projectItems("acc-draft3") {
			if it.Assignee == "work-host.cli" {
				t.Errorf("绕过了允许的执行者名单：%+v", it)
			}
		}
		// 名单校验失败要暴露为真实的拒绝：委派命令被管线拒绝，留下 command.rejected（带 acc-draft3 的起草事项）。
		var alerts []event
		for _, e := range f.events("", "command.rejected") {
			if b := mustJSON(e); (ids3[e.Object] || strings.Contains(b, f.Projects["acc-draft3"])) && strings.Contains(b, "delegate") {
				alerts = append(alerts, e)
			}
		}
		if len(alerts) == 0 {
			t.Errorf("acc-draft3 的名单校验失败应暴露为 command.rejected，事件流里没有：%.2500s", mustJSON(f.events("", "command.rejected")))
		}
		f.evidence["ACT-DRF-02-alerts"] = alerts
	})

	step("ACT-DRF-01 本机有检出时任务型起草", func(t *testing.T) {
		before := git(t, co, "rev-parse", "HEAD")
		// 规则已在 DRF-02 里由 Web 切为 auto（周期由覆盖包定为 1 分钟，默认 harness 在 setup 里已设为 Hermes）。
		defer func() {
			if f.t.Failed() {
				if keep := os.Getenv("APA_LIVE_KEEP"); keep != "" { // 排查用：留下 Hermes 家目录与运行目录
					_ = exec.Command("cp", "-a", f.hhome, f.Home+"/runs", keep).Run()
				}
				t.Logf("acc-draft 事项：%s", f.webOK("GET", "/items?project="+f.Projects["acc-draft"]+"&limit=50", "", 200))
				t.Logf("运行：%.3000s", f.webOK("GET", "/runs?limit=20", "", 200))
				for _, r := range jsonOf[struct {
					Runs []run `json:"runs"`
				}](t, f.webOK("GET", "/runs?limit=20", "", 200)).Runs {
					_ = filepath.Walk(filepath.Join(f.Home, "runs", r.ID), func(p string, info os.FileInfo, err error) error {
						if err == nil && !info.IsDir() && info.Size() < 20000 && !strings.Contains(p, "credential") {
							t.Logf("运行文件 %s：\n%.3000s", strings.TrimPrefix(p, f.Home), f.redact(readFile(t, p)))
						}
						return nil
					})
				}
				t.Logf("health.alert：%.3000s", mustJSON(f.events("", "health.alert")))
				t.Logf("规则：%.1500s", f.webOK("GET", "/rules", "", 200))
			}
		}()
		f.waitFor("任务型起草出一条 proposed 的规范修订", draftWait(), func() bool {
			for _, m := range f.guidelines(f.Projects["acc-draft"]) {
				for _, r := range f.revisions(m.ID) {
					if r.State == "proposed" {
						draftMat, draftRev = m.ID, r.ID
						return true
					}
				}
			}
			return false
		})
		var content string
		for _, r := range f.revisions(draftMat) {
			if r.ID == draftRev {
				content = r.Content
			}
		}
		if strings.TrimSpace(content) == "" {
			t.Fatal("起草的修订正文为空")
		}
		if !strings.Contains(content, "hello") && !strings.Contains(content, "go test") && !strings.Contains(content, "go.mod") {
			t.Errorf("起草内容没有引用仓库里看得到的事实：\n%.800s", content)
		}
		for _, m := range f.guidelines(f.Projects["acc-draft"]) {
			eq(t, "起草后规范的当前修订（不得自动生效）", m.CurrentRev, "")
		}
		eq(t, "没有 guideline.activated", len(f.events(draftMat, "guideline.activated")), 0)
		eq(t, "检出 HEAD 未变", git(t, co, "rev-parse", "HEAD"), before)
		f.evidence["ACT-DRF-01-draft"] = content
		f.saveEvidence("act-drf-01-draft.md", content)
	})

	step("ACT-DRF-03 确认后下一次同步写入", func(t *testing.T) {
		head := git(t, co, "rev-parse", "HEAD")
		f.webOK("POST", "/revisions/"+draftRev+"/confirm", "{}", 200)
		if len(f.events("", "guideline.activated")) == 0 {
			t.Error("确认后应有 guideline.activated")
		}
		f.wantIn(co, f.OBS, 0, "agents", "sync")
		got := readFile(t, filepath.Join(co, "AGENTS.md"))
		if !strings.Contains(got, "apa:managed") {
			t.Errorf("写入的 AGENTS.md 不是渲染产物：\n%.400s", got)
		}
		eq(t, "不产生提交", git(t, co, "rev-parse", "HEAD"), head)
		has(t, "git status", git(t, co, "status", "--porcelain"), "AGENTS.md")
		f.wantIn(co, f.OBS, 0, "agents", "check")
	})

	step("ACT-DRF-04 已有规范时不重复起草", func(t *testing.T) {
		before := len(f.projectItems("acc-draft"))
		time.Sleep(130 * time.Second) // 两个检测周期（1 分钟）
		after := f.projectItems("acc-draft")
		if len(after) != before {
			t.Errorf("acc-draft 又产生了事项：%d → %d：%+v", before, len(after), after)
		}
		eq(t, "acc-draft 的规范材料数", len(f.guidelines(f.Projects["acc-draft"])), 1)
		out := f.apa(f.OBS, 0, "material", "list", "--project", f.Projects["acc-draft"]).Stdout
		eq(t, "material list 中 guideline 的出现次数", strings.Count(out, `"kind":"guideline"`), 1)
		eq(t, "proposed 修订数", countProposed(f, draftMat), 0)
	})
	f.flushEvidence("guideline-draft-evidence.json")
	_ = os.Stdout
}

func countProposed(f *fx, material string) int {
	n := 0
	for _, r := range f.revisions(material) {
		if r.State == "proposed" {
			n++
		}
	}
	return n
}

func mustJSON(v any) string { b, _ := jsonMarshal(v); return string(b) }

// draftWait 是 Hermes 起草的等待上限；APA_LIVE_DRAFT_WAIT 可在排查时缩短。
func draftWait() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("APA_LIVE_DRAFT_WAIT")); err == nil {
		return d
	}
	return 20 * time.Minute
}

type run struct {
	ID string `json:"id"`
}
