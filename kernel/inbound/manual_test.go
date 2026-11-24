package inbound

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// cliOwner 是主机终端上的本人（apa merge 离线运行时的身份，同 apa serve 读主机配置文件的 hostOwner：与 Web 上的本人同级）。
var cliOwner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

// proposal 让来源 src 送来一条事件，起草成标题 title、项目 project（空为不属于任何项目）的建议。
func (e *env) proposal(src, id, title, project string) domain.Item {
	e.t.Helper()
	e.draft(fmt.Sprintf(`{"title":%q,"kind":"task","project":%q,"recommend":"accept","reason":"r"}`, title, project))
	switch project { // 夹具包里定项目的规则：代码仓库链接归 web，x.fixed 归 mail，x.prop 不归项目
	case e.web:
		e.emit(src, "ticket", ev(id, "ticket.opened", title, map[string]any{"link": "https://git.example/org/web/issues/" + id}))
	case e.mail:
		e.emit(src, "exits", ev(id, "x.fixed", title, nil))
	default:
		e.emit(src, "exits", ev(id, "x.prop", title, nil))
	}
	it := e.byTitle(title)
	if it.Project != project {
		e.t.Fatalf("%s: project %q, want %q", title, it.Project, project)
	}
	return it
}

func (e *env) head() (n int64) {
	e.view(func(tx *store.Tx) { n, _ = eventlog.Head(tx) })
	return
}

// sweepNow 跑一次维护循环里的归类，先等过攒批的等待时间。
func (e *env) sweepNow() {
	e.t.Helper()
	e.now = e.now.Add(31 * time.Minute)
	if err := e.svc.Sweep(bg); err != nil {
		e.t.Fatal(err)
	}
}

func mergeReply(action string) modelstub.Reply {
	return modelstub.Reply{Content: `{"decisions":[{"new":"N1","action":"` + action + `","target":"E1","reason":"N1 与 E1 是同一件事","confident":true}]}`}
}

// 合并护栏：相似但属于别的项目的记录不当候选、不送模型，新建议按规则留着；同一项目的照常合并，去处记下被并那条的原始标题与 origin.ref。
// 护栏加之前，跨项目的那条会作为候选送给模型并被合并。
func TestMergeGuardBlocksCrossProjectCandidates(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame; p.Merge.BatchSize = 2 })
	e.stub.When(batchMark, mergeReply(domain.SweepMerge))
	a := e.proposal("src_a", "a1", "季度预算评审会议", e.web)
	e.sweepNow()
	if e.item(a.ID).Proposal.Sweep.State != "done" || e.batchCalls() != 0 {
		t.Fatalf("the first proposal has nothing to compare: %+v", e.item(a.ID).Proposal.Sweep)
	}

	b := e.proposal("src_b", "b1", "季度预算评审会议改到周五", e.mail)
	e.sweepNow()
	got := e.item(b.ID)
	if got.Phase != domain.PhaseProposed || got.Proposal.Sweep.Action != domain.SweepKeep || !strings.Contains(got.Proposal.Sweep.Why, "别的项目") || e.batchCalls() != 0 {
		t.Fatalf("a look-alike in another project must not be a candidate: %s %+v, calls %d", got.Phase, got.Proposal.Sweep, e.batchCalls())
	}
	if ap := e.item(a.ID).Proposal; len(ap.Merges) != 0 || len(ap.Absorbed) != 0 {
		t.Fatalf("target untouched: %+v", ap)
	}

	c := e.proposal("src_c", "c1", "季度预算评审会议改期", e.web)
	e.sweepNow()
	if e.batchCalls() != 1 {
		t.Fatalf("same project goes to the model, got %d calls", e.batchCalls())
	}
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, batchMark) && strings.Contains(r.BodyText, b.Title) {
			t.Fatal("the cross-project record must not be sent as a candidate")
		}
	}
	if got := e.item(c.ID); got.Phase != domain.PhaseDropped || got.Proposal.Sweep.Action != domain.SweepMerge || got.Proposal.Sweep.Target != a.ID {
		t.Fatalf("same-project merge: %s %+v", got.Phase, got.Proposal.Sweep)
	}
	ab := e.item(a.ID).Proposal.Absorbed
	if len(ab) != 1 || ab[0].Item != c.ID || ab[0].Title != c.Title || ab[0].Source != "src_c" || ab[0].Ref == "" || ab[0].Ref != c.Origin.Ref ||
		ab[0].Action != domain.SweepMerge || ab[0].Project != e.web || !ab[0].Created.Equal(c.CreatedAt) {
		t.Fatalf("the target keeps the folded title and origin.ref: %+v (origin %+v)", ab, c.Origin)
	}
	e.verify()
}

// 护栏口径：只有两边都有项目且不同才拦。无→无、无→有、有→无、同项目放行；有 A→有 B 拦。
// 在落账处验（绕过规则层直接给 inbound.sweep），规则层用的是同一个 sameOwner。
func TestSweepGuardOnlyBlocksTwoDifferentProjects(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame; p.Merge.BatchSize = 50 })
	for i, c := range []struct {
		name     string
		from, to string
		fold     bool
	}{
		{"无→无", "", "", true},
		{"无→有", "", e.web, true},
		{"有→无", e.web, "", true},
		{"同项目", e.web, e.web, true},
		{"有A→有B", e.mail, e.web, false},
	} {
		to := e.proposal("src_t", fmt.Sprintf("t%d", i), fmt.Sprintf("供应商合同续签 去处%d", i), c.to)
		x := e.proposal("src_x", fmt.Sprintf("x%d", i), fmt.Sprintf("供应商合同续签 新到%d", i), c.from)
		if err := e.svc.settle(bg, system, sweepIn{Item: x.ID, Batch: "mb_test", Action: domain.SweepMerge, Target: to.ID, By: "generate", Why: "同一件事"}); err != nil {
			t.Fatal(err)
		}
		got, ap := e.item(x.ID), e.item(to.ID).Proposal
		if c.fold {
			if got.Phase != domain.PhaseDropped || got.Proposal.Sweep.Action != domain.SweepMerge || len(ap.Absorbed) != 1 || ap.Absorbed[0].Item != x.ID {
				t.Fatalf("%s must fold: %s %+v, target %+v", c.name, got.Phase, got.Proposal.Sweep, ap.Absorbed)
			}
			continue
		}
		if got.Phase != domain.PhaseProposed || got.Proposal.Sweep.Action != domain.SweepKeep || !strings.Contains(got.Proposal.Sweep.Why, "另一个项目") {
			t.Fatalf("%s must be refused at apply: %s %+v", c.name, got.Phase, got.Proposal.Sweep)
		}
		if len(ap.Merges) != 0 || len(ap.Absorbed) != 0 || !slices.Equal(ap.Sources, to.Proposal.Sources) {
			t.Fatalf("%s: target untouched: %+v", c.name, ap)
		}
	}
	e.verify()
}

// 补打待归类与手动合并：补打只给没有标记的建议（含迁移来的、没有建议信息与消息的），幂等，事件记操作者；补打的不被维护循环自动归类；
// 演练不调模型、不写账本，真跑的规则层结果与演练逐字段相同；第二遍零变化；判定重复同样在去处留下原始标题与 origin.ref，撤销时删掉。
func TestMarkPendingThenManualMerge(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame; p.Merge.BatchSize = 2 })
	tgt := e.proposal("src_mail", "m1", "整理报销单据 R2201", "")
	e.sweepNow() // 只有它一条：留着
	var made struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "整理报销单据 R2201（清单）", "phase": "proposed", "reason": "migrate",
		"origin": map[string]any{"source": "agent:vault-migrate", "ref": "todo-migrate:abc"}}), &made)
	mig := e.item(made.ID)
	if mig.Proposal != nil {
		t.Fatalf("a migrated proposal has no proposal info: %+v", mig.Proposal)
	}
	f := ManualFilter{Source: "agent:vault-*"}

	// 补打：演练零写入；真跑只补这一条，记操作者；第二遍零变化。
	h := e.head()
	rep, err := e.svc.MarkPending(bg, cliOwner, "tester", f, false)
	if err != nil || !rep.DryRun || len(rep.Marked) != 1 || rep.Marked[0].Item != mig.ID || e.head() != h {
		t.Fatalf("dry mark: %+v %v, head %d→%d", rep, err, h, e.head())
	}
	if rep, err = e.svc.MarkPending(bg, cliOwner, "tester", f, true); err != nil || len(rep.Marked) != 1 {
		t.Fatalf("mark: %+v %v", rep, err)
	}
	sw := e.item(mig.ID).Proposal.Sweep
	if sw == nil || sw.State != "pending" || sw.Backfill == nil || sw.Backfill.By != "tester" {
		t.Fatalf("marked pending with the operator: %+v", sw)
	}
	es := e.eventsOf(mig.ID)
	last := es[len(es)-1]
	if last.Actor.Type != domain.ActorHuman || last.Actor.Principal != "owner" || !strings.Contains(last.Reason, "tester") {
		t.Fatalf("audit event: %+v", last)
	}
	h = e.head()
	if rep, err = e.svc.MarkPending(bg, cliOwner, "tester", ManualFilter{}, true); err != nil || len(rep.Marked) != 0 || e.head() != h || rep.Skipped["already_marked"] != 2 {
		t.Fatalf("second mark is a no-op: %+v %v", rep, err)
	}

	// 补打的不被维护循环归类，等多久都一样。
	e.now = e.now.Add(2 * time.Hour)
	e.sweepNow()
	if !pending(e.item(mig.ID)) || e.batchCalls() != 0 || e.head() != h {
		t.Fatal("backfilled marks wait for an explicit manual run")
	}

	// 演练：不调模型、不写账本；列出送模型的条目、候选与组（标题与 origin.ref）。
	dry, err := e.svc.PlanManual(bg, ManualFilter{})
	if err != nil || e.head() != h || e.batchCalls() != 0 {
		t.Fatalf("dry run writes nothing and calls no model: %v", err)
	}
	if dry.Matched != 1 || len(dry.Ask) != 1 || dry.Ask[0].Item != mig.ID || dry.Ask[0].Ref != "todo-migrate:abc" ||
		len(dry.Ask[0].Candidates) != 1 || dry.Ask[0].Candidates[0].Item != tgt.ID || dry.Ask[0].Candidates[0].Ref != tgt.Origin.Ref ||
		len(dry.Groups) != 1 || len(dry.Groups[0]) != 2 || dry.Batch == "" || dry.Chars == 0 {
		t.Fatalf("dry plan: %+v", dry)
	}

	// 真跑：同一份规则层结果，一次模型调用，判定重复；去处留下溯源。
	e.stub.When(batchMark, mergeReply(domain.SweepDuplicate))
	run, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || e.batchCalls() != 1 {
		t.Fatalf("apply: %v, calls %d", err, e.batchCalls())
	}
	strip := func(r ManualReport) string {
		r.DryRun, r.Decisions = false, nil
		b, _ := json.Marshal(r)
		return string(b)
	}
	if strip(run) != strip(dry) {
		t.Fatalf("dry run and apply must match:\n%s\n%s", strip(dry), strip(run))
	}
	if len(run.Decisions) != 1 || run.Decisions[0].Action != domain.SweepDuplicate || run.Decisions[0].Target != tgt.ID {
		t.Fatalf("decisions: %+v", run.Decisions)
	}
	got := e.item(mig.ID)
	if got.Phase != domain.PhaseDropped || got.Proposal.Sweep.Backfill == nil || got.Proposal.Sweep.By != "generate" {
		t.Fatalf("folded, keeping the backfill record: %s %+v", got.Phase, got.Proposal.Sweep)
	}
	ab := e.item(tgt.ID).Proposal.Absorbed
	if len(ab) != 1 || ab[0].Item != mig.ID || ab[0].Title != mig.Title || ab[0].Source != "agent:vault-migrate" || ab[0].Ref != "todo-migrate:abc" || ab[0].Action != domain.SweepDuplicate {
		t.Fatalf("duplicate of a migrated proposal keeps its title and origin.ref on the target: %+v", ab)
	}
	es = e.eventsOf(mig.ID)
	if a := es[len(es)-1].Actor; a.Type != domain.ActorHuman || a.Principal != "owner" {
		t.Fatalf("manual merge is recorded as the operator's: %+v", a)
	}

	// 第二遍：没有待归类的，零变化。
	h = e.head()
	if again, err := e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || again.Matched != 0 || len(again.Decisions) != 0 || e.head() != h || e.batchCalls() != 1 {
		t.Fatalf("second run changes nothing: %+v %v", again, err)
	}

	// 撤销：溯源记录一并删掉。
	if err := e.svc.Unmerge(bg, owner, mig.ID); err != nil {
		t.Fatal(err)
	}
	if ab := e.item(tgt.ID).Proposal.Absorbed; len(ab) != 0 || e.item(mig.ID).Phase != domain.PhaseProposed {
		t.Fatalf("undo removes the trail: %+v", ab)
	}
	e.verify()
}

// 筛选：来源（精确、前缀）、项目（含"不属于任何项目"）、时间窗、条数上限。
func TestManualFilter(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	it := domain.Item{Project: "", Origin: &domain.Origin{Source: "agent:vault-migrate"}}
	it.CreatedAt = t0
	for _, c := range []struct {
		f    ManualFilter
		want bool
	}{
		{ManualFilter{}, true},
		{ManualFilter{Source: "agent:vault-migrate"}, true},
		{ManualFilter{Source: "agent:vault-*"}, true},
		{ManualFilter{Source: "agent:vault"}, false},
		{ManualFilter{Source: "src_*"}, false},
		{ManualFilter{Project: "-"}, true},
		{ManualFilter{Project: "p1"}, false},
		{ManualFilter{Since: t0}, true},
		{ManualFilter{Since: t0.Add(time.Second)}, false},
		{ManualFilter{Until: t0}, false},
		{ManualFilter{Until: t0.Add(time.Second)}, true},
	} {
		if got := c.f.match(it); got != c.want {
			t.Errorf("%+v: got %v", c.f, got)
		}
	}
}
