//go:build e2e

package materials

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func setup(t *testing.T) *fx { t.Helper(); return &fx{newEnv(t, ""), t} }

// TestFlowMaterialOwned 对应验收文档 flow-material-owned（ACT-MAT-01..13）。
func TestFlowMaterialOwned(t *testing.T) {
	f := setup(t)
	open := f.Projects["acc-open"]
	var note, plan committed // 纪要（direct）与方案（proposed）

	t.Run("ACT-MAT-01 纪要类直接生效", func(t *testing.T) {
		note = f.commit(f.WRK, "# 周会\n本周讨论登录超时的根因", "--reason", "周会", "--kind", "minutes", "--title", "周会纪要", "--project", open)
		eq(t, "修订状态", note.State, "current")
		s := f.show(note.Material)
		eq(t, "current_rev", s.Material.CurrentRev, note.Revision)
		if s.Revision == nil || s.Revision.State != "current" || s.Revision.Actor.Type != "agent" || !strings.Contains(s.Revision.Content, "登录超时") {
			t.Errorf("material show 应回当前修订且 actor 为 agent: %+v", s.Revision)
		}
		if len(f.events(note.Material, "material.revised")) != 1 {
			t.Error("事件流应有一条 material.revised")
		}
		var seen bool
		for _, e := range f.agentChanges(note.Material) {
			seen = seen || e.Type == "material.revised"
		}
		if !seen {
			t.Error("agent 变更视图应包含这次直写")
		}
	})

	t.Run("ACT-MAT-02 方案类进入确认", func(t *testing.T) {
		plan = f.commit(f.WRK, "# 方案 v1\n用令牌桶限流", "--reason", "初稿", "--kind", "design", "--title", "限流方案", "--project", open)
		eq(t, "修订状态", plan.State, "proposed")
		s := f.show(plan.Material)
		eq(t, "current_rev（不生效）", s.Material.CurrentRev, "")
		if s.Revision != nil {
			t.Errorf("proposed 修订不应成为当前内容: %+v", s.Revision)
		}
		rs := f.revisions(plan.Material)
		if len(rs) != 1 || rs[0].State != "proposed" || rs[0].Content == "" {
			t.Errorf("Web 版本列表应有一条带正文的待确认修订: %+v", rs)
		}
		if len(f.events(plan.Material, "material.revision_proposed")) != 1 {
			t.Error("事件流应有 material.revision_proposed")
		}
	})

	t.Run("ACT-MAT-03 确认与拒绝修订", func(t *testing.T) {
		f.webOK("POST", "/revisions/"+plan.Revision+"/confirm", `{}`, http.StatusOK)
		eq(t, "确认后 current_rev", f.show(plan.Material).Material.CurrentRev, plan.Revision)
		v2 := f.revise(f.WRK, plan.Material, "# 方案 v2\n用滑动窗口", "--reason", "改", "--based-on", plan.Revision)
		v3 := f.revise(f.WRK, plan.Material, "# 方案 v3\n被拒的方案", "--reason", "再改", "--based-on", plan.Revision)
		f.webOK("POST", "/revisions/"+v2.Revision+"/confirm", `{}`, http.StatusOK)
		f.webOK("POST", "/revisions/"+v3.Revision+"/reject", `{}`, http.StatusOK)
		states := map[string]revision{}
		for _, r := range f.revisions(plan.Material) {
			states[r.ID] = r
		}
		eq(t, "新确认的修订", states[v2.Revision].State, "current")
		eq(t, "旧修订", states[plan.Revision].State, "superseded")
		if r := states[v3.Revision]; r.State == "current" || !strings.Contains(r.Content, "被拒的方案") {
			t.Errorf("被拒绝的修订不应生效且内容保留: %+v", r)
		}
		eq(t, "current_rev", f.show(plan.Material).Material.CurrentRev, v2.Revision)
	})

	t.Run("ACT-MAT-04 修订追加不改，版本列表带 actor/reason/based_on", func(t *testing.T) {
		a := f.commit(f.WRK, "第一版", "--reason", "r1", "--kind", "worknote", "--title", "笔记", "--project", open)
		b := f.revise(f.WRK, a.Material, "第二版", "--reason", "r2", "--based-on", a.Revision)
		c := f.revise(f.WRK, a.Material, "第三版", "--reason", "r3", "--based-on", b.Revision)
		old := f.show(a.Material, "--rev", a.Revision)
		if old.Revision == nil || old.Revision.Content != "第一版" {
			t.Errorf("旧修订内容被改写: %+v", old.Revision)
		}
		rs := f.revisions(a.Material)
		if len(rs) != 3 {
			t.Fatalf("应有 3 条修订: %+v", rs)
		}
		for i, want := range []struct{ id, reason, based string }{{a.Revision, "r1", ""}, {b.Revision, "r2", a.Revision}, {c.Revision, "r3", b.Revision}} {
			r := rs[i]
			if r.ID != want.id || r.Reason != want.reason || r.BasedOn != want.based || r.Actor.Type != "agent" {
				t.Errorf("第 %d 条修订 = %+v，期望 %+v", i+1, r, want)
			}
		}
		// diff 由 Web 对两条修订的正文逐行比较：两条正文确实不同且都完整保留
		if rs[0].Content == rs[2].Content || rs[2].Content != "第三版" {
			t.Errorf("diff 的两端正文不对: %q / %q", rs[0].Content, rs[2].Content)
		}
	})

	t.Run("ACT-MAT-05 基线变化时直写冲突退出码 6", func(t *testing.T) {
		a := f.commit(f.WRK, "基线", "--reason", "起", "--kind", "reference", "--title", "参考", "--project", open)
		f.revise(f.WRK, a.Material, "先到的", "--reason", "x", "--based-on", a.Revision)
		r := f.Apa(f.WRK, "", "material", "commit", a.Material, "--from", f.file("late.md", "后到的"), "--reason", "y", "--based-on", a.Revision)
		eq(t, "后一次退出码", r.Code, 6)
		if got := f.show(a.Material).Revision; got == nil || got.Content != "先到的" {
			t.Errorf("先到的内容被静默覆盖: %+v", got)
		}
	})

	t.Run("ACT-MAT-06 proposed 修订基线变化时提示，可采用或基于当前重提", func(t *testing.T) {
		base := f.commit(f.WRK, "# 规格 A", "--reason", "起", "--kind", "requirement", "--title", "需求", "--project", open)
		f.webOK("POST", "/revisions/"+base.Revision+"/confirm", `{}`, http.StatusOK)
		stale := f.revise(f.WRK, base.Material, "# 规格 B（旧基线）", "--reason", "旧基线", "--based-on", base.Revision)
		cur := f.revise(f.WRK, base.Material, "# 规格 C", "--reason", "抢先", "--based-on", base.Revision)
		f.webOK("POST", "/revisions/"+cur.Revision+"/confirm", `{}`, http.StatusOK)
		eq(t, "旧基线的提议仍可入队", f.revisions(base.Material)[1].State, "proposed")
		if r := f.Web("POST", "/v1/revisions/"+stale.Revision+"/confirm", `{}`); r.Code != http.StatusConflict || !strings.Contains(r.Body, "baseline changed") {
			t.Errorf("无取舍的确认应提示基线已变化: %d %s", r.Code, r.Body)
		}
		f.webOK("POST", "/revisions/"+stale.Revision+"/confirm", `{"on_stale":"adopt"}`, http.StatusOK)
		eq(t, "仍然采用后 current_rev", f.show(base.Material).Material.CurrentRev, stale.Revision)

		stale2 := f.revise(f.WRK, base.Material, "# 规格 D（旧基线）", "--reason", "旧基线2", "--based-on", base.Revision)
		f.webOK("POST", "/revisions/"+stale2.Revision+"/confirm", `{"on_stale":"rebase"}`, http.StatusOK)
		eq(t, "基于当前重提不直接生效", f.show(base.Material).Material.CurrentRev, stale.Revision)
		var rebased bool
		for _, r := range f.revisions(base.Material) {
			rebased = rebased || (r.State == "proposed" && r.BasedOn == stale.Revision && strings.Contains(r.Content, "规格 D"))
		}
		if !rebased {
			t.Error("rebase 应生成基于当前修订、等待再次确认的 proposed 修订")
		}
	})

	t.Run("ACT-MAT-07 回退只在 Web", func(t *testing.T) {
		a := f.commit(f.WRK, "原文", "--reason", "起", "--kind", "worknote", "--title", "回退用", "--project", open)
		b := f.revise(f.WRK, a.Material, "改坏了", "--reason", "改", "--based-on", a.Revision)
		body := `{"to":"` + a.Revision + `","reason":"回退"}`
		if r := f.APIRaw("POST", "/v1/materials/"+a.Material+"/revert", body, f.DEL); r.Code < 400 {
			t.Errorf("token 不应能回退: %d %s", r.Code, r.Body)
		}
		eq(t, "被拒绝后 current_rev", f.show(a.Material).Material.CurrentRev, b.Revision)
		before := len(f.revisions(a.Material))
		f.webOK("POST", "/materials/"+a.Material+"/revert", body, http.StatusOK)
		rs := f.revisions(a.Material)
		eq(t, "回退新增一条修订", len(rs), before+1)
		if last := rs[len(rs)-1]; last.Content != "原文" || last.State != "current" {
			t.Errorf("回退应新增内容等于旧快照的当前修订: %+v", last)
		}
		eq(t, "历史未改", rs[0].Content, "原文")
		eq(t, "历史未改", rs[1].Content, "改坏了")
	})

	t.Run("ACT-MAT-08 附件内容寻址", func(t *testing.T) {
		img := f.file("a.png", "\x89PNG fake-image-bytes")
		put := func() string {
			var out struct {
				Hash string `json:"hash"`
			}
			out = decode[struct {
				Hash string `json:"hash"`
			}](t, f.apa(f.WRK, 0, "blob", "put", img).Stdout)
			return out.Hash
		}
		h1, h2 := put(), put()
		if len(h1) != 64 || h1 != h2 {
			t.Fatalf("两次上传 hash 应相同且为 sha256: %q %q", h1, h2)
		}
		c := f.commit(f.WRK, "截图见 apa://blob/"+h1, "--reason", "附图", "--kind", "worknote", "--title", "带图", "--project", open)
		if got := f.show(c.Material).Revision; got == nil || !strings.Contains(got.Content, "apa://blob/"+h1) {
			t.Errorf("正文应引用 blob: %+v", got)
		}
		out := f.file("back.png", "")
		f.apa(f.OBS, 0, "blob", "get", h1, "--out", out)
		if b, _ := os.ReadFile(out); string(b) != "\x89PNG fake-image-bytes" {
			t.Errorf("取回内容不一致: %q", b)
		}
	})

	t.Run("ACT-MAT-09 中文全文检索，CLI 与 Web 一致，筛选正确", func(t *testing.T) {
		type hits struct {
			Hits []map[string]any `json:"hits"`
		}
		cli := decode[hits](t, f.apa(f.OBS, 0, "material", "search", "超时").Stdout)
		web := decode[hits](t, f.webOK("GET", "/materials/search?q=%E8%B6%85%E6%97%B6", "", http.StatusOK))
		if len(cli.Hits) == 0 {
			t.Fatal("中文关键词 超时 应命中纪要")
		}
		for _, h := range cli.Hits {
			for k := range h {
				switch k {
				case "id", "title", "snippet", "kind", "project", "hosted", "tags", "path", "repo", "rank", "score", "updated_at", "at":
				default:
					t.Errorf("命中结果带了多余字段 %q（只应有 id、标题、片段等）", k)
				}
			}
			if s, _ := h["snippet"].(string); strings.Count(s, "") > 400 {
				t.Errorf("snippet 不应是全文: %d 字符", len(s))
			}
		}
		if len(cli.Hits) != len(web.Hits) || cli.Hits[0]["id"] != web.Hits[0]["id"] {
			t.Errorf("Web 与 CLI 结果应一致: %v vs %v", cli.Hits, web.Hits)
		}
		kinds := decode[struct {
			Materials []material `json:"materials"`
		}](t, f.apa(f.OBS, 0, "material", "list", "--project", open).Stdout).Materials
		if len(kinds) == 0 {
			t.Fatal("按项目筛选应有材料")
		}
		for _, m := range kinds {
			eq(t, "项目筛选", m.Project, open)
		}
		only := decode[hits](t, f.apa(f.OBS, 0, "material", "search", "超时", "--kind", "design").Stdout)
		eq(t, "kind=design 不应命中纪要", len(only.Hits), 0)
		other := decode[struct {
			Materials []material `json:"materials"`
		}](t, f.apa(f.OBS, 0, "material", "list", "--project", f.Projects["acc-empty"]).Stdout).Materials
		eq(t, "别的项目不混入", len(other), 0)
	})

	t.Run("ACT-MAT-10 context 不带正文", func(t *testing.T) {
		out := f.apa(f.DEL, 0, "context", "--session", "s-mat", "--scope", "project", "--project", open, "--format", "inject").Stdout
		if !strings.Contains(out, "周会纪要") {
			t.Errorf("context 应带材料标题: %s", out)
		}
		for _, body := range []string{"本周讨论登录超时的根因", "用滑动窗口", "第三版"} {
			if strings.Contains(out, body) {
				t.Errorf("context 不应带材料正文 %q: %s", body, out)
			}
		}
	})

	t.Run("ACT-MAT-11 写入策略可在 Web 调整", func(t *testing.T) {
		changed := len(f.events("", "policy.changed"))
		f.webOK("PUT", "/settings/policy", `{"material_write":{"default":"proposed","kinds":{"minutes":"proposed","reference":"direct","worknote":"direct","requirement":"proposed","design":"proposed","guideline":"proposed"}}}`, http.StatusOK)
		c := f.revise(f.WRK, note.Material, "策略改后的纪要", "--reason", "二次", "--based-on", note.Revision)
		eq(t, "纪要进入确认流", c.State, "proposed")
		eq(t, "纪要 current_rev 不变", f.show(note.Material).Material.CurrentRev, note.Revision)
		if len(f.events("", "policy.changed")) != changed+1 {
			t.Error("策略变更应记一条 policy.changed Event")
		}

	})

	t.Run("ACT-MAT-12 observer 不能提交修订，退出码 4", func(t *testing.T) {
		before := len(f.revisions(note.Material))
		r := f.Apa(f.OBS, "", "material", "commit", note.Material, "--from", f.file("x.md", "x"), "--reason", "x")
		eq(t, "退出码", r.Code, 4)
		eq(t, "修订数不变", len(f.revisions(note.Material)), before)
	})

	t.Run("ACT-MAT-13 事项与材料建立类型化关联", func(t *testing.T) {
		item := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "依据材料的任务", "--phase", "open", "--assignee", "work-host.cli",
			"--project", open, "--evidence", "建个任务").Stdout).ID
		f.apa(f.WRK, 0, "item", "relate", item, "--to", plan.Material, "--type", "derived_from", "--reason", "依据该方案")
		got := f.apa(f.OBS, 0, "item", "show", item).Stdout
		if !strings.Contains(got, plan.Material) || !strings.Contains(got, "derived_from") {
			t.Errorf("事项一侧应看到带类型的关联: %s", got)
		}
		hit := f.webOK("GET", "/materials/search?q=%E6%96%B9%E6%A1%88&item="+item, "", http.StatusOK)
		if !strings.Contains(hit, plan.Material) {
			t.Errorf("材料一侧（按关联事项筛选）应看到该材料: %s", hit)
		}
		if len(f.events(item, "item.updated")) == 0 {
			t.Error("关联变更应记 Event")
		}
	})
}
