//go:build live

package brief

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlowBrief 对应验收文档 flow-brief，按验收顺序执行；前一步未通过，后续不得开始。
// 目前只有 ACT-BRF-01：它在当前 main 上不通过（见 PR/issue 说明），ACT-BRF-02..06 依赖它产出的简报，因此尚未编写。
func TestFlowBrief(t *testing.T) {
	f := setup(t)
	defer f.flushEvidence()
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		parent := f.t
		ok := t.Run(name, func(t *testing.T) {
			f.t, f.Env.T = t, t
			defer func() { f.t, f.Env.T = parent, parent }()
			fn(t)
		})
		f.evidence[strings.Fields(name)[0]+"-result"] = map[string]any{"step": name, "passed": ok}
		if !ok {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}

	step("ACT-BRF-01 到期生成简报", func(t *testing.T) {
		for _, p := range []string{"acc-work", "acc-open"} { // 账本里有可分节的素材
			f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "BRF 示例任务 "+p, "--phase", "open", "--project", f.Projects[p], "--evidence", "简报素材")
		}
		f.apa(f.DEL, 0, "instance", "add", "brief", "--param", "schedule=* * * * *", "--evidence", "每分钟出一份简报")
		f.waitFor("到期后出现 brief 事项", 3*time.Minute, func() bool { return len(f.briefs()) > 0 })
		b := f.briefs()[0]
		r := f.finishedRun(b.ID, 15*time.Minute)
		if dir := filepath.Join(f.Home, "runs", r.ID); true {
			out, _ := os.ReadFile(filepath.Join(dir, "output.txt"))
			f.saveEvidence("act-brf-01-run-output.txt", string(out))
		}
		eq(t, "Run 类别", r.Category, "task")
		eq(t, "Run 状态", r.State, "succeeded")
		eq(t, "Run 的 harness", r.Harness, harness)
		got := f.briefs()
		var done briefItem
		for _, x := range got {
			if x.ID == b.ID {
				done = x
			}
		}
		period, _ := done.Spec["period"].(string)
		sections, _ := done.Spec["sections"].([]any)
		f.evidence["ACT-BRF-01"] = map[string]any{"brief": b.ID, "run": r.ID, "run_state": r.State, "period": period, "sections": len(sections), "phase": done.Phase}
		if period == "" {
			t.Errorf("spec.period 为空：任务型运行没有把本期标识写回简报事项 %s", b.ID)
		}
		if len(sections) == 0 {
			t.Errorf("spec.sections 为空：简报没有按项目分节（运行状态 %s）", r.State)
		}
		for _, s := range sections {
			m, _ := s.(map[string]any)
			if p, _ := m["project"].(string); p == "" {
				t.Errorf("分节缺少 project: %v", m)
			}
			if body, _ := m["body"].(string); strings.TrimSpace(body) == "" {
				t.Errorf("分节缺少 body: %v", m)
			}
		}
		// Web 简报页读的就是这份列表（GET /v1/items?kind=brief）。
		if web := f.webOK("GET", "/items?kind=brief", "", 200); !strings.Contains(web, b.ID) {
			t.Errorf("Web 简报列表里没有 %s", b.ID)
		}
	})
}
