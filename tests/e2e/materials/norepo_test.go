//go:build e2e

package materials

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const draftRule = "guideline-draft%2Fclock_rules%2Fguideline-check"

// TestFlowNoRepoGuideline 对应验收文档 flow-no-repo-guideline（ACT-NRG-01..03）：无仓库项目的规范只存在账本里。
func TestFlowNoRepoGuideline(t *testing.T) {
	f := &fx{Env: newEnv(t, ""), t: t}
	var bare struct {
		ID string `json:"id"`
	}
	bare = decode[struct {
		ID string `json:"id"`
	}](t, f.webOK("POST", "/projects", `{"name":"acc-bare"}`, 201))
	const marker = "NRGBODY-资金类事务先问本人"
	body := "## 处理规则\n\n- " + marker + "。\n"
	var gid string
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}
	guidelines := func(project string) []material {
		return decode[struct {
			Materials []material `json:"materials"`
		}](t, f.webOK("GET", "/materials?project="+project, "", 200)).Materials
	}
	projectItems := func(project string) int {
		return len(decode[struct {
			Items []struct{ ID string } `json:"items"`
		}](t, f.webOK("GET", "/items?project="+project+"&limit=500", "", 200)).Items)
	}

	step("ACT-NRG-01 写处理规则并生效", func(t *testing.T) {
		r := f.webOK("POST", "/materials", `{"project":"`+bare.ID+`","kind":"guideline","title":"acc-bare 处理规则","content":`+jsonStr(body)+`,"reason":"只写处理规则"}`, 201)
		gid = decode[struct {
			ID string `json:"material"`
		}](t, r).ID
		if gid == "" {
			t.Fatalf("创建响应没有材料 id: %s", r)
		}
		revs := f.revisions(gid)
		if len(revs) != 1 || revs[0].State != "proposed" {
			t.Fatalf("规范应先作为待确认修订: %+v", revs)
		}
		f.webOK("POST", "/revisions/"+revs[0].ID+"/confirm", "{}", 200)
		var ms []material
		for _, m := range guidelines(bare.ID) {
			if m.Kind == "guideline" {
				ms = append(ms, m)
			}
		}
		if len(ms) != 1 || ms[0].CurrentRev == "" {
			t.Fatalf("只填“处理规则”一节的规范应已生效: %+v", ms)
		}
		if len(f.events("", "guideline.activated")) == 0 {
			t.Error("事件流应有 guideline.activated")
		}
	})

	step("ACT-NRG-02 agent 经 context 获知，不生成文件", func(t *testing.T) {
		ctx := f.apa(f.OBS, 0, "context", "--scope", "project", "--project", bare.ID).Stdout
		if !strings.Contains(ctx, "acc-bare 处理规则") {
			t.Errorf("context 应列出规范标题:\n%.800s", ctx)
		}
		if strings.Contains(ctx, marker) {
			t.Errorf("context 不得带规范正文:\n%.800s", ctx)
		}
		if show := f.apa(f.OBS, 0, "material", "show", gid).Stdout; !strings.Contains(show, marker) {
			t.Errorf("material show 应含正文:\n%.600s", show)
		}
		_ = filepath.Walk(f.Root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && info.Name() == "AGENTS.md" {
				t.Errorf("无仓库项目不得渲染 AGENTS.md: %s", p)
			}
			return nil
		})
		eq(t, "渲染记录数", len(f.events("", "agents.rendered")), 0)
	})

	step("ACT-NRG-03 无仓库项目不做缺规范检测", func(t *testing.T) {
		// 没有仓库也没有规范的项目：若检测不跳过无仓库项目，它就会被起草。
		bare2 := decode[struct {
			ID string `json:"id"`
		}](t, f.webOK("POST", "/projects", `{"name":"acc-bare2"}`, 201))
		f.webOK("PATCH", "/rules/"+draftRule+"/mode", `{"mode":"auto"}`, 200)
		before := projectItems(bare.ID)
		f.TCAdvance(2 * 24 * 3600) // 越过两个检测周期（24h），内核不重启
		// 对照：有仓库而无规范的 acc-work 会被检测并起草，说明检测确实跑过。
		f.WaitFor(60*time.Second, func() bool { return projectItems(f.Projects["acc-work"]) > 0 })
		if projectItems(f.Projects["acc-work"]) == 0 {
			t.Fatal("对照项目 acc-work（有仓库无规范）应被检测并起草，检测没有运行")
		}
		eq(t, "acc-bare 的事项数", projectItems(bare.ID), before)
		eq(t, "acc-bare2（无仓库无规范）的事项数", projectItems(bare2.ID), 0)
		eq(t, "acc-bare2 的规范材料数", len(guidelines(bare2.ID)), 0)
		eq(t, "acc-bare 的规范材料数", len(guidelines(bare.ID)), 1)
		for _, e := range f.events("", "health.alert") {
			if strings.Contains(e.Object, bare.ID) || strings.Contains(e.Object, bare2.ID) {
				t.Errorf("无仓库项目不应触发缺规范检测的告警: %+v", e)
			}
		}
	})
}

func jsonStr(s string) string {
	b := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
	return `"` + b + `"`
}
