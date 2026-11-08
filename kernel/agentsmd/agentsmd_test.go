package agentsmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

var bg = context.Background()

func agent(p string, lvl authz.Level) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: p, Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: lvl}
}

var (
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	worker   = agent("wrk", authz.LevelWorker)
	delegate = agent("dlg", authz.LevelDelegate)
	observer = agent("obs", authz.LevelObserver)
)

const repo = "https://example.com/acme/app.git"

type env struct {
	st    *store.Store
	x     *command.Executor
	svc   *Service
	prj   string
	errs  chan error
	fetch func(context.Context, string) (string, bool, error)
}

// template 取 agents-md 内置包里真实的渲染模板。
func template(t *testing.T) string {
	t.Helper()
	ld := kp.New(kp.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	if _, err := ld.Load(); err != nil {
		t.Fatal(err)
	}
	p, ok := ld.Get("agents-md")
	if !ok || p.Templates["agents"] == "" {
		t.Fatal("agents-md pack has no render template")
	}
	return p.Templates["agents"]
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, materials.Module, hosted.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, errs: make(chan error, 16)}
	e.x = command.New(st, time.Now)
	for _, f := range []func() error{
		func() error { return projects.Register(e.x) },
		func() error { return items.Register(e.x, items.NewKinds()) },
		func() error { return materials.Register(e.x) },
		func() error { return hosted.Register(e.x) },
		func() error { return decisions.Register(e.x) },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	e.svc, err = New(e.x, Options{Template: template(t), OnError: func(err error) { e.errs <- err },
		Fetch: func(ctx context.Context, r string) (string, bool, error) { return e.fetch(ctx, r) }})
	if err != nil {
		t.Fatal(err)
	}
	e.prj = e.id(t, e.must(t, owner, "project.create", map[string]any{"name": "acc-work"}))
	e.must(t, owner, "project.repo.add", map[string]any{"id": e.prj, "remote": repo})
	return e
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", WebURL: "http://web/x"})
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) command.Result {
	t.Helper()
	r, err := e.do(id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func (e *env) id(t *testing.T, r command.Result) string {
	t.Helper()
	var o struct{ ID string }
	if err := json.Unmarshal(r.Value, &o); err != nil || o.ID == "" {
		t.Fatalf("no id in %s: %v", r.Value, err)
	}
	return o.ID
}

func exitOf(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func (e *env) wantExit(t *testing.T, want int, id command.Identity, cmd string, in any) {
	t.Helper()
	if _, err := e.do(id, cmd, in); exitOf(err) != want {
		t.Fatalf("%s: want exit %d, got %v", cmd, want, err)
	}
}

// guideline 提交一份规范并由本人确认生效，返回材料 id。
func (e *env) guideline(t *testing.T, project, content string) string {
	t.Helper()
	in := map[string]any{"project": project, "kind": "guideline", "title": "规范", "content": content, "reason": "r"}
	e.st.View(bg, func(tx *store.Tx) error {
		ms, _ := materials.List(tx, project)
		for _, m := range ms {
			if m.Kind == materials.KindGuideline && m.Project == project {
				in = map[string]any{"id": m.ID, "content": content, "reason": "r", "based_on": m.CurrentRev}
			}
		}
		return nil
	})
	var r materials.Result
	json.Unmarshal(e.must(t, worker, "material.commit", in).Value, &r)
	if r.State != materials.StateProposed {
		t.Fatalf("a guideline is always proposed: %+v", r)
	}
	e.must(t, owner, "revision.confirm", map[string]any{"id": r.Revision})
	return r.Material
}

func (e *env) view(t *testing.T) View {
	t.Helper()
	v, err := e.svc.Get(bg, repo)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) check(t *testing.T, id command.Identity, content string) Verdict {
	t.Helper()
	var v Verdict
	json.Unmarshal(e.must(t, id, "agents.check", map[string]any{"repo": repo, "host": "m1", "content": content}).Value, &v)
	return v
}

func (e *env) sync(t *testing.T, content string) SyncOut {
	t.Helper()
	var v SyncOut
	json.Unmarshal(e.must(t, observer, "agents.sync", map[string]any{"repo": repo, "host": "m1", "content": content}).Value, &v)
	return v
}

func (e *env) needsInput(t *testing.T) []domain.Item {
	t.Helper()
	var out []domain.Item
	e.st.View(bg, func(tx *store.Tx) error {
		all, err := items.List(tx, items.Filter{})
		for _, it := range all {
			if it.Phase == domain.PhaseNeedsInput {
				out = append(out, it)
			}
		}
		return err
	})
	return out
}

func (e *env) item(t *testing.T, id string) domain.Item {
	t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) resolve(t *testing.T, id command.Identity, item, choice string) ResolveOut {
	t.Helper()
	out, err := e.svc.Resolve(bg, id, ResolveIn{Item: item, Choice: choice})
	if err != nil {
		t.Fatalf("resolve %s: %v", choice, err)
	}
	return out
}

func (e *env) noErrors(t *testing.T) {
	t.Helper()
	select {
	case err := <-e.errs:
		t.Fatalf("background render failed: %v", err)
	default:
	}
}

// 把渲染结果的项目部分手改一行，模拟有人直接改了工作区文件。
func edited(v View) string {
	return strings.Replace(v.Content, "## 处理规则", "## 处理规则\n\n- 手改：发布前先跑冒烟", 1)
}

const projGuideline = "## 处理规则\n\n- 先写测试\n\n## 背景\n\n   \n\n## 禁区\n\n- 不推默认分支"

// 渲染：按模板五段依次生成；空小节被省略；hash 不含头部标记行。
func TestRenderFollowsTemplateAndHashIgnoresHeader(t *testing.T) {
	e := setup(t)
	e.guideline(t, "", "## 全局\n\n- 说中文")
	e.guideline(t, e.prj, projGuideline)
	e.must(t, worker, "material.add", map[string]any{"project": e.prj, "kind": "design", "repo": repo, "path": "docs/arch.md", "title": "架构"})
	e.must(t, worker, "material.add", map[string]any{"project": e.prj, "kind": "design", "repo": "https://example.com/other.git", "path": "docs/other.md", "title": "别处"})
	e.noErrors(t)

	v := e.view(t)
	if !strings.HasPrefix(v.Content, "<!-- apa:managed rev=") || !strings.Contains(v.Content, "hash="+v.Hash) {
		t.Fatalf("managed header must carry the hash:\n%s", v.Content)
	}
	order := []string{"## 全局规范", "- 说中文", "## 处理规则", "- 先写测试", "## 禁区", "## 设计文档索引", "- 架构：`docs/arch.md`", "## 协作约定", "走分支与 PR"}
	at := 0
	for _, s := range order {
		i := strings.Index(v.Content[at:], s)
		if i < 0 {
			t.Fatalf("%q missing or out of order in:\n%s", s, v.Content)
		}
		at += i
	}
	if strings.Contains(v.Content, "## 背景") || strings.Contains(v.Content, "别处") {
		t.Fatalf("empty sections and other repos' docs must not render:\n%s", v.Content)
	}
	if FileHash(v.Content) != v.Hash {
		t.Fatalf("hash must be the hash of the file without header lines")
	}
	// 只改头部标记行不算改文件。
	noHeader := strings.Join(strings.Split(v.Content, "\n")[3:], "\n")
	if got := e.check(t, observer, "<!-- apa:managed rev=x hash=y -->\n"+noHeader); got.State != StateCurrent || got.Exit != 0 {
		t.Fatalf("header-only change must stay current: %+v", got)
	}
}

// 规范生效、托管文档路径消失都会让渲染重新生成；记录的 hash 随之变化，旧 hash 成为历史。
func TestRerenderOnGuidelineAndHostedChanges(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	h1 := e.view(t).Hash
	e.guideline(t, e.prj, projGuideline+"\n\n## 新增\n\n- 新规矩")
	h2 := e.view(t).Hash
	if h1 == h2 {
		t.Fatal("confirming a guideline revision must re-render")
	}
	// 同一规范再提交同样内容：渲染结果相同，不产生新记录。
	var n int
	count := func() {
		e.st.View(bg, func(tx *store.Tx) error {
			r, _ := tx.Query("SELECT COUNT(*) FROM agentsmd_render WHERE repo = ?", repo)
			defer r.Close()
			r.Next()
			return r.Scan(&n)
		})
	}
	count()
	before := n
	e.must(t, worker, "agents.render", map[string]any{})
	count()
	if n != before {
		t.Fatalf("rendering identical input must not add a record: %d -> %d", before, n)
	}
	// 规范改回旧内容：旧 hash 重新成为最新。
	e.guideline(t, e.prj, projGuideline)
	if got := e.view(t).Hash; got != h1 {
		t.Fatalf("reverting the guideline must bring back the earlier rendering: %s vs %s", got, h1)
	}
	e.noErrors(t)
}

// 一致 / 过期 / 漂移三态，退出码 0/10/11；过期同一渲染 hash 只提示一次。
func TestCheckThreeStatesAndStalePromptsOnce(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	old := e.view(t)

	if v := e.check(t, observer, old.Content); v.State != StateCurrent || v.Exit != 0 || v.Prompt {
		t.Fatalf("current: %+v", v)
	}
	if lc := e.view(t).LastCheck; lc == nil || lc.State != StateCurrent || lc.Host != "m1" || lc.FileHash != old.Hash {
		t.Fatalf("last_check must record the latest check: %+v", lc)
	}

	e.guideline(t, e.prj, projGuideline+"\n\n## 新增\n\n- 新规矩")
	cur := e.view(t)
	v := e.check(t, observer, old.Content)
	if v.State != StateStale || v.Exit != ExitStale || !v.Prompt || v.Item != "" || v.RenderHash != cur.Hash {
		t.Fatalf("stale first time: %+v", v)
	}
	if v := e.check(t, observer, old.Content); v.State != StateStale || v.Prompt {
		t.Fatalf("the same render hash prompts only once: %+v", v)
	}
	if len(e.needsInput(t)) != 0 {
		t.Fatal("stale must not raise needs_input")
	}
	// 渲染再变，新的过期 hash 重新提示一次。
	e.guideline(t, e.prj, projGuideline+"\n\n## 新增\n\n- 更新的规矩")
	if v := e.check(t, observer, old.Content); !v.Prompt {
		t.Fatalf("a new render hash prompts again: %+v", v)
	}
	// 只报 hash 也能判定一致与过期；报了未知 hash 要求重发内容。
	var hv Verdict
	json.Unmarshal(e.must(t, observer, "agents.check", map[string]any{"repo": repo, "host": "m1", "file_hash": old.Hash}).Value, &hv)
	if hv.State != StateStale {
		t.Fatalf("hash-only stale: %+v", hv)
	}
	e.wantExit(t, domain.ExitUsage, observer, "agents.check", map[string]any{"repo": repo, "host": "m1", "file_hash": "deadbeef"})
	// 文件不存在：可直接写入，按过期处理。
	var mv Verdict
	json.Unmarshal(e.must(t, observer, "agents.check", map[string]any{"repo": repo, "host": "m2", "missing": true}).Value, &mv)
	if mv.State != StateStale || mv.Exit != ExitStale {
		t.Fatalf("missing file: %+v", mv)
	}
	e.noErrors(t)
}

// 漂移：生成带 diff 的 needs_input，选项合回/覆盖/本次忽略；同一份手改不重复打扰；sync 不覆盖。
func TestDriftRaisesNeedsInputWithDiffAndSyncRefuses(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	file := edited(e.view(t))

	v := e.check(t, observer, file)
	if v.State != StateDrift || v.Exit != ExitDrift || v.Item == "" || v.Decision != driftOpen {
		t.Fatalf("drift: %+v", v)
	}
	it := e.item(t, v.Item)
	if it.Phase != domain.PhaseNeedsInput || it.Project != e.prj || it.Ask == nil {
		t.Fatalf("item: %+v", it)
	}
	var ids []string
	for _, o := range it.Ask.Options {
		ids = append(ids, o.ID)
	}
	if strings.Join(ids, ",") != "merge,override,ignore" {
		t.Fatalf("options: %v", ids)
	}
	if !strings.Contains(it.Body, "+ - 手改：发布前先跑冒烟") || strings.Contains(it.Body, "- - ") {
		t.Fatalf("the item must carry the diff of the hand edit:\n%s", it.Body)
	}
	// 再检查同一份手改：沿用同一事项。
	if again := e.check(t, observer, file); again.Item != v.Item || len(e.needsInput(t)) != 1 {
		t.Fatalf("same drift must not raise a second item: %+v", again)
	}
	// sync 不覆盖，手改内容不丢（内核不写文件，只说不写）。
	s := e.sync(t, file)
	if s.Write || !s.Blocked || s.Exit != domain.ExitConflict || s.Content != "" || s.Item != v.Item {
		t.Fatalf("sync must refuse while drifted: %+v", s)
	}
	e.noErrors(t)
}

// ACT-AGM-06：合回——diff 成为 Guideline 的 proposed 修订，确认后重新渲染，sync 之后 check 退出码 0。
func TestMergeBackBecomesProposedRevisionThenRerenders(t *testing.T) {
	e := setup(t)
	mat := e.guideline(t, e.prj, projGuideline)
	file := edited(e.view(t))
	v := e.check(t, observer, file)

	out := e.resolve(t, owner, v.Item, ChoiceMerge)
	if out.Material != mat || out.Revision == "" {
		t.Fatalf("merge must propose a revision of the project guideline: %+v", out)
	}
	if it := e.item(t, v.Item); it.Phase != domain.PhaseDone || it.Ask.Resolution == nil || it.Ask.Resolution.Choice != ChoiceMerge {
		t.Fatalf("drift item must be resolved as merge: %+v", it)
	}
	e.st.View(bg, func(tx *store.Tx) error {
		_, rev, err := materials.Show(tx, mat, out.Revision)
		if err != nil || rev.State != materials.StateProposed || !strings.Contains(rev.Content, "- 手改：发布前先跑冒烟") {
			t.Fatalf("proposed revision must hold the hand edit: %+v %v", rev, err)
		}
		m, _ := materials.Get(tx, mat)
		_, cur, _ := materials.Show(tx, mat, m.CurrentRev)
		if strings.Contains(cur.Content, "手改") {
			t.Fatal("the guideline must not change before the owner confirms")
		}
		return nil
	})
	// 重试同一选择不重复提交修订；另选别的被拒（只认第一次）。
	again, err := e.svc.Resolve(bg, owner, ResolveIn{Item: v.Item, Choice: ChoiceMerge})
	if err != nil || again.Revision != out.Revision {
		t.Fatalf("retry must replay the same revision: %+v %v", again, err)
	}
	if _, err := e.svc.Resolve(bg, owner, ResolveIn{Item: v.Item, Choice: ChoiceOverride}); exitOf(err) != domain.ExitConflict {
		t.Fatalf("the first choice stands: %v", err)
	}
	// 规范生效属 web_only：delegate 不能确认，本人确认后自动重新渲染。
	e.wantExit(t, domain.ExitWebOnly, delegate, "revision.confirm", map[string]any{"id": out.Revision})
	if s := e.sync(t, file); !s.Blocked {
		t.Fatalf("still drifted until the revision is confirmed: %+v", s)
	}
	e.must(t, owner, "revision.confirm", map[string]any{"id": out.Revision})
	e.noErrors(t)
	nv := e.view(t)
	if !strings.Contains(nv.Content, "- 手改：发布前先跑冒烟") {
		t.Fatalf("re-render must include the merged edit:\n%s", nv.Content)
	}
	s := e.sync(t, file)
	if s.State != StateCurrent || s.Write || s.Blocked {
		t.Fatalf("after the merge the edited file is the rendering: %+v", s)
	}
	if v := e.check(t, observer, nv.Content); v.Exit != 0 {
		t.Fatalf("check after sync: %+v", v)
	}
}

// ACT-AGM-07：覆盖——选了才允许 sync 写入；忽略——不写文件、事项关闭，同一份手改不再提。
func TestOverrideAllowsSyncIgnoreWritesNothing(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	rendered := e.view(t)

	drifted := edited(rendered)
	v := e.check(t, observer, drifted)
	if s := e.sync(t, drifted); s.Write {
		t.Fatal("must not write before the owner chose override")
	}
	e.wantExit(t, domain.ExitPermissionDenied, worker, "agents.resolve", map[string]any{"item": v.Item, "choice": "override"})
	e.wantExit(t, domain.ExitUsage, owner, "agents.resolve", map[string]any{"item": v.Item, "choice": "nope"})
	e.resolve(t, owner, v.Item, ChoiceOverride)
	s := e.sync(t, drifted)
	if !s.Write || s.Content != rendered.Content || s.Blocked {
		t.Fatalf("override must let sync write the rendering: %+v", s)
	}
	if it := e.item(t, v.Item); it.Phase != domain.PhaseDone {
		t.Fatalf("override closes the item: %+v", it)
	}

	// 再造一次漂移，选本次忽略。
	other := strings.Replace(rendered.Content, "- 先写测试", "- 先写测试\n- 另一处手改", 1)
	v2 := e.check(t, observer, other)
	if v2.Item == "" || v2.Item == v.Item {
		t.Fatalf("a different hand edit is a new drift: %+v", v2)
	}
	e.resolve(t, owner, v2.Item, ChoiceIgnore)
	if it := e.item(t, v2.Item); it.Phase != domain.PhaseDone {
		t.Fatalf("ignore closes the item: %+v", it)
	}
	if s := e.sync(t, other); s.Write || !s.Blocked {
		t.Fatalf("ignore must not write the file: %+v", s)
	}
	if again := e.check(t, observer, other); again.Item != v2.Item || again.Decision != driftIgnored || len(e.needsInput(t)) != 0 {
		t.Fatalf("an ignored drift must not nag again for the same render: %+v", again)
	}
	// 渲染变了，同一份手改重新作为新的漂移提出。
	e.guideline(t, e.prj, projGuideline+"\n\n## 新增\n\n- 新规矩")
	if v3 := e.check(t, observer, other); v3.Item == "" || v3.Item == v2.Item {
		t.Fatalf("a new rendering re-raises the drift: %+v", v3)
	}
	e.noErrors(t)
}

// ACT-AGM-06/07：本人经普通决策回答（decision.answer，Web 与 POST /items/{id}/decide 同一命令）选的结果必须生效。
func TestPlainDecisionAnswerAppliesOverride(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	file := edited(e.view(t))
	v := e.check(t, observer, file)
	e.must(t, owner, "decision.answer", map[string]any{"id": v.Item, "choice": ChoiceOverride})
	again := e.check(t, observer, file)
	if again.Decision != driftOverride || again.Item != v.Item || len(e.needsInput(t)) != 0 {
		t.Fatalf("%+v", again)
	}
	if s := e.sync(t, file); !s.Write || s.Blocked || s.Content != e.view(t).Content {
		t.Fatalf("override chosen through a plain answer must let sync write: %+v", s)
	}
}

func TestPlainDecisionAnswerAppliesIgnore(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	file := edited(e.view(t))
	v := e.check(t, observer, file)
	e.must(t, owner, "decision.answer", map[string]any{"id": v.Item, "choice": ChoiceIgnore})
	again := e.check(t, observer, file)
	if again.Decision != driftIgnored || again.Item != v.Item || len(e.needsInput(t)) != 0 {
		t.Fatalf("%+v", again)
	}
	if s := e.sync(t, file); s.Write || !s.Blocked {
		t.Fatalf("ignore must not write: %+v", s)
	}
}

func TestPlainDecisionAnswerAppliesMerge(t *testing.T) {
	e := setup(t)
	mat := e.guideline(t, e.prj, projGuideline)
	file := edited(e.view(t))
	v := e.check(t, observer, file)
	e.must(t, owner, "decision.answer", map[string]any{"id": v.Item, "choice": ChoiceMerge})
	e.noErrors(t)
	var rev materials.Revision
	e.st.View(bg, func(tx *store.Tx) error {
		_, cur, _ := materials.Show(tx, mat, "")
		revs, err := materials.History(tx, mat)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range revs {
			if r.State == materials.StateProposed {
				rev = r
			}
		}
		if strings.Contains(cur.Content, "手改") {
			t.Fatal("the guideline must not change before the owner confirms")
		}
		return nil
	})
	if rev.ID == "" || !strings.Contains(rev.Content, "- 手改：发布前先跑冒烟") {
		t.Fatalf("merge through a plain answer must propose a revision holding the edit: %+v", rev)
	}
	// 同一选择经 Resolve 重试：重放同一修订，不重复提交。
	again, err := e.svc.Resolve(bg, owner, ResolveIn{Item: v.Item, Choice: ChoiceMerge})
	if err != nil || again.Revision != rev.ID {
		t.Fatalf("retry must replay the same revision: %+v %v", again, err)
	}
	e.must(t, owner, "revision.confirm", map[string]any{"id": rev.ID})
	if s := e.sync(t, file); s.State != StateCurrent {
		t.Fatalf("after confirm the edit is the rendering: %+v", s)
	}
}

// 不合格的合回（手改不在项目规范里）经普通回答也被拒，且回答整体回滚，事项仍在等本人决定。
func TestPlainDecisionAnswerRejectsInvalidMerge(t *testing.T) {
	e := setup(t)
	e.guideline(t, "", "## 全局\n\n- 说中文")
	e.guideline(t, e.prj, projGuideline)
	v := e.check(t, observer, strings.Replace(e.view(t).Content, "- 说中文", "- 说英文", 1))
	e.wantExit(t, domain.ExitUsage, owner, "decision.answer", map[string]any{"id": v.Item, "choice": ChoiceMerge})
	if it := e.item(t, v.Item); it.Phase != domain.PhaseNeedsInput {
		t.Fatal("a refused merge must leave the drift undecided")
	}
}

// ACT-AGM-08 的合回路径：已有手写文件、项目还没有规范——合回新建项目规范（proposed）。
func TestHandWrittenFileMergesIntoNewGuideline(t *testing.T) {
	e := setup(t)
	hand := "# 我们的约定\n\n- 提交前跑 make test\n"
	v := e.check(t, observer, hand)
	if v.State != StateDrift || v.Exit != ExitDrift {
		t.Fatalf("a hand-written file is drift on first check: %+v", v)
	}
	out := e.resolve(t, owner, v.Item, ChoiceMerge)
	e.st.View(bg, func(tx *store.Tx) error {
		m, err := materials.Get(tx, out.Material)
		if err != nil || m.Kind != materials.KindGuideline || m.Project != e.prj || m.CurrentRev != "" {
			t.Fatalf("a new project guideline with no current revision yet: %+v %v", m, err)
		}
		_, rev, _ := materials.Show(tx, out.Material, out.Revision)
		if rev.State != materials.StateProposed || !strings.Contains(rev.Content, "提交前跑 make test") {
			t.Fatalf("%+v", rev)
		}
		return nil
	})
	e.must(t, owner, "revision.confirm", map[string]any{"id": out.Revision})
	if !strings.Contains(e.view(t).Content, "提交前跑 make test") {
		t.Fatal("the merged text must be rendered after confirmation")
	}
}

// 合回只认项目部分：手改落在全局规范上，没有可合回的内容，明确拒绝而不是写出重复内容。
func TestMergeRejectsEditOutsideProjectGuideline(t *testing.T) {
	e := setup(t)
	e.guideline(t, "", "## 全局\n\n- 说中文")
	e.guideline(t, e.prj, projGuideline)
	file := strings.Replace(e.view(t).Content, "- 说中文", "- 说英文", 1)
	v := e.check(t, observer, file)
	if _, err := e.svc.Resolve(bg, owner, ResolveIn{Item: v.Item, Choice: ChoiceMerge}); exitOf(err) != domain.ExitUsage {
		t.Fatalf("want exit 2, got %v", err)
	}
	if it := e.item(t, v.Item); it.Phase != domain.PhaseNeedsInput {
		t.Fatal("a refused merge must leave the drift undecided")
	}
	if _, err := e.svc.Resolve(bg, owner, ResolveIn{Item: "itm_missing", Choice: ChoiceMerge}); exitOf(err) != domain.ExitNotFound {
		t.Fatalf("unknown drift item: %v", err)
	}
}

// ACT-AGM-09：默认分支上改了 AGENTS.md 的提交触发同一检测；无关提交不读文件。
func TestCommitTouchingAgentsMDTriggersSameDetection(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	rendered := e.view(t)
	var reads int
	content, exists := edited(rendered), true
	e.fetch = func(_ context.Context, r string) (string, bool, error) {
		reads++
		if r != repo {
			t.Fatalf("read the wrong repo: %s", r)
		}
		return content, exists, nil
	}

	if v, err := e.svc.Trigger(bg, repo, []string{"docs/arch.md", "sub/AGENTS.md"}); v != nil || err != nil || reads != 0 {
		t.Fatalf("unrelated commits must not trigger: %v %v reads=%d", v, err, reads)
	}
	v, err := e.svc.Trigger(bg, repo, []string{"README.md", "AGENTS.md"})
	if err != nil || v == nil || v.State != StateDrift || v.Item == "" {
		t.Fatalf("a hand edit pushed to the default branch must raise the drift item: %+v %v", v, err)
	}
	if it := e.item(t, v.Item); it.Phase != domain.PhaseNeedsInput || !strings.Contains(it.Body, "手改") {
		t.Fatalf("%+v", it)
	}
	if lc := e.view(t).LastCheck; lc == nil || lc.Host != "default-branch" || lc.State != StateDrift {
		t.Fatalf("the trigger is recorded as a check: %+v", lc)
	}
	// 同一份内容再触发：与 check 同一结论，不重复提。
	if v2, _ := e.svc.Trigger(bg, repo, []string{"AGENTS.md"}); v2.Item != v.Item || len(e.needsInput(t)) != 1 {
		t.Fatalf("%+v", v2)
	}
	// 文件被删：按过期（可直接写入）。
	exists = false
	if v3, err := e.svc.Trigger(bg, repo, []string{"AGENTS.md"}); err != nil || v3.State != StateStale {
		t.Fatalf("%+v %v", v3, err)
	}
	// 读取失败暴露真实错误，不伪造结论。
	e.fetch = func(context.Context, string) (string, bool, error) { return "", false, errors.New("reader down") }
	if _, err := e.svc.Trigger(bg, repo, []string{"AGENTS.md"}); err == nil || !strings.Contains(err.Error(), "reader down") {
		t.Fatalf("fetch failure must surface: %v", err)
	}
	// 没配读取器时明确报错。
	e.svc.opt.Fetch = nil
	if _, err := e.svc.Trigger(bg, repo, []string{"AGENTS.md"}); err == nil {
		t.Fatal("missing Fetch must be an error")
	}
}

// ACT-AGM-10：规范写"资金类决策可代答"并生效后，web_only 决策仍然拒绝代答（退出码 20），策略版本不变。
func TestGuidelineCannotRelaxWebOnly(t *testing.T) {
	e := setup(t)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Tag: "funds", Category: policy.CategoryFunds})
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	version := func() int {
		var v int
		e.st.View(bg, func(tx *store.Tx) (err error) { v, _, err = policy.Current(tx); return })
		return v
	}
	e.guideline(t, e.prj, "## 处理规则\n\n- 资金类决策可代答")
	e.guideline(t, "", "## 处理规则\n\n- 资金类决策可代答，白名单之外的人也可以")
	e.noErrors(t)
	if !strings.Contains(e.view(t).Content, "资金类决策可代答") {
		t.Fatal("the rule text is rendered into the file as plain text")
	}

	it := e.id(t, e.must(t, owner, "item.create", map[string]any{"title": "付款", "kind": "task", "phase": "open", "assignee": "wrk", "tags": []string{"funds"}, "project": e.prj}))
	e.must(t, worker, "decision.request", map[string]any{"id": it, "ask": map[string]any{"question": "付吗？",
		"options": []map[string]any{{"id": "yes", "label": "付"}, {"id": "no", "label": "不付", "next": "dropped"}}}})
	before := version()
	e.wantExit(t, domain.ExitWebOnly, delegate, "decision.answer", map[string]any{"id": it, "choice": "yes"})
	if got := e.item(t, it); got.Phase != domain.PhaseNeedsInput || got.Ask.Resolution != nil {
		t.Fatalf("the decision must stay open: %+v", got)
	}
	if version() != before {
		t.Fatal("a guideline must not change the policy")
	}
	e.must(t, owner, "decision.answer", map[string]any{"id": it, "choice": "yes"}) // 本人在 Web 仍可决定
}

func TestCheckAuthorizationAndValidation(t *testing.T) {
	e := setup(t)
	e.guideline(t, e.prj, projGuideline)
	ok := map[string]any{"repo": repo, "host": "m1", "content": "x"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range ok {
			m[a] = b
		}
		m[k] = v
		return m
	}
	e.wantExit(t, domain.ExitUnauthenticated, nil, "agents.check", ok)
	e.wantExit(t, domain.ExitNotFound, observer, "agents.check", with("repo", "https://example.com/unknown.git"))
	for name, in := range map[string]map[string]any{
		"no host":     with("host", ""),
		"no repo":     with("repo", " "),
		"two sources": with("missing", true),
		"no source":   {"repo": repo, "host": "m1"},
		"unknown key": with("path", "x"),
	} {
		if _, err := e.do(observer, "agents.check", in); exitOf(err) != domain.ExitUsage {
			t.Fatalf("%s: want exit 2, got %v", name, err)
		}
	}
	e.wantExit(t, domain.ExitPermissionDenied, observer, "agents.render", map[string]any{})
	if _, err := e.svc.Get(bg, "https://example.com/never.git"); exitOf(err) != domain.ExitNotFound {
		t.Fatalf("Get of an unknown repo: %v", err)
	}
	// 渲染记录可被一个从未检查过的仓库读取：last_check 为 null。
	if v := e.view(t); v.LastCheck != nil || v.Hash == "" {
		t.Fatalf("%+v", v)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	e := setup(t)
	tpl := template(t)
	fresh := func() *command.Executor { return command.New(e.st, time.Now) }
	if _, err := New(fresh(), Options{Template: strings.Replace(tpl, "{fixed_guidance}", "", 1), OnError: func(error) {}}); err == nil {
		t.Fatal("a template without a placeholder must be rejected")
	}
	if _, err := New(fresh(), Options{Template: tpl}); err == nil {
		t.Fatal("OnError is required")
	}
	if _, err := New(e.x, Options{Template: tpl, OnError: func(error) {}}); err == nil {
		t.Fatal("registering the commands twice must fail")
	}
}

// 能力包热加载后模板换新：不合格的模板被拒绝且保留旧模板；合格的下一次渲染才生效。
func TestSetTemplateAppliesToNextRender(t *testing.T) {
	e := setup(t)
	before := e.view(t).Hash
	tpl := template(t)
	if err := e.svc.SetTemplate(strings.Replace(tpl, "{fixed_guidance}", "", 1)); err == nil {
		t.Fatal("a template without a placeholder must be rejected")
	}
	e.must(t, owner, "agents.render", map[string]any{})
	if e.view(t).Hash != before {
		t.Fatal("a rejected template must not change rendering")
	}
	if err := e.svc.SetTemplate(tpl + "\n热加载新增的一行\n"); err != nil {
		t.Fatal(err)
	}
	e.must(t, owner, "agents.render", map[string]any{})
	if v := e.view(t); v.Hash == before || !strings.Contains(v.Content, "热加载新增的一行") {
		t.Fatalf("new template not used: %+v", v)
	}
	e.noErrors(t)
}

func TestDropEmptySectionsKeepsOtherTextVerbatim(t *testing.T) {
	got := dropEmptySections("前言\n\n## A\n\n内容\n\n## B\n\n  \n## C\n\n- x")
	if want := "前言\n\n## A\n\n内容\n\n## C\n\n- x"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if dropEmptySections("## 只有标题\n") != "" {
		t.Fatal("a guideline with only empty sections renders nothing")
	}
}
