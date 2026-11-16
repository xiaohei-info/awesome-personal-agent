//go:build e2e

package linkmaterial

import (
	"slices"
	"strings"
	"testing"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// TestFlowReportLinks 对应验收 flow-report-links（§7.5）：done 回执里落在已登记笔记库内的链接，由内核登记为链接型材料
// 并以 output 关联到委派的父事项；库外链接、未登记的库与 progress、blocked 回执不登记。回执由 STUB_HARNESS 替身发出。
func TestFlowReportLinks(t *testing.T) {
	f := setup(t)
	proj := f.Projects["acc-open"]

	// 先手工登记其中一个位置：自动登记要复用它，而不是再建一条。
	manual := f.addLink(0, proj, "vault:notes/out/r.md", "手工登记的 R").Material

	parent, child := f.delegated("RL-1", "acc-open")
	t.Run("done 回执带多个链接：只有库内的登记并关联为产出，已登记的位置复用", func(t *testing.T) {
		f.harnessReport(child, "done", "vault:notes/out/r.md", "vault:notes/out/new.md", "vault:other/x.md", "https://example.com/pr/1")
		if p := f.phase(child); p != "done" {
			t.Fatalf("子事项应 done，实际 %s", p)
		}
		fresh, ok := f.byLink(proj, "vault:notes/out/new.md")
		if !ok || fresh.Title != "new.md" || fresh.Kind != "reference" || fresh.Form != "link" {
			t.Fatalf("库内新链接应登记为标题 new.md、kind reference 的链接型材料: %+v ok=%v", fresh, ok)
		}
		outs := f.outputs(parent)
		if len(outs) != 2 || !slices.Contains(outs, manual) || !slices.Contains(outs, fresh.ID) {
			t.Fatalf("父事项的产出 = %v，期望 {%s（复用）, %s（新登记）}", outs, manual, fresh.ID)
		}
		if n := len(f.materials(proj)); n != 2 {
			t.Errorf("项目材料应恰为 2 条（库外网址与未登记的库不登记），实际 %d", n)
		}
		if _, ok := f.byLink(proj, "vault:other/x.md"); ok {
			t.Error("未登记的库里的链接不应被登记")
		}
		if _, ok := f.byLink(proj, "https://example.com/pr/1"); ok {
			t.Error("普通网址不应被登记")
		}
	})

	t.Run("操作者是 system，依据记内置规则", func(t *testing.T) {
		body := f.webOK("GET", "/audit/events?limit=500&type=material.revised", "", 200)
		evs := decode[struct {
			Events []struct {
				Actor struct{ Type string } `json:"actor"`
				Basis struct {
					RuleID string `json:"rule_id"`
				} `json:"basis"`
				Reason string `json:"reason"`
			} `json:"events"`
		}](t, body).Events
		found := false
		for _, e := range evs {
			if contains(e.Reason, "vault:notes/out/new.md") {
				found = true
				if e.Actor.Type != "system" || e.Basis.RuleID != "delegate.report.vault_links" {
					t.Errorf("自动登记的事件 actor=%s rule=%s", e.Actor.Type, e.Basis.RuleID)
				}
			}
		}
		if !found {
			t.Errorf("没有找到自动登记的 material.revised 事件: %s", body)
		}
	})

	t.Run("同一位置的第二次 done 回执不产生第二条材料，也不重复关联", func(t *testing.T) {
		p2, c2 := f.delegated("RL-2", "acc-open")
		f.harnessReport(c2, "done", "vault:notes/out/new.md")
		if n := len(f.materials(proj)); n != 2 {
			t.Fatalf("同一位置应复用，项目材料 %d 条", n)
		}
		fresh, _ := f.byLink(proj, "vault:notes/out/new.md")
		if outs := f.outputs(p2); len(outs) != 1 || outs[0] != fresh.ID {
			t.Fatalf("第二个父事项的产出 = %v，期望 [%s]", outs, fresh.ID)
		}
	})

	t.Run("progress 与 blocked 回执带库内链接也不登记", func(t *testing.T) {
		before := len(f.materials(proj))
		for _, outcome := range []string{"progress", "blocked"} {
			p, c := f.delegated("RL-"+outcome, "acc-open")
			f.harnessReport(c, outcome, "vault:notes/out/later-"+outcome+".md")
			if outs := f.outputs(p); len(outs) != 0 {
				t.Errorf("%s 回执后父事项不应有产出: %v", outcome, outs)
			}
			if _, ok := f.byLink(proj, "vault:notes/out/later-"+outcome+".md"); ok {
				t.Errorf("%s 回执里的链接不应被登记", outcome)
			}
		}
		if after := len(f.materials(proj)); after != before {
			t.Errorf("项目材料 %d → %d", before, after)
		}
	})

	t.Run("只带库外链接的 done 回执照常完成，只记在证据里", func(t *testing.T) {
		p, c := f.delegated("RL-ext", "acc-open")
		before := len(f.materials(proj))
		f.harnessReport(c, "done", "vault:other/y.md", "https://example.com/pr/2")
		if ph := f.phase(c); ph != "done" {
			t.Fatalf("子事项应 done，实际 %s", ph)
		}
		if outs := f.outputs(p); len(outs) != 0 {
			t.Errorf("父事项不应有产出: %v", outs)
		}
		if after := len(f.materials(proj)); after != before {
			t.Errorf("项目材料 %d → %d", before, after)
		}
	})
}
