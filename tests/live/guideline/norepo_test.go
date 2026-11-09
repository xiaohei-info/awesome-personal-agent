//go:build live

package guideline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlowNoRepoGuideline 对应 flow-no-repo-guideline（ACT-NRG-01..03）。
func TestFlowNoRepoGuideline(t *testing.T) {
	f := setup(t)
	home := f.Projects["acc-home"]
	const marker = "NRGBODY-资金类事务先问本人"
	body := "## 处理规则\n\n- " + marker + "。\n"
	var gid string
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
		f.evidence[strings.SplitN(name, " ", 2)[0]] = "passed"
	}

	step("ACT-NRG-01 写处理规则并生效", func(t *testing.T) {
		r := f.webOK("POST", "/materials", `{"project":"`+home+`","kind":"guideline","title":"acc-home 处理规则","content":`+jsonStr(body)+`,"reason":"只写处理规则"}`, 201)
		gid = jsonOf[struct {
			ID string `json:"material"`
		}](t, r).ID
		if gid == "" {
			gid = jsonOf[struct {
				ID string `json:"id"`
			}](t, r).ID
		}
		f.webOK("POST", "/revisions/"+f.proposedRev(gid)+"/confirm", "{}", 200)
		ms := f.guidelines(home)
		if len(ms) != 1 || ms[0].CurrentRev == "" {
			t.Fatalf("只填“处理规则”一节的规范应已生效：%+v", ms)
		}
		if len(f.events("", "guideline.activated")) == 0 {
			t.Error("事件流应有 guideline.activated")
		}
	})

	step("ACT-NRG-02 agent 经 context 获知，不生成文件", func(t *testing.T) {
		ctx := f.apa(f.OBS, 0, "context", "--scope", "project", "--project", home).Stdout
		has(t, "context", ctx, "acc-home 处理规则")
		if strings.Contains(ctx, marker) {
			t.Errorf("context 不得带规范正文：\n%.800s", ctx)
		}
		show := f.apa(f.OBS, 0, "material", "show", gid).Stdout
		has(t, "material show", show, marker)
		_ = filepath.Walk(f.Root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && info.Name() == "AGENTS.md" {
				t.Errorf("无仓库项目不得渲染 AGENTS.md：%s", p)
			}
			return nil
		})
		eq(t, "渲染记录数", len(f.events("", "agents.rendered")), 0)
	})

	step("ACT-NRG-03 无仓库项目不做缺规范检测", func(t *testing.T) {
		f.newProject("acc-bare")
		f.webOK("PATCH", "/rules/"+draftRule+"/mode", `{"mode":"auto"}`, 200)
		time.Sleep(130 * time.Second) // 两个检测周期
		for _, name := range []string{"acc-bare", "acc-home"} {
			if its := f.projectItems(name); len(its) != 0 {
				t.Errorf("%s 不应产生起草事项：%+v", name, its)
			}
		}
		eq(t, "acc-bare 的规范材料数", len(f.guidelines(f.Projects["acc-bare"])), 0)
		for _, e := range f.events("", "health.alert") { // 只看缺规范检测相关的告警；判断型/生成型卡片文案的告警与本流程无关
			if b := mustJSON(e); strings.Contains(b, "guideline-check") || strings.Contains(b, f.Projects["acc-bare"]) || strings.Contains(b, home) {
				t.Errorf("无仓库项目不应触发缺规范检测的告警：%s", b)
			}
		}
	})
	f.flushEvidence("no-repo-guideline-evidence.json")
}
