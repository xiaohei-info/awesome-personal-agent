//go:build e2e

package agents

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type briefItem struct {
	ID      string         `json:"id"`
	Title   string         `json:"title"`
	Phase   string         `json:"phase"`
	Created string         `json:"created_at"`
	Spec    map[string]any `json:"spec"`
}

func (f *fx) briefs(titlePrefix string) []briefItem {
	f.t.Helper()
	var out []briefItem
	for _, b := range decode[struct{ Items []briefItem }](f.t, f.apa(f.DEL, 0, "item", "list", "--kind", "brief").Stdout).Items {
		if strings.HasPrefix(b.Title, titlePrefix) {
			out = append(out, b)
		}
	}
	return out
}

// briefScript 是简报的 STUB_HARNESS 脚本：按账本里的事项逐项目分节，用运行凭据把正文写回目标事项（写回就是回执）。
// 周期取目标事项标题里的到期时刻（"brief 2026-11-09 10:50"）。
func (f *fx) briefScript() {
	f.t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -e
APA=%q
"$APA" item show "$1" > show.json
"$APA" item list > items.json
"$APA" project list > projects.json
spec=$(python3 - <<'PY'
import json
title = json.load(open("show.json"))["item"]["title"]
names = {p["id"]: p["name"] for p in json.load(open("projects.json"))["projects"]}
by = {}
for it in json.load(open("items.json"))["items"]:
    if it.get("project") and it.get("kind") != "brief":
        by.setdefault(names.get(it["project"], it["project"]), []).append(it["title"])
sections = [{"project": n, "body": "进展：" + "；".join(ts) + "。到期事项：无。待本人决定：无。"} for n, ts in sorted(by.items())]
print(json.dumps({"period": title.split(" ", 1)[1], "sections": sections}, ensure_ascii=False))
PY
)
exec "$APA" item update "$1" --spec "$spec" --reason 写回简报
`, f.Bin["apa"])
	if err := os.WriteFile(filepath.Join(f.Root, "acc-receipt.sh"), []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.mode("receipt")
}

// TestFlowBrief 对应验收文档 flow-brief（ACT-BRF-01..06，替身版）：简报由 STUB_HARNESS 脚本经运行凭据写回，
// "等待几分钟"由测试时钟推进代替（TC_ADVANCE）。ACT-BRF-05 见 TestBriefGenerateOverrideLeavesOpen（生成型覆盖，无 harness）。
func TestFlowBrief(t *testing.T) {
	f := setup(t, "")
	f.briefScript()
	work, open := f.Projects["acc-work"], f.Projects["acc-open"]
	for name, p := range map[string]string{"BRF 示例任务 acc-work": work, "BRF 示例任务 acc-open": open} { // 账本里有可分节的素材
		f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", name, "--phase", "open", "--project", p, "--evidence", "简报素材")
	}
	// 周期推进一格并等该周期的简报离开 open（Run 已写回）。
	tick := func(prefix string, want int) {
		t.Helper()
		f.TCAdvance(61)
		f.waitFor(fmt.Sprintf("%s 累计 %d 份且写回完成", prefix, want), 60*time.Second, func() bool {
			bs := f.briefs(prefix)
			if len(bs) < want {
				return false
			}
			for _, b := range bs {
				if b.Phase == "open" {
					return false
				}
			}
			return true
		})
	}
	var inst string

	t.Run("ACT-BRF-01 到期生成简报，按项目分节", func(t *testing.T) {
		inst = idOf(t, f.apa(f.DEL, 0, "instance", "add", "brief", "--param", "schedule=* * * * *", "--evidence", "每分钟出一份简报").Stdout)
		tick("brief ", 1)
		b := f.briefs("brief ")[0]
		eq(t, "简报最终阶段", b.Phase, "done")
		eq(t, "spec.period 取自到期时刻", b.Spec["period"], strings.TrimPrefix(b.Title, "brief "))
		secs, _ := b.Spec["sections"].([]any)
		projects := map[string]bool{}
		for _, s := range secs {
			m := s.(map[string]any)
			p, _ := m["project"].(string)
			body, _ := m["body"].(string)
			if p == "" || strings.TrimSpace(body) == "" {
				t.Errorf("分节缺 project 或 body: %v", m)
			}
			projects[p] = true
		}
		if !projects["acc-work"] || !projects["acc-open"] {
			t.Errorf("正文应按项目分节（acc-work、acc-open）: %v", secs)
		}
		// 事项先建好，随后由 task 型 Run（STUB_HARNESS）写回。
		var found bool
		for _, r := range f.taskRuns() {
			if r.Target == b.ID {
				found = true
				eq(t, "Run 状态", r.State, "succeeded")
				eq(t, "Run 的 harness", r.Harness, harness)
			}
		}
		if !found {
			t.Errorf("没有以简报 %s 为目标的任务型 Run: %+v", b.ID, f.taskRuns())
		}
		contains(t, "Web 简报页", f.webOK("GET", "/items?kind=brief", "", http.StatusOK), b.ID)
	})

	t.Run("ACT-BRF-02 简报送达：会话注入、Web 未读、渠道领取", func(t *testing.T) {
		b := f.briefs("brief ")[0]
		// 简报是账本里的 brief 事项，不入投递队列：送达靠会话注入（事项增量）和 Web 简报页；
		// Web 未读与渠道领取只承载投递队列里的条目，这里确认两个入口照常可用且不重复投递简报。
		unread := f.webOK("GET", "/deliveries/unread", "", http.StatusOK)
		lacks(t, "简报不入投递队列", unread, b.ID)
		code, out, errs := f.apaEnv(f.DEL, "", []string{"APA_SESSION=acc-session-9", "APA_SESSION_KIND=direct"}, "context", "--format", "inject")
		if code != 0 {
			t.Fatalf("apa context 退出码 %d: %s%s", code, out, errs)
		}
		contains(t, "会话开始注入带上简报摘要", out, b.Title)
		if r := f.APIRaw("GET", "/v1/deliveries/next?wait=1", "", f.CHAN); r.Code != http.StatusOK && r.Code != http.StatusNoContent {
			t.Errorf("CHAN_EXT 领取应返回 200/204: %d %s", r.Code, r.Body)
		}
	})

	t.Run("ACT-BRF-03 同一周期只生成一份", func(t *testing.T) {
		for i := 2; i <= 5; i++ { // 保持每分钟的实例运行 5 个周期
			tick("brief ", i)
		}
		per := map[string]int{}
		for _, b := range f.briefs("brief ") {
			per[b.Title]++
		}
		for p, n := range per {
			if n > 1 {
				t.Errorf("周期 %s 出现 %d 份简报", p, n)
			}
		}
		if len(per) < 5 {
			t.Errorf("5 个周期应至少 5 份简报，实际 %d", len(per))
		}
	})

	t.Run("ACT-BRF-04 停机后只补最近一次", func(t *testing.T) {
		before := len(f.briefs("brief "))
		f.StopKernel()
		f.TCAdvance(5 * 60) // 停机 5 分钟
		f.StartKernelAgain()
		f.waitFor("重启后补出简报", 60*time.Second, func() bool { return len(f.briefs("brief ")) > before })
		time.Sleep(3 * time.Second) // 给多补的第二份留出出现的时间
		eq(t, "错过 5 个周期只补一份", len(f.briefs("brief "))-before, 1)
		f.apa(f.DEL, 0, "instance", "disable", inst, "--evidence", "停止每分钟简报")
	})

	t.Run("ACT-BRF-06 周期复盘，由 spec.period 区分周期", func(t *testing.T) {
		f.apa(f.DEL, 0, "instance", "add", "review", "--param", "schedule=* * * * *", "--evidence", "每分钟复盘")
		tick("review ", 1)
		tick("review ", 2)
		periods := map[string]bool{}
		for _, b := range f.briefs("review ") {
			p, _ := b.Spec["period"].(string)
			if p == "" {
				t.Errorf("复盘 %s 没有 spec.period", b.ID)
			}
			periods[p] = true
			eq(t, "复盘最终阶段", b.Phase, "done")
		}
		eq(t, "按 spec.period 区分的周期数", len(periods), 2)
	})
}
