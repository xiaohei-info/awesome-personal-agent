package task

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/summaries"
)

// pendingDigest 是还没汇入过简报的"知会"条数。
func (e *env) pendingDigest() int {
	e.t.Helper()
	var n int
	if err := e.st.View(bg, func(tx *store.Tx) error {
		d, err := summaries.PendingDigest(tx)
		n = len(d)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// runCall 按模板跑一次实例到期的 call_agent，等运行结束，返回事项与资料。
func (e *env) runCall(tmpl string) (string, map[string]string) {
	e.t.Helper()
	id := e.openItemWithOrigin("instance:ins_1")
	if err := e.svc.CallAgent(AgentOptions{})(bg, e.call(tmpl, id)); err != nil {
		e.t.Fatal(err)
	}
	return id, materialsOf(e, e.waitResult().Run.ID)
}

// "知会"只在简报真的完成后才算汇入：运行失败、写回不全都留给下一份；成功后不再重复。
func TestDigestConsumedOnlyAfterSuccessfulBrief(t *testing.T) {
	e := setup(t)
	ids := e.rawSource("src_news", "full", domain.SourceEvent{ID: "n1", Type: "news", Subject: "资讯一", OccurredAt: t0})
	e.digest(ids...)

	e.harness("hx", "exit1") // 运行失败
	e.setDefaultHarness("hx")
	if id, m := e.runCall("brief"); !strings.Contains(m["digest"], "资讯一") || e.item(id).Phase != domain.PhaseNeedsInput || e.pendingDigest() != 1 {
		t.Fatalf("a failed run must not consume digest: %s pending=%d", e.item(id).Phase, e.pendingDigest())
	}
	e.harness("hs", "summary") // 报 done 但没写声明的字段：写回不全
	e.setDefaultHarness("hs")
	if id, _ := e.runCall("write-brief"); e.item(id).Phase != domain.PhaseNeedsInput || e.pendingDigest() != 1 {
		t.Fatalf("an incomplete write-back must not consume digest: %s pending=%d", e.item(id).Phase, e.pendingDigest())
	}
	if id, _ := e.runCall("brief"); e.item(id).Phase != domain.PhaseDone || e.pendingDigest() != 0 { // 无声明字段的模板凭摘要完成
		t.Fatalf("a successful brief consumes digest: %s pending=%d", e.item(id).Phase, e.pendingDigest())
	}
	if _, m := e.runCall("brief"); strings.Contains(m["digest"], "资讯一") {
		t.Fatalf("consumed digest must not repeat: %s", m["digest"])
	}
}

// 复盘只组声明的段：不查知会与抽检，也不汇入知会；日报照样拿得到。
func TestReviewNeitherQueriesNorConsumesDigest(t *testing.T) {
	e := setup(t)
	ids := e.rawSource("src_news", "full", domain.SourceEvent{ID: "n1", Type: "news", Subject: "资讯一", OccurredAt: t0})
	e.digest(ids...)
	e.harness("hx", "noOutcome")
	e.setDefaultHarness("hx")
	id := e.openItemWithOrigin("instance:ins_1")
	if err := e.svc.CallAgent(AgentOptions{})(bg, e.call("write-review", id)); err != nil {
		t.Fatal(err)
	}
	runDir := e.runs + "/" + e.runList()[0].Run.ID
	agent := runidentity.Identity{Secret: waitFile(t, runDir+"/token")}
	e.must(agent, "item.update", map[string]any{"id": id, "spec": json.RawMessage(`{"period":"p","sections":[{"title":"本期完成","body":"x"}]}`), "reason": "write"})
	if err := os.WriteFile(runDir+"/go", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := materialsOf(e, e.waitResult().Run.ID)
	for _, absent := range []string{"digest", "spot_check", "ledger_snapshot", "proposals"} {
		if _, ok := m[absent]; ok {
			t.Errorf("review must not carry %s", absent)
		}
	}
	for _, present := range []string{"context", "prior", "activity", "open_items", "due_waiting"} {
		if _, ok := m[present]; !ok {
			t.Errorf("review must carry %s", present)
		}
	}
	if e.item(id).Phase != domain.PhaseDone || e.pendingDigest() != 1 {
		t.Fatalf("review settled %s and left %d digest entries; want done and 1", e.item(id).Phase, e.pendingDigest())
	}
}

// 后续事件只认同一来源里共用关联键的事件（同一对象），按该来源外发范围裁剪；不按链接推测，没有后续就不给。
func TestProposalFollowedByLaterEventsOfSameObject(t *testing.T) {
	e := setup(t)
	gh := e.rawSource("src_gh", "meta",
		domain.SourceEvent{ID: "o1", Type: "pr.opened", Subject: "PR 一", Link: "https://example.org/r/pull/1", OccurredAt: t0.Add(-3 * time.Hour), CorrelationKeys: []string{"r#1"}},
		domain.SourceEvent{ID: "o2", Type: "pr.opened", Subject: "PR 二", Link: "https://example.org/r", OccurredAt: t0.Add(-3 * time.Hour), CorrelationKeys: []string{"r#2"}},
		domain.SourceEvent{ID: "c1", Type: "pr.commented", Subject: "PR 一 评论", OccurredAt: t0.Add(-2 * time.Hour), CorrelationKeys: []string{"r#1"}},
		domain.SourceEvent{ID: "m1", Type: "pr.merged", Subject: "PR 一", Summary: "SUMMARY-HIDDEN-BY-META", OccurredAt: t0.Add(-time.Hour), CorrelationKeys: []string{"r#1"}},
		domain.SourceEvent{ID: "x2", Type: "pr.updated", Subject: "同链接别的对象", Link: "https://example.org/r", OccurredAt: t0.Add(-time.Hour), CorrelationKeys: []string{"r#9"}},
	)
	hidden := e.rawSource("src_none", "", // 外发范围未选择：只计数
		domain.SourceEvent{ID: "h1", Type: "mail", Subject: "原信", OccurredAt: t0.Add(-3 * time.Hour), CorrelationKeys: []string{"thread:a"}},
		domain.SourceEvent{ID: "h2", Type: "mail", Subject: "SECRET-REPLY", OccurredAt: t0.Add(-time.Hour), CorrelationKeys: []string{"thread:a"}},
	)
	for title, src := range map[string]string{"合并的": gh[0], "没后续的": gh[1], "看不到的": hidden[0]} {
		e.must(kernel, "item.create", map[string]any{"kind": "task", "title": title, "phase": "proposed",
			"proposal": map[string]any{"recommend": "accept", "reason": "r", "sources": []string{src}}})
	}
	e.harness("hx", "summary")
	e.setDefaultHarness("hx")
	_, m := e.runCall("write-brief")
	var pr struct {
		Proposals []struct {
			Title      string    `json:"title"`
			WebURL     string    `json:"web_url"`
			FollowedBy *followUp `json:"followed_by"`
		} `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(m["proposals"]), &pr); err != nil {
		t.Fatal(err)
	}
	got := map[string]*followUp{}
	for _, p := range pr.Proposals {
		got[p.Title] = p.FollowedBy
		if !strings.HasPrefix(p.WebURL, "http://web.example/items/") {
			t.Errorf("proposal %s web_url %q", p.Title, p.WebURL)
		}
	}
	if f := got["合并的"]; f == nil || f.Count != 2 || len(f.Latest) != 2 || f.Latest[0].Type != "pr.merged" || f.Latest[1].Type != "pr.commented" {
		t.Fatalf("merged PR follow-up: %+v", f)
	}
	if strings.Contains(m["proposals"], "SUMMARY-HIDDEN-BY-META") || strings.Contains(m["proposals"], "SECRET-REPLY") || strings.Contains(m["proposals"], "同链接别的对象") {
		t.Fatalf("follow-ups must respect outbound scope and object keys: %s", m["proposals"])
	}
	if got["没后续的"] != nil {
		t.Fatalf("a shared link is not the same object: %+v", got["没后续的"])
	}
	if f := got["看不到的"]; f == nil || f.Count != 1 || len(f.Latest) != 0 {
		t.Fatalf("outbound none gives a count only: %+v", f)
	}
}

// context 把来源覆盖分成三层：拉取（失败、从未、正常）、窗口内落盘数、未初筛积压；时刻带实例时区。
// 项目范围真正过滤事项类段；简报自己不算活动；在途事项不截断，等本人决定的排最前。
func TestContextCoverageScopeAndOpenItems(t *testing.T) {
	e := setup(t)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, s := range []struct{ id, stats string }{
			{"src_fail", `{"runs":5,"consecutive_failures":3,"last_success_at":"2026-02-27T00:00:00Z"}`},
			{"src_never", ``},
			{"src_ok", `{"runs":9,"last_success_at":"2026-02-28T23:00:00Z"}`},
		} {
			if _, err := tx.Exec(`INSERT INTO sources_registry_source
				(id, name, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, default_project, outbound_local, outbound_external, config, cursor, enabled, stats)
				VALUES (?, ?, 'script', 'mail', '', '', 0, '', '', '', 'full', 'full', '', '', 1, ?)`, s.id, "名-"+s.id, s.stats); err != nil {
				return err
			}
		}
		// 正常拉取的来源收到一条、还没初筛：落盘数与积压分开计
		if err := runner.AppendInternal(tx, domain.Source{ID: "src_ok"}, domain.SourceEvent{ID: "q1", Type: "mail", Subject: "积压", OccurredAt: t0}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, "src_ok")
		if err != nil {
			return err
		}
		return runner.SetDisposition(tx, evs[0].ID, domain.DispositionUnjudged, "", domain.UnjudgedModelUnavailable)
	}); err != nil {
		t.Fatal(err)
	}
	e.rawSource("src_x", "full") // 与覆盖无关的来源，只为借 helper 写事件
	var p1, p2 struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "project.create", map[string]any{"name": "工作"}).Value, &p1)
	_ = json.Unmarshal(e.must(owner, "project.create", map[string]any{"name": "家里"}).Value, &p2)
	e.must(owner, "item.create", map[string]any{"kind": "task", "title": "范围内-在途", "phase": "open", "project": p1.ID})
	e.must(owner, "item.create", map[string]any{"kind": "task", "title": "范围内-待决", "phase": "needs_input", "project": p1.ID, "ask": map[string]any{"question": "选哪个？"}})
	e.must(owner, "item.create", map[string]any{"kind": "task", "title": "范围外-到期", "phase": "open", "project": p2.ID, "deadline": t0})
	for _, in := range []map[string]any{{"kind": "task", "title": "范围内-完成", "phase": "open", "project": p1.ID}, {"kind": "brief", "title": "OLD-BRIEF", "phase": "open", "project": p1.ID}} {
		var ref struct{ ID string }
		_ = json.Unmarshal(e.must(owner, "item.create", in).Value, &ref)
		e.must(owner, "item.transition", map[string]any{"id": ref.ID, "to": "done", "reason": "ok", "evidence": "e"})
	}

	e.harness("hx", "summary")
	e.setDefaultHarness("hx")
	id := e.openItemWithOrigin("instance:ins_1")
	c := e.call("write-review", id)
	c.Instance.Params = json.RawMessage(`{"schedule":"CRON_TZ=Asia/Shanghai 0 8 * * *","projects":["` + p1.ID + `"]}`)
	if err := e.svc.CallAgent(AgentOptions{})(bg, c); err != nil {
		t.Fatal(err)
	}
	m := materialsOf(e, e.waitResult().Run.ID)

	var ctx struct {
		Context struct {
			Now      string           `json:"now"`
			Timezone string           `json:"timezone"`
			Projects []string         `json:"projects"`
			Sources  []sourceCoverage `json:"sources"`
		} `json:"context"`
	}
	if err := json.Unmarshal([]byte(m["context"]), &ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Context.Timezone != "Asia/Shanghai" || !strings.HasSuffix(ctx.Context.Now, "+08:00") || strings.Join(ctx.Context.Projects, ",") != "工作" {
		t.Fatalf("context zone and scope: %+v", ctx.Context)
	}
	fetch := map[string]sourceCoverage{}
	for _, s := range ctx.Context.Sources {
		fetch[s.Name] = s
	}
	if f := fetch["名-src_fail"]; f.Fetch != "failing" || f.Failures != 3 || f.LastSuccess == nil {
		t.Errorf("failing source: %+v", f)
	}
	if ok := fetch["名-src_ok"]; ok.Fetch != "ok" || ok.Ingested != 1 || ok.Backlog != 1 {
		t.Errorf("ok source with backlog: %+v", ok)
	}
	if fetch["名-src_never"].Fetch != "never" || fetch["名-src_never"].Ingested != 0 {
		t.Errorf("never/ok sources: %+v", fetch)
	}
	if strings.Contains(m["due_waiting"], "范围外") || strings.Contains(m["open_items"], "范围外") {
		t.Fatalf("project scope must filter item sections: %s %s", m["due_waiting"], m["open_items"])
	}
	var oi struct {
		Open struct {
			Total int `json:"total"`
			Items []struct {
				Title   string `json:"title"`
				Project string `json:"project"`
				WebURL  string `json:"web_url"`
			} `json:"items"`
		} `json:"open_items"`
	}
	if err := json.Unmarshal([]byte(m["open_items"]), &oi); err != nil || oi.Open.Total != 2 || len(oi.Open.Items) != 2 ||
		oi.Open.Items[0].Title != "范围内-待决" || oi.Open.Items[0].Project != "工作" || oi.Open.Items[0].WebURL == "" {
		t.Fatalf("open_items: %v %+v", err, oi.Open)
	}
	if !strings.Contains(m["activity"], "范围内-完成") || strings.Contains(m["activity"], "OLD-BRIEF") {
		t.Fatalf("activity counts work, not briefs: %s", m["activity"])
	}
}

// 失败的那份简报不作下一份的起点：窗口从最近一份成功的简报算起，资料里的 prior 也是那份。
func TestFailedBriefDoesNotMoveWindowOrPrior(t *testing.T) {
	e := setup(t)
	e.harness("hx", "summary")
	e.setDefaultHarness("hx")
	ok, _ := e.runCall("brief") // 成功：窗口 [t0-24h, t0]
	e.harness("hf", "exit1")
	e.setDefaultHarness("hf")
	e.svc.o.Now = func() time.Time { return t0.Add(30 * time.Minute) }
	bad, _ := e.runCall("brief") // 失败，但拉起时写了窗口 [t0, t0+30m]
	if e.item(ok).Phase != domain.PhaseDone || e.item(bad).Phase != domain.PhaseNeedsInput {
		t.Fatalf("setup: %s %s", e.item(ok).Phase, e.item(bad).Phase)
	}
	e.setDefaultHarness("hx")
	e.svc.o.Now = func() time.Time { return t0.Add(time.Hour) }
	_, m := e.runCall("write-brief")
	var ctx struct {
		Context struct {
			Window window `json:"window"`
		} `json:"context"`
	}
	var pr struct {
		Prior *struct{ CreatedAt time.Time } `json:"prior"`
	}
	if err := json.Unmarshal([]byte(m["context"]), &ctx); err != nil || !ctx.Context.Window.Since.Equal(t0) {
		t.Fatalf("window must start at the successful brief's until: %v %+v", err, ctx.Context.Window)
	}
	if err := json.Unmarshal([]byte(m["prior"]), &pr); err != nil || pr.Prior == nil {
		t.Fatalf("prior must be the successful brief: %v %s", err, m["prior"])
	}
}
