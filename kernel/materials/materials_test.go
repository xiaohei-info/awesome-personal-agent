package materials

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

func agent(p string, lvl authz.Level) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: p, Via: domain.ViaCLI, Session: "s1"}, Level: lvl, CredentialAgentID: "agent_" + p}
}

// setTier 以本人身份设 agent 的档位（3b 设置，测试里直接写策略）。
func (e *env) setTier(t *testing.T, agent string, tier policy.Card) {
	t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.AgentCards[agent] = tier
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// pending 由本人建一份材料，再由档位只整理的 worker 改一次：改已有材料留待确认，返回那条待确认修订。
func (e *env) pending(t *testing.T, kind string) (base, rev Result) {
	t.Helper()
	base = e.must(t, owner, "", "material.commit", commitIn{Project: e.prj, Kind: kind, Title: "方案", Content: "v1", Reason: "初版"})
	rev = e.must(t, worker, "", "material.commit", commitIn{ID: base.Material, Content: "v2", Reason: "改", BasedOn: base.Revision})
	return base, rev
}

var (
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}
	worker   = agent("wrk", authz.LevelWorker)
	delegate = agent("dlg", authz.LevelDelegate)
	observer = agent("obs", authz.LevelObserver)
)

type env struct {
	st  *store.Store
	x   *command.Executor
	now time.Time
	prj string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e.x = command.New(st, func() time.Time { e.now = e.now.Add(time.Minute); return e.now })
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := Register(e.x); err != nil {
		t.Fatal(err)
	}
	var out struct{ ID string }
	b, _ := json.Marshal(map[string]any{"name": "acc-open"})
	r, err := e.x.Do(bg, command.Request{Identity: owner, Command: "project.create", Input: b, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(r.Value, &out)
	e.prj = out.ID
	return e
}

func (e *env) do(id command.Identity, evidence, cmd string, in any) (Result, error) {
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", WebURL: "http://web/m"})
	var out Result
	if err == nil {
		json.Unmarshal(r.Value, &out)
	}
	return out, err
}

func (e *env) must(t *testing.T, id command.Identity, evidence, cmd string, in any) Result {
	t.Helper()
	r, err := e.do(id, evidence, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func code(t *testing.T, err error, want domain.Code) {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != want {
		t.Fatalf("want %s, got %v", want, err)
	}
}

func (e *env) view(t *testing.T, fn func(tx *store.Tx) error) {
	t.Helper()
	if err := e.st.View(bg, fn); err != nil {
		t.Fatal(err)
	}
}

func (e *env) events(t *testing.T, object string) []domain.Event {
	t.Helper()
	var out []domain.Event
	e.view(t, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, object)
		for _, en := range es {
			out = append(out, en.Event)
		}
		return err
	})
	return out
}

func hasEvent(es []domain.Event, typ string) bool {
	for _, e := range es {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func (e *env) show(t *testing.T, id, rev string) (Material, Revision) {
	t.Helper()
	var m Material
	var v Revision
	e.view(t, func(tx *store.Tx) (err error) { m, v, err = Show(tx, id, rev); return })
	return m, v
}

func (e *env) newNote(t *testing.T, content string) Result {
	return e.must(t, worker, "", "material.commit", commitIn{Project: e.prj, Kind: "minutes", Title: "周会", Content: content, Reason: "周会"})
}

// flow-material-owned / ACT-MAT-01：纪要类直接生效，actor 为 agent，事件流有 material.revised。
func TestDirectKindBecomesCurrentWithAgentActor(t *testing.T) {
	e := setup(t)
	r := e.newNote(t, "登录超时的根因")
	if r.State != StateCurrent {
		t.Fatalf("state %s", r.State)
	}
	m, v := e.show(t, r.Material, "")
	if m.CurrentRev != r.Revision || v.Content != "登录超时的根因" || v.Actor.Type != domain.ActorAgent || v.Actor.Principal != "wrk" {
		t.Fatalf("material %+v revision %+v", m, v)
	}
	if !hasEvent(e.events(t, r.Material), domain.EvMaterialRevised) {
		t.Fatal("no material.revised event")
	}
}

// ACT-MAT-02：新建材料直接生效；档位只整理的 Agent 改已有材料进入确认，不成为 current。
func TestProposedKindQueuesForConfirmation(t *testing.T) {
	e := setup(t)
	first := e.must(t, worker, "", "material.commit", commitIn{Project: e.prj, Kind: "design", Title: "方案", Content: "v1", Reason: "初版"})
	if first.State != StateCurrent {
		t.Fatalf("new material: state %s", first.State)
	}
	r := e.must(t, worker, "", "material.commit", commitIn{ID: first.Material, Content: "v2", Reason: "改", BasedOn: first.Revision})
	if r.State != StateProposed {
		t.Fatalf("edit: state %s", r.State)
	}
	if m, _ := e.show(t, r.Material, r.Revision); m.CurrentRev != first.Revision {
		t.Fatal("proposed revision became current")
	}
	var p []Pending
	e.view(t, func(tx *store.Tx) (err error) { p, err = PendingRevisions(tx); return })
	if len(p) != 1 || p[0].Revision.ID != r.Revision || p[0].BaselineChanged {
		t.Fatalf("pending %+v", p)
	}
	if !hasEvent(e.events(t, r.Material), domain.EvMaterialRevisionProposed) {
		t.Fatal("no proposed event")
	}
	// 放开二档后，同一个 Agent 的改动直接生效。
	e.setTier(t, "agent_wrk", policy.DefaultCard().With(policy.GroupDecide))
	if d := e.must(t, worker, "", "material.commit", commitIn{ID: first.Material, Content: "v3", Reason: "再改", BasedOn: first.Revision}); d.State != StateCurrent {
		t.Fatalf("edit with decide tier: state %s", d.State)
	}
}

// ACT-MAT-03：确认的成为 current 且旧修订 superseded；拒绝的不生效且内容保留。
func TestConfirmSupersedesPreviousAndRejectKeepsContent(t *testing.T) {
	e := setup(t)
	a, b := e.pending(t, "design")
	e.must(t, owner, "", "revision.confirm", confirmIn{ID: b.Revision})
	m, cur := e.show(t, a.Material, "")
	if m.CurrentRev != b.Revision || cur.Content != "v2" {
		t.Fatalf("current %+v", m)
	}
	_, old := e.show(t, a.Material, a.Revision)
	if old.State != StateSuperseded || old.Content != "v1" {
		t.Fatalf("old %+v", old)
	}
	c := e.must(t, worker, "", "material.commit", commitIn{ID: a.Material, Content: "v3", Reason: "再改", BasedOn: b.Revision})
	e.must(t, owner, "", "revision.reject", rejectIn{ID: c.Revision, Reason: "不对"})
	m, _ = e.show(t, a.Material, "")
	_, rej := e.show(t, a.Material, c.Revision)
	if m.CurrentRev != b.Revision || rej.State != StateSuperseded || rej.Content != "v3" {
		t.Fatalf("rejected %+v current %s", rej, m.CurrentRev)
	}
	if !hasEvent(e.events(t, a.Material), EvRevisionRejected) {
		t.Fatal("rejection not recorded")
	}
	// 只认第一次：已处理的修订再次确认返回 6。
	_, err := e.do(owner, "", "revision.confirm", confirmIn{ID: c.Revision})
	code(t, err, domain.CodeConflict)
}

// ACT-MAT-04：连续修订追加不改，每条有 actor、reason、based_on，diff 正确。
func TestRevisionsAreAppendOnlyWithProvenanceAndDiff(t *testing.T) {
	e := setup(t)
	r1 := e.newNote(t, "a\nb\nc")
	e.setTier(t, "agent_wrk", policy.DefaultCard().With(policy.GroupDecide))
	r2 := e.must(t, worker, "", "material.commit", commitIn{ID: r1.Material, Content: "a\nB\nc", Reason: "二", BasedOn: r1.Revision})
	r3 := e.must(t, worker, "", "material.commit", commitIn{ID: r1.Material, Content: "a\nB\nc\nd", Reason: "三", BasedOn: r2.Revision})
	var h []Revision
	e.view(t, func(tx *store.Tx) (err error) { h, err = History(tx, r1.Material); return })
	if len(h) != 3 || h[0].Content != "a\nb\nc" || h[1].BasedOn != r1.Revision || h[2].BasedOn != r2.Revision || h[2].Reason != "三" || h[1].Actor.Principal != "wrk" {
		t.Fatalf("history %+v", h)
	}
	if h[0].State != StateSuperseded || h[2].State != StateCurrent || r3.State != StateCurrent {
		t.Fatalf("states %+v", h)
	}
	var d []DiffLine
	if d = Diff(h[0].Content, h[1].Content); len(d) != 4 || d[1] != (DiffLine{"-", "b"}) || d[2] != (DiffLine{"+", "B"}) {
		t.Fatalf("diff %+v", d)
	}
	// 存储层拒绝改写已写入的内容。
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE revisions SET content = 'x' WHERE id = ?", r1.Revision)
		return err
	})
	if err == nil {
		t.Fatal("revision content was rewritten")
	}
}

// ACT-MAT-05：同一 based_on 提交两次 direct 修订，后一次退出码 6，不静默覆盖。
func TestDirectBaselineMismatchConflicts(t *testing.T) {
	e := setup(t)
	r1 := e.newNote(t, "v1")
	e.setTier(t, "agent_wrk", policy.DefaultCard().With(policy.GroupDecide))
	e.must(t, worker, "", "material.commit", commitIn{ID: r1.Material, Content: "v2", Reason: "一", BasedOn: r1.Revision})
	_, err := e.do(worker, "", "material.commit", commitIn{ID: r1.Material, Content: "v2'", Reason: "二", BasedOn: r1.Revision})
	code(t, err, domain.CodeConflict)
	if de := err.(*domain.Error); de.ExitCode() != domain.ExitConflict {
		t.Fatalf("exit %d", de.ExitCode())
	}
	m, cur := e.show(t, r1.Material, "")
	if cur.Content != "v2" {
		t.Fatalf("current %+v %+v", m, cur)
	}
	var h []Revision
	e.view(t, func(tx *store.Tx) (err error) { h, err = History(tx, r1.Material); return })
	if len(h) != 2 {
		t.Fatalf("conflicting commit left %d revisions", len(h))
	}
}

// ACT-MAT-06：旧 based_on 的 proposed 修订可入队，确认时提示基线变化，可选仍然采用或基于当前重提。
func TestProposedStaleBaselineNeedsExplicitChoice(t *testing.T) {
	e := setup(t)
	a := e.must(t, worker, "", "material.commit", commitIn{Project: e.prj, Kind: "design", Title: "方案", Content: "v1", Reason: "初版"})
	stale := e.must(t, worker, "", "material.commit", commitIn{ID: a.Material, Content: "mine", Reason: "旧基线", BasedOn: a.Revision})
	fresh := e.must(t, worker, "", "material.commit", commitIn{ID: a.Material, Content: "theirs", Reason: "另一份", BasedOn: a.Revision})
	e.must(t, owner, "", "revision.confirm", confirmIn{ID: fresh.Revision}) // 当前变为 fresh，stale 的基线过期

	var p []Pending
	e.view(t, func(tx *store.Tx) (err error) { p, err = PendingRevisions(tx); return })
	if len(p) != 1 || p[0].Revision.ID != stale.Revision || !p[0].BaselineChanged {
		t.Fatalf("pending %+v", p)
	}
	_, err := e.do(owner, "", "revision.confirm", confirmIn{ID: stale.Revision})
	code(t, err, domain.CodeConflict)
	if !strings.Contains(err.Error(), "baseline changed") {
		t.Fatalf("no baseline hint: %v", err)
	}
	// 基于当前重提：旧提议作废，新提议基于当前，仍待确认。
	rb := e.must(t, owner, "", "revision.confirm", confirmIn{ID: stale.Revision, OnStale: OnStaleRebase})
	if rb.State != StateProposed {
		t.Fatalf("rebase %+v", rb)
	}
	_, nv := e.show(t, a.Material, rb.Revision)
	_, old := e.show(t, a.Material, stale.Revision)
	if nv.BasedOn != fresh.Revision || nv.Content != "mine" || old.State != StateSuperseded {
		t.Fatalf("rebased %+v old %+v", nv, old)
	}
	// 仍然采用：把基线过期的提议直接生效。
	late := e.must(t, worker, "", "material.commit", commitIn{ID: a.Material, Content: "late", Reason: "旧", BasedOn: a.Revision})
	adopt := e.must(t, owner, "", "revision.confirm", confirmIn{ID: late.Revision, OnStale: OnStaleAdopt})
	if _, cur := e.show(t, a.Material, ""); adopt.State != StateCurrent || cur.Content != "late" {
		t.Fatalf("adopt %+v %+v", adopt, cur)
	}
}

// ACT-MAT-07：回退是 3a（档位只整理的 delegate 得 23，worker 在钥匙范围外）；回退新增一条内容等于旧快照的修订，历史不改。
func TestRevertIsWebOnlyAndAppendsRevision(t *testing.T) {
	e := setup(t)
	r1 := e.newNote(t, "原文")
	e.setTier(t, "agent_wrk", policy.DefaultCard().With(policy.GroupDecide))
	r2 := e.must(t, worker, "", "material.commit", commitIn{ID: r1.Material, Content: "被 agent 改坏", Reason: "改", BasedOn: r1.Revision})
	in := revertIn{ID: r1.Material, To: r1.Revision, Reason: "回退"}
	_, err := e.do(delegate, "", "material.revert", in)
	code(t, err, domain.CodeTierClosed)
	_, err = e.do(worker, "", "material.revert", in)
	code(t, err, domain.CodePermissionDenied)
	if _, cur := e.show(t, r1.Material, ""); cur.ID != r2.Revision {
		t.Fatal("token revert changed current")
	}
	rv := e.must(t, owner, "", "material.revert", in)
	var h []Revision
	e.view(t, func(tx *store.Tx) (err error) { h, err = History(tx, r1.Material); return })
	if len(h) != 3 || h[2].ID != rv.Revision || h[2].Content != "原文" || h[2].BasedOn != r2.Revision || h[2].State != StateCurrent ||
		h[1].Content != "被 agent 改坏" || h[0].Content != "原文" || h[2].Actor.Type != domain.ActorHuman {
		t.Fatalf("history %+v", h)
	}
	if _, err := e.do(owner, "", "material.revert", revertIn{ID: r1.Material, To: rv.Revision, Reason: "x"}); err == nil {
		t.Fatal("reverting to the current revision must fail")
	}
}

// ACT-MAT-12：observer 不能提交修订（退出码 4），也没有任何写入。
func TestObserverCannotCommit(t *testing.T) {
	e := setup(t)
	r := e.newNote(t, "v1")
	_, err := e.do(observer, "", "material.commit", commitIn{ID: r.Material, Content: "x", Reason: "x", BasedOn: r.Revision})
	code(t, err, domain.CodePermissionDenied)
	_, err = e.do(observer, "", "material.commit", commitIn{Project: e.prj, Kind: "minutes", Title: "t", Content: "x", Reason: "x"})
	code(t, err, domain.CodePermissionDenied)
	var ms []Material
	e.view(t, func(tx *store.Tx) (err error) { ms, err = List(tx, ""); return })
	if len(ms) != 1 {
		t.Fatalf("materials %+v", ms)
	}
}

// flow-agents-md / ACT-AGM-01：规范恒留待确认（新建也是）；规范生效是 3a：档位未放开的 delegate 确认或拒绝得 23，
// worker 在钥匙范围外；Web 确认后生效并记 guideline.activated；放开 3a 的 Agent 也能确认。
func TestGuidelineActivationIsWebOnly(t *testing.T) {
	e := setup(t)
	r := e.must(t, delegate, "", "material.commit", commitIn{Project: e.prj, Kind: KindGuideline, Title: "规范", Content: "## 处理规则\n先问", Reason: "初版"})
	if r.State != StateProposed {
		t.Fatalf("state %s", r.State)
	}
	_, err := e.do(delegate, "", "revision.confirm", confirmIn{ID: r.Revision})
	code(t, err, domain.CodeTierClosed)
	e.setTier(t, "agent_dlg", policy.DefaultCard().With(policy.GroupDecide)) // 二档放开也不够
	_, err = e.do(delegate, "", "revision.reject", rejectIn{ID: r.Revision})
	code(t, err, domain.CodeTierClosed)
	_, err = e.do(worker, "", "revision.confirm", confirmIn{ID: r.Revision})
	code(t, err, domain.CodePermissionDenied)
	es := e.events(t, r.Material)
	if hasEvent(es, domain.EvGuidelineActivated) || !hasEvent(es, domain.EvCommandRejected) {
		t.Fatalf("events %+v", es)
	}
	if m, _ := e.show2(t, r.Material); m.CurrentRev != "" {
		t.Fatal("guideline became current without the owner")
	}
	e.must(t, owner, "", "revision.confirm", confirmIn{ID: r.Revision})
	if m, v := e.show(t, r.Material, ""); m.CurrentRev != r.Revision || v.State != StateCurrent {
		t.Fatalf("not activated: %+v", m)
	}
	if !hasEvent(e.events(t, r.Material), domain.EvGuidelineActivated) {
		t.Fatal("no guideline.activated event")
	}
	e.setTier(t, "agent_dlg", policy.DefaultCard().With(policy.GroupDecide, policy.GroupRules))
	r2 := e.must(t, delegate, "", "material.commit", commitIn{ID: r.Material, Content: "## 处理规则\n先问再做", Reason: "改", BasedOn: r.Revision})
	if r2.State != StateProposed {
		t.Fatalf("guideline edit: state %s", r2.State)
	}
	e.must(t, delegate, "", "revision.confirm", confirmIn{ID: r2.Revision})
}

func (e *env) show2(t *testing.T, id string) (Material, error) {
	t.Helper()
	var m Material
	err := e.st.View(bg, func(tx *store.Tx) (err error) { m, err = Get(tx, id); return })
	return m, err
}

// 确认普通材料的修订是二档：档位只整理的 delegate 得 23，放开后可以确认。
func TestDelegateConfirmsNonGuidelineWithEvidenceOnly(t *testing.T) {
	e := setup(t)
	_, r := e.pending(t, "requirement")
	_, err := e.do(delegate, "", "revision.confirm", confirmIn{ID: r.Revision})
	code(t, err, domain.CodeTierClosed)
	e.setTier(t, "agent_dlg", policy.DefaultCard().With(policy.GroupDecide))
	e.must(t, delegate, "", "revision.confirm", confirmIn{ID: r.Revision})
	if m, _ := e.show(t, r.Material, ""); m.CurrentRev != r.Revision {
		t.Fatal("not confirmed")
	}
}

// 托管型不能 commit：退出码 6 并提示仓库路径；托管登记本身不在本包，这里以一行登记作为测试边界。
func TestHostedMaterialCannotBeCommitted(t *testing.T) {
	e := setup(t)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return insertMaterial(tx, Material{ID: "mat_hosted", Project: e.prj, Kind: "design", Title: "设计", Location: "git@example.test/repo.git:docs/design.md"})
	}); err != nil {
		t.Fatal(err)
	}
	_, err := e.do(worker, "", "material.commit", commitIn{ID: "mat_hosted", Content: "x", Reason: "x"})
	code(t, err, domain.CodeConflict)
	if !strings.Contains(err.Error(), "docs/design.md") {
		t.Fatalf("no path hint: %v", err)
	}
}

func TestCommitValidation(t *testing.T) {
	e := setup(t)
	ok := commitIn{Project: e.prj, Kind: "minutes", Title: "t", Content: "c", Reason: "r"}
	for name, mod := range map[string]func(*commitIn){
		"no reason":       func(c *commitIn) { c.Reason = " " },
		"no content":      func(c *commitIn) { c.Content = "" },
		"unknown kind":    func(c *commitIn) { c.Kind = "memo" },
		"no title":        func(c *commitIn) { c.Title = "" },
		"no project":      func(c *commitIn) { c.Project = "" },
		"unknown project": func(c *commitIn) { c.Project = "prj_nope" },
		"base on new":     func(c *commitIn) { c.BasedOn = "rev_x" },
	} {
		c := ok
		mod(&c)
		if _, err := e.do(worker, "", "material.commit", c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r := e.must(t, worker, "", "material.commit", ok)
	if _, err := e.do(worker, "", "material.commit", commitIn{ID: "mat_nope", Content: "c", Reason: "r"}); err == nil {
		t.Error("unknown material accepted")
	} else {
		code(t, err, domain.CodeNotFound)
	}
	if _, err := e.do(worker, "", "material.commit", commitIn{ID: r.Material, Kind: "design", Content: "c", Reason: "r", BasedOn: r.Revision}); err == nil {
		t.Error("kind change accepted")
	}
	// 能力包扩展的 kind。
	e2 := setup(t)
	e2.x = command.New(e2.st, nil)
	if err := RegisterWith(e2.x, retroPack{}); err != nil {
		t.Fatal(err)
	}
	// 能力包声明的 kind：新建材料直接生效（§12.4）。
	if r, err := e2.do(worker, "", "material.commit", commitIn{Project: e2.prj, Kind: "retro", Title: "t", Content: "c", Reason: "r"}); err != nil || r.State != StateCurrent {
		t.Errorf("extended kind: state %q err %v", r.State, err)
	}
}

func TestOneGuidelinePerScopeAndGlobalGuideline(t *testing.T) {
	e := setup(t)
	g := commitIn{Project: e.prj, Kind: KindGuideline, Title: "规范", Content: "x", Reason: "r"}
	e.must(t, worker, "", "material.commit", g)
	_, err := e.do(worker, "", "material.commit", g)
	code(t, err, domain.CodeConflict)
	g.Project = ""
	e.must(t, worker, "", "material.commit", g) // 全局规范
	g.Kind, g.Title = "minutes", "t"
	if _, err := e.do(worker, "", "material.commit", g); err == nil {
		t.Fatal("global non-guideline accepted")
	}
}

func TestListFiltersByProject(t *testing.T) {
	e := setup(t)
	e.newNote(t, "x")
	e.must(t, worker, "", "material.commit", commitIn{Kind: KindGuideline, Title: "全局", Content: "x", Reason: "r"})
	var in, all []Material
	e.view(t, func(tx *store.Tx) (err error) {
		if in, err = List(tx, e.prj); err != nil {
			return
		}
		all, err = List(tx, "")
		return
	})
	if len(in) != 1 || len(all) != 2 {
		t.Fatalf("in %d all %d", len(in), len(all))
	}
	var err error
	e.view(t, func(tx *store.Tx) error { _, _, err = Show(tx, in[0].ID, ""); return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.view(t, func(tx *store.Tx) error { _, _, err = Show(tx, all[0].ID, "rev_nope"); return nil })
	code(t, err, domain.CodeNotFound)
	e.view(t, func(tx *store.Tx) error {
		g, _ := Get(tx, all[0].ID)
		if g.Kind != KindGuideline {
			g, _ = Get(tx, all[1].ID)
		}
		_, _, err = Show(tx, g.ID, "") // 规范尚无当前修订
		return nil
	})
	code(t, err, domain.CodeNotFound)
}

func TestDiffEdges(t *testing.T) {
	if d := Diff("a", "a"); len(d) != 1 || d[0].Op != " " {
		t.Fatalf("%+v", d)
	}
	d := Diff("a\nb", "c")
	if len(d) != 3 || d[0].Op != "-" || d[2].Op != "+" {
		t.Fatalf("%+v", d)
	}
	if d := Diff("a\nb\nc", "a"); len(d) != 3 || d[1] != (DiffLine{"-", "b"}) {
		t.Fatalf("%+v", d)
	}
	if d := Diff("a", "a\nb\nc"); len(d) != 3 || d[2] != (DiffLine{"+", "c"}) {
		t.Fatalf("%+v", d)
	}
}

// 幂等：同一幂等键重放不重复写入修订。
func TestCommitIdempotentReplay(t *testing.T) {
	e := setup(t)
	b, _ := json.Marshal(commitIn{Project: e.prj, Kind: "minutes", Title: "t", Content: "c", Reason: "r"})
	req := command.Request{Identity: worker, Command: "material.commit", Input: b, IdemKey: "k1", Owner: "owner"}
	r1, err := e.x.Do(bg, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.x.Do(bg, req)
	if err != nil || !r2.Replayed || string(r1.Value) != string(r2.Value) {
		t.Fatalf("replay %+v %v", r2, err)
	}
	var ms []Material
	e.view(t, func(tx *store.Tx) (err error) { ms, err = List(tx, ""); return })
	if len(ms) != 1 {
		t.Fatalf("materials %d", len(ms))
	}
}

// 确认、拒绝与回退的失败语义：未知或已处理的修订、非法取舍、跨材料引用、proposed 不能当回退目标。
func TestRevisionCommandFailures(t *testing.T) {
	e := setup(t)
	_, a := e.pending(t, "design")
	other := e.newNote(t, "别的材料")
	for name, c := range map[string]struct {
		cmd string
		in  any
		is  domain.Code
	}{
		"confirm no id":        {"revision.confirm", confirmIn{}, domain.CodeInvalidRequest},
		"confirm unknown":      {"revision.confirm", confirmIn{ID: "rev_nope"}, domain.CodeNotFound},
		"confirm bad on_stale": {"revision.confirm", confirmIn{ID: a.Revision, OnStale: "x"}, domain.CodeInvalidRequest},
		"reject unknown":       {"revision.reject", rejectIn{ID: "rev_nope"}, domain.CodeNotFound},
		"revert no reason":     {"material.revert", revertIn{ID: other.Material, To: other.Revision}, domain.CodeInvalidRequest},
		"revert unknown mat":   {"material.revert", revertIn{ID: "mat_nope", To: "x", Reason: "r"}, domain.CodeNotFound},
		"revert other's rev":   {"material.revert", revertIn{ID: other.Material, To: a.Revision, Reason: "r"}, domain.CodeNotFound},
		"revert proposed":      {"material.revert", revertIn{ID: a.Material, To: a.Revision, Reason: "r"}, domain.CodeConflict},
	} {
		if _, err := e.do(owner, "", c.cmd, c.in); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			var de *domain.Error
			if !errors.As(err, &de) || de.Code != c.is {
				t.Errorf("%s: got %v, want %s", name, err, c.is)
			}
		}
	}
	e.must(t, owner, "", "revision.reject", rejectIn{ID: a.Revision})
	_, err := e.do(owner, "", "revision.reject", rejectIn{ID: a.Revision})
	code(t, err, domain.CodeConflict)
	// proposed 的 based_on 必须是本材料的修订。
	_, err = e.do(worker, "", "material.commit", commitIn{ID: a.Material, Content: "x", Reason: "r", BasedOn: other.Revision})
	code(t, err, domain.CodeInvalidRequest)
	if got := orNone(""); got != "(none)" {
		t.Fatal(got)
	}
}

func TestAddHostedAndSetLocationGuards(t *testing.T) {
	e := setup(t)
	var id string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := AddHosted(tx, e.prj, "design", "t", nil, " "); err == nil {
			t.Fatal("blank location must be rejected")
		}
		m, err := AddHosted(tx, e.prj, "design", " t ", nil, "r:p.md")
		id = m.ID
		if err != nil || m.Title != "t" || m.Location != "r:p.md" {
			t.Fatalf("%+v %v", m, err)
		}
		if err := SetLocation(tx, id, "r:q.md"); err != nil {
			t.Fatal(err)
		}
		code(t, SetLocation(tx, "mat_none", "x"), domain.CodeConflict)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.view(t, func(tx *store.Tx) error {
		if m, err := Get(tx, id); err != nil || m.Location != "r:q.md" || m.CurrentRev != "" {
			t.Fatalf("%+v %v", m, err)
		}
		return nil
	})
}

// retroPack 是一个声明了 material_kinds 的能力包来源（retro 默认 proposed）。
type retroPack struct{}

func (retroPack) MaterialKinds() map[string]string {
	return map[string]string{"retro": policy.WriteProposed}
}
