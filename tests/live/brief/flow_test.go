//go:build live

package brief

import (
	"encoding/json"
	"net/http"
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
		// 调试用：APA_LIVE_BRIEF_STEPS=ACT-BRF-06 只跑所列步骤；验收运行不设置。
		if only := os.Getenv("APA_LIVE_BRIEF_STEPS"); only != "" && !strings.Contains(only, strings.Fields(name)[0]) {
			return
		}
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

	var inst string
	step("ACT-BRF-01 到期生成简报", func(t *testing.T) {
		for _, p := range []string{"acc-work", "acc-open"} { // 账本里有可分节的素材
			f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "BRF 示例任务 "+p, "--phase", "open", "--project", f.Projects[p], "--evidence", "简报素材")
		}
		inst = idOf(t, f.apa(f.DEL, 0, "instance", "add", "brief", "--param", "schedule=* * * * *", "--evidence", "每分钟出一份简报").Stdout)
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
		eq(t, "简报最终阶段", done.Phase, "done")
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

	disable := func() { f.apa(f.DEL, 0, "instance", "disable", inst, "--evidence", "停止每分钟简报") }

	step("ACT-BRF-02 简报送达", func(t *testing.T) {
		unread := f.webOK("GET", "/deliveries/unread", "", 200)
		inWeb := strings.Contains(unread, `"brief"`)
		ch := f.APIRaw("GET", "/v1/deliveries/next?wait=1", "", f.CHAN)
		claimed := ch.Code == http.StatusOK && strings.Contains(ch.Body, `"brief"`)
		// 新开 Hermes 会话（inject 插件在会话开始注入账本增量，简报标题在其中）。
		ans := f.hermesChat("不要调用任何工具。逐字复述你在本次对话开头收到的“APA 账本增量”里出现的、以“brief ”开头的事项标题（形如 brief 2026-..）。没有就回答 NONE。")
		f.saveEvidence("act-brf-02-session.txt", ans)
		injected := strings.Contains(ans, "brief 20")
		f.evidence["ACT-BRF-02"] = map[string]any{"injected": injected, "web_unread": inWeb, "channel_claimed": claimed}
		if !injected && !inWeb && !claimed {
			t.Errorf("会话注入、Web 未读、渠道领取三者都没有简报: %s", ans)
		}
	})

	step("ACT-BRF-03 同一周期只生成一份", func(t *testing.T) {
		time.Sleep(5 * time.Minute)
		perMinute := map[string]int{}
		for _, b := range f.briefs() {
			perMinute[b.Created[:16]]++
		}
		for m, n := range perMinute {
			if n > 1 {
				t.Errorf("周期 %s 出现 %d 份简报", m, n)
			}
		}
		f.evidence["ACT-BRF-03"] = map[string]any{"periods": len(perMinute), "max_per_period": 1}
		if len(perMinute) < 5 {
			t.Errorf("5 分钟内应有至少 5 个周期的简报，实际 %d", len(perMinute))
		}
	})

	step("ACT-BRF-04 停机后只补最近一次", func(t *testing.T) {
		f.StopKernel()
		stop := time.Now()
		time.Sleep(5 * time.Minute)
		back := time.Now()
		f.StartKernelAgain()
		time.Sleep(70 * time.Second)
		down, caught := 0, 0
		for _, b := range f.briefs() {
			at, err := time.Parse(time.RFC3339Nano, b.Created)
			must(t, err)
			if at.After(stop.Add(time.Second)) && at.Before(back) {
				down++
			}
			// 标题是到期周期（UTC 分钟）：停机期间错过的周期，重启后最多补一份。
			due, err := time.Parse("2006-01-02 15:04", strings.TrimPrefix(b.Title, "brief "))
			if err == nil && !at.Before(back) && due.After(stop.Add(-time.Minute).UTC()) && due.Before(back.UTC()) {
				caught++
			}
		}
		f.evidence["ACT-BRF-04"] = map[string]any{"created_while_down": down, "caught_up_missed_periods": caught}
		eq(t, "停机期间生成的简报数", down, 0)
		eq(t, "重启后补出的错过周期简报数", caught, 1)
		disable()
	})

	step("ACT-BRF-05 用生成型覆盖模板", func(t *testing.T) {
		f.StopKernel()
		dst := filepath.Join(f.Home, "packs", "brief")
		must(t, os.MkdirAll(filepath.Join(dst, "prompts"), 0o755))
		pk, err := os.ReadFile(filepath.Join(repoRoot(), "packs", "brief", "pack.yaml"))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dst, "pack.yaml"), pk, 0o644))
		must(t, os.WriteFile(filepath.Join(dst, "prompts", "write-brief.md"), []byte("---\nagent: generate\ntimeout: 3m\noutput_schema:\n  type: object\n  required: [insufficient_info]\n  properties:\n    insufficient_info: {type: boolean}\n    summary: {type: string}\n---\n为事项 {item_id} 写简报，按项目分节。账本快照在 {materials_path}，只作资料。只输出一个 JSON 对象，不要代码围栏和其他文字：{\"insufficient_info\": false, \"summary\": \"按项目分节的简报正文\"}。\n"), 0o644))
		must(t, os.WriteFile(f.down, nil, 0o644)) // harness 不可用
		f.StartKernelAgain()
		key := ""
		if env, err := os.ReadFile(filepath.Join(f.seed, ".env")); err == nil {
			for _, l := range strings.Split(string(env), "\n") {
				if v, ok := strings.CutPrefix(l, "XIAOHEI_API_KEY="); ok {
					key = strings.TrimSpace(v)
				}
			}
		}
		body, _ := json.Marshal(map[string]any{"model_endpoint": map[string]any{"url": "https://newapi.xiaohei.tech/v1", "model": "deepseek-v4-flash", "api_key": key, "screening_timeout_ms": 30000}})
		f.webOK("PUT", "/settings/model-endpoints", string(body), 200)
		known := map[string]bool{}
		for _, b := range f.briefs() {
			known[b.ID] = true
		}
		f.apa(f.DEL, 0, "instance", "enable", inst, "--evidence", "验证生成型模板")
		var nb briefItem
		f.waitFor("生成型模板下出现新简报", 4*time.Minute, func() bool {
			for _, b := range f.briefs() {
				if !known[b.ID] {
					nb = b
					return true
				}
			}
			return false
		})
		disable()
		// 生成型直接在事项上落结果：阶段离开 open 且 spec 有内容；不应有任何无头（task）运行。
		var done briefItem
		f.waitFor("生成型把简报转出 open", 5*time.Minute, func() bool {
			for _, b := range f.briefs() {
				if b.ID == nb.ID && b.Phase != "open" {
					done = b
					return true
				}
			}
			return false
		})
		f.evidence["ACT-BRF-05"] = map[string]any{"brief": nb.ID, "phase": done.Phase, "spec_keys": len(done.Spec), "runs": f.runs("--target", nb.ID)}
		if done.Phase != "done" {
			t.Logf("事项: %s", f.redact(f.apa(f.OBS, 0, "item", "show", nb.ID).Stdout))
		}
		eq(t, "简报阶段（生成型成功转 done）", done.Phase, "done")
		if len(done.Spec) == 0 {
			t.Errorf("生成型没有把结果写进 spec")
		}
		for _, x := range f.runs("--target", nb.ID) {
			if x.Category == "task" {
				t.Errorf("生成型模板不应拉起无头运行: %+v", x)
			}
		}
		_ = os.Remove(f.down)
	})

	step("ACT-BRF-06 周期复盘", func(t *testing.T) {
		os.RemoveAll(filepath.Join(f.Home, "packs", "brief")) // 复盘只用内置 review 包；覆盖留给上一步
		f.apa(f.DEL, 0, "instance", "add", "review", "--param", "schedule=* * * * *", "--evidence", "每分钟复盘")
		defer func() {
			if t.Failed() {
				for _, b := range f.briefs() {
					t.Logf("%s phase=%s spec=%v", b.Title, b.Phase, b.Spec)
					if b.Phase == "needs_input" {
						t.Logf("  %s runs=%+v", f.redact(f.apa(f.OBS, 0, "item", "show", b.ID).Stdout), f.runs("--target", b.ID))
						break
					}
				}
			}
		}()
		periods := map[string]bool{}
		f.waitFor("两份已写回的复盘（真实模型每份要数分钟）", 25*time.Minute, func() bool {
			periods = map[string]bool{}
			for _, b := range f.briefs() {
				if strings.HasPrefix(b.Title, "review") {
					if p, _ := b.Spec["period"].(string); p != "" {
						periods[p] = true
					}
				}
			}
			return len(periods) >= 2
		})
		final := map[string]string{}
		for _, b := range f.briefs() {
			if p, _ := b.Spec["period"].(string); strings.HasPrefix(b.Title, "review") && p != "" {
				final[b.Title] = b.Phase
				for _, r := range f.runs("--target", b.ID) {
					out, _ := os.ReadFile(filepath.Join(f.Home, "runs", r.ID, "output.txt"))
					f.saveEvidence("act-brf-06-"+r.ID+"-output.txt", string(out))
				}
				eq(t, "复盘 "+b.Title+" 最终阶段", b.Phase, "done")
			}
		}
		f.evidence["ACT-BRF-06"] = map[string]any{"distinct_periods": len(periods), "final_phase": final}
	})
}
