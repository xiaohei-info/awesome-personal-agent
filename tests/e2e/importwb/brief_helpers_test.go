//go:build e2e

package importwb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ---- 简报取材：默认 Agent 是读资料文件并写回简报的 STUB_HARNESS（与 tests/e2e/briefwb 同一接法） ----

const briefHarness = "acc-brief-stub"

// declareBriefHarness 声明 acc-brief-stub：包装脚本按模式文件选 STUB_HARNESS 的 brief 模式
// （运行子进程不继承内核环境，开关只能走脚本与文件），并设为 Web 里的默认 Agent。
func (f *fx) declareBriefHarness() {
	f.t.Helper()
	f.mode = filepath.Join(f.Root, "brief-mode")
	writeFile(f.t, f.mode, "brief")
	wrapper := filepath.Join(f.Root, "acc-brief-harness.sh")
	script := fmt.Sprintf("#!/bin/sh\nexport STUB_HARNESS_MODE=$(cat %q)\nexport STUB_HARNESS_APA=%q\nexec %q \"$@\"\n", f.mode, f.Bin["apa"], f.Bin["stub-harness"])
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	decl, _ := json.Marshal(map[string]any{"harness": briefHarness, "headless": map[string]any{
		"command": []string{wrapper, "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "验收：简报替身声明"); r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.web("POST", "/v1/settings/declarations/"+briefHarness+"/1/confirm", "", 200)
	f.web("PUT", "/v1/settings/policy", `{"default_harness":"`+briefHarness+`"}`, 200)
	// 出厂的默认简报实例（工作日 08:30）停用，免得测试恰好跑在到期时刻。
	for _, in := range decode[struct {
		Instances []struct {
			ID string `json:"id"`
		} `json:"instances"`
	}](f.t, f.apa(f.OBS, 0, "instance", "list").Stdout).Instances {
		f.apa(f.DEL, 0, "instance", "disable", in.ID, "--evidence", "验收：停用出厂实例")
	}
}

type briefItem struct {
	ID     string `json:"id"`
	Phase  string `json:"phase"`
	Origin *struct {
		Source string `json:"source"`
	} `json:"origin"`
}

func (f *fx) briefs() []briefItem {
	f.t.Helper()
	return decode[struct {
		Items []briefItem `json:"items"`
	}](f.t, f.apa(f.OBS, 0, "item", "list", "--kind", "brief").Stdout).Items
}

// addBriefInstance 建一个简报实例（schedule 取永不到期的年度时刻，只靠"立即跑一次"触发）。
func (f *fx) addBriefInstance() string {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "instance", "add", "brief", "--param", "schedule=0 3 1 1 *", "--evidence", "验收实例")
	return decode[struct {
		ID string `json:"id"`
	}](f.t, r.Stdout).ID
}

// runBrief 对实例点"立即跑一次"（Web 简报页调用的 POST /instances/{id}/run），等简报事项离开 open，
// 返回 STUB_HARNESS 保存下来的资料文件里各段的 JSON（段名 → JSON）。
func (f *fx) runBrief(inst string) map[string]string {
	f.t.Helper()
	known := map[string]bool{}
	for _, b := range f.briefs() {
		known[b.ID] = true
	}
	f.TCAdvance(120) // 每次到期时刻不同：同一周期只一份（幂等键取实例与到期时刻）
	f.offset += 120
	f.web("POST", "/v1/instances/"+inst+"/run", "", 202)
	f.until("实例 "+inst+" 的简报到达 done", func() bool {
		for _, b := range f.briefs() {
			if !known[b.ID] && b.Origin != nil && b.Origin.Source == "instance:"+inst && b.Phase == "done" {
				return true
			}
		}
		return false
	})
	mats := map[string]string{}
	f.until("STUB_HARNESS 保存了资料文件", func() bool {
		dirs, _ := filepath.Glob(filepath.Join(f.Home, "runs", "*", "stub-harness-saved", "materials.json"))
		for _, d := range dirs {
			if f.seen[d] {
				continue
			}
			raw, err := os.ReadFile(d)
			if err != nil {
				continue
			}
			var m struct {
				Materials []struct{ Name, Body string }
			}
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			f.seen[d] = true
			for _, x := range m.Materials {
				mats[x.Name] = x.Body
			}
			if len(mats) > 0 {
				return true
			}
		}
		return false
	})
	return mats
}
