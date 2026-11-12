package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

var owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

const (
	workHost  = "work-host.cli"
	otherHost = "other-host.cli"
)

type env struct {
	st     *store.Store
	x      *command.Executor
	au     *auth.Auth
	errs   []error
	secret map[string]string // 名称 → 凭据明文
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, executors.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, au: auth.New(nil), secret: map[string]string{}}
	e.x = command.New(st, func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := executors.Register(e.x, executors.Options{Owner: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := Register(e.x, Options{Owner: "owner", OnError: func(_ command.Commit, err error) { e.errs = append(e.errs, err) }}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{workHost, otherHost} {
		e.must(t, owner, "executor.put", map[string]any{"id": id, "kind": "external-cli"})
	}
	e.issue(t, "WRK", auth.Spec{Kind: auth.Worker, Executor: workHost})
	e.issue(t, "WRK2", auth.Spec{Kind: auth.Worker, Executor: otherHost})
	e.issue(t, "DEL", auth.Spec{Kind: auth.Delegate})
	return e
}

func (e *env) issue(t *testing.T, name string, s auth.Spec) {
	t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, secret, err := e.au.Issue(tx, s)
		e.secret[name] = secret
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// as 以真实签发的凭据发起请求：操作者与信任级由认证步骤据账本得出。
func (e *env) as(name string) command.Identity {
	return command.Bearer{Auth: e.au, Secret: e.secret[name], Origin: auth.Origin{Harness: "h", Session: "s1", SessionKind: "direct"}, OnBehalfOf: "owner"}
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	req := command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner"}
	req.Evidence = "owner said so"
	return e.x.Do(bg, req)
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) command.Result {
	t.Helper()
	r, err := e.do(id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func exitOf(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func (e *env) wantExit(t *testing.T, want int, id command.Identity, cmd string, in any) *domain.Error {
	t.Helper()
	_, err := e.do(id, cmd, in)
	if exitOf(err) != want {
		t.Fatalf("%s: want exit %d, got %v", cmd, want, err)
	}
	var de *domain.Error
	errors.As(err, &de)
	return de
}

func (e *env) id(t *testing.T, r command.Result) string {
	t.Helper()
	var out struct{ ID string }
	if err := json.Unmarshal(r.Value, &out); err != nil || out.ID == "" {
		t.Fatalf("no id in %s", r.Value)
	}
	return out.ID
}

func (e *env) project(t *testing.T, name string, allowed any) string {
	t.Helper()
	id := e.id(t, e.must(t, owner, "project.create", map[string]any{"name": name}))
	if allowed != nil {
		e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": id, "executors": allowed})
	}
	return id
}

// parent 建一个 open 的父事项。
func (e *env) parent(t *testing.T, project string) string {
	t.Helper()
	return e.id(t, e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "fix bug", "phase": "open", "project": project}))
}

func (e *env) item(t *testing.T, id string) domain.Item {
	t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) events(t *testing.T) []domain.Event {
	t.Helper()
	var out []domain.Event
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			out = append(out, x.Event)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func countType(es []domain.Event, typ string) (n int) {
	for _, x := range es {
		if x.Type == typ {
			n++
		}
	}
	return
}

func (e *env) consistent(t *testing.T) {
	t.Helper()
	if err := e.st.View(bg, items.Verify); err != nil {
		t.Fatalf("projection diverges from events: %v", err)
	}
	if len(e.errs) > 0 {
		t.Fatalf("post-commit errors: %v", e.errs)
	}
}

func (e *env) delegate(t *testing.T, id command.Identity, parent, assignee string) string {
	t.Helper()
	return e.id(t, e.must(t, id, "delegate.create", map[string]any{"parent": parent, "assignee": assignee}))
}

func rep(id, outcome string, ev Evidence) map[string]any {
	return map[string]any{"id": id, "outcome": outcome, "summary": "s", "evidence": ev}
}

// flow-delegate-recover：委派、拿到项目归属、绑定凭据回执、父事项回 open、其他凭据不能回执。
func TestDelegateAndReceiptRecoverParent(t *testing.T) {
	e := setup(t)
	proj := e.project(t, "acc-work", []string{workHost})
	parent := e.parent(t, proj)

	child := e.delegate(t, e.as("DEL"), parent, workHost)
	c := e.item(t, child)
	if c.Assignee != workHost || c.Project != proj || c.Kind != domain.KindDelegation || c.Phase != domain.PhaseOpen {
		t.Fatalf("child = %+v", c)
	}
	p := e.item(t, parent)
	if p.Phase != domain.PhaseWaiting || p.WaitingOn == nil || p.WaitingOn.Kind != domain.WaitingExecutor || p.WaitingOn.Ref != workHost {
		t.Fatalf("parent after delegate = %+v", p)
	}
	if countType(e.events(t), domain.EvDelegationCreated) != 1 {
		t.Fatal("delegation.created not recorded")
	}

	// 其他 worker 凭据：退出码 4，子事项与父事项都不变。
	e.wantExit(t, domain.ExitPermissionDenied, e.as("WRK2"), "delegate.report", rep(child, "done", Evidence{PR: "https://x/pr/9"}))
	if got := e.item(t, child); got.Phase != domain.PhaseOpen {
		t.Fatalf("foreign receipt changed the child: %+v", got)
	}

	// 绑定凭据回执 done 并附 PR：子事项 done、父事项回 open、PR 登记为关联键。
	e.must(t, e.as("WRK"), "delegate.report", rep(child, "done", Evidence{PR: "https://x/pr/88", Commit: "abc"}))
	if got := e.item(t, child); got.Phase != domain.PhaseDone {
		t.Fatalf("child = %+v", got)
	}
	if got := e.item(t, parent); got.Phase != domain.PhaseOpen || got.WaitingOn != nil {
		t.Fatalf("parent after receipt = %+v", got)
	}
	if countType(e.events(t), domain.EvDelegationReported) != 1 {
		t.Fatal("delegation.reported not recorded")
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		if it, ok, err := Lookup(tx, "https://x/pr/88"); err != nil || !ok || it != child {
			t.Fatalf("PR link = %q %v %v", it, ok, err)
		}
		rs, err := Reports(tx, child)
		if err != nil || len(rs) != 1 || rs[0].Outcome != OutcomeDone || rs[0].Evidence.Commit != "abc" || rs[0].Actor.Principal != workHost {
			t.Fatalf("reports = %+v %v", rs, err)
		}
		return nil
	})
	e.consistent(t)
}

// 全部子事项终态后父事项才回 open；子事项 needs_input 时保持 waiting；丢弃也算终态。
func TestParentReopensOnlyWhenAllChildrenTerminal(t *testing.T) {
	e := setup(t)
	parent := e.parent(t, "")
	a := e.delegate(t, e.as("DEL"), parent, workHost)
	b := e.delegate(t, e.as("DEL"), parent, workHost)

	e.must(t, e.as("WRK"), "delegate.report", rep(a, "done", Evidence{Link: "https://x/1"}))
	if got := e.item(t, parent); got.Phase != domain.PhaseWaiting {
		t.Fatalf("parent reopened with a live child: %+v", got)
	}

	// 另一个子事项进 needs_input：父事项仍 waiting。
	e.must(t, e.as("WRK"), "item.update", map[string]any{"id": b, "ask": map[string]any{"question": "which?", "free_text": true}})
	e.must(t, e.as("WRK"), "item.transition", map[string]any{"id": b, "to": "needs_input", "reason": "need owner"})
	if got := e.item(t, parent); got.Phase != domain.PhaseWaiting {
		t.Fatalf("parent reopened while child needs input: %+v", got)
	}

	// 本人把它丢弃：经提交后的订阅补做，父事项回 open。
	e.must(t, owner, "item.transition", map[string]any{"id": b, "to": "dropped", "reason": "not needed"})
	if got := e.item(t, parent); got.Phase != domain.PhaseOpen {
		t.Fatalf("parent not reopened after last child dropped: %+v", got)
	}
	e.consistent(t)
}

func TestBlockedReportKeepsParentWaitingAndIsRecorded(t *testing.T) {
	e := setup(t)
	parent := e.parent(t, "")
	child := e.delegate(t, e.as("DEL"), parent, workHost)
	in := rep(child, "blocked", Evidence{})
	in["summary"] = "no checkout on this host"
	in["cursor"] = "42"
	e.must(t, e.as("WRK"), "delegate.report", in)

	if got := e.item(t, child); got.Phase != domain.PhaseOpen || got.StatusLabel != "blocked: no checkout on this host" {
		t.Fatalf("child = %+v", got)
	}
	if got := e.item(t, parent); got.Phase != domain.PhaseWaiting {
		t.Fatalf("parent = %+v", got)
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		rs, err := Reports(tx, child)
		if err != nil || len(rs) != 1 || rs[0].Outcome != OutcomeBlocked || rs[0].Summary != "no checkout on this host" || rs[0].Cursor != "42" {
			t.Fatalf("reports = %+v %v", rs, err)
		}
		return nil
	})
	e.consistent(t)
}

func TestReportValidation(t *testing.T) {
	e := setup(t)
	parent := e.parent(t, "")
	child := e.delegate(t, e.as("DEL"), parent, workHost)
	other := e.delegate(t, e.as("DEL"), parent, workHost)

	// done 没有证据：拒绝，子事项不变。
	e.wantExit(t, domain.ExitUsage, e.as("WRK"), "delegate.report", rep(child, "done", Evidence{}))
	e.wantExit(t, domain.ExitUsage, e.as("WRK"), "delegate.report", rep(child, "finished", Evidence{Link: "x"}))
	bad := rep(child, "progress", Evidence{})
	bad["summary"] = " "
	e.wantExit(t, domain.ExitUsage, e.as("WRK"), "delegate.report", bad)
	e.wantExit(t, domain.ExitPermissionDenied, e.as("WRK"), "delegate.report", rep(parent, "progress", Evidence{})) // 不是它的事项
	e.wantExit(t, domain.ExitNotFound, e.as("WRK"), "delegate.report", rep("itm_nope", "progress", Evidence{}))
	if got := e.item(t, child); got.Phase != domain.PhaseOpen {
		t.Fatalf("child = %+v", got)
	}

	// 同一个 PR 不能登记到两个事项（挂靠消息会被劫持）。
	e.must(t, e.as("WRK"), "delegate.report", rep(child, "progress", Evidence{PR: "https://x/pr/1"}))
	e.wantExit(t, domain.ExitConflict, e.as("WRK"), "delegate.report", rep(other, "progress", Evidence{PR: "https://x/pr/1"}))

	// 终态的子事项不再接受回执。
	e.must(t, e.as("WRK"), "delegate.report", rep(child, "done", Evidence{PR: "https://x/pr/1"}))
	e.wantExit(t, domain.ExitConflict, e.as("WRK"), "delegate.report", rep(child, "progress", Evidence{}))

	// observer 不能回执。
	e.issue(t, "OBS", auth.Spec{Kind: auth.Observer})
	e.wantExit(t, domain.ExitPermissionDenied, e.as("OBS"), "delegate.report", rep(other, "progress", Evidence{}))
	e.consistent(t)
}

// flow-allowed-executors：名单是内核硬约束。
func TestAllowedExecutorsAreEnforced(t *testing.T) {
	e := setup(t)
	work := e.project(t, "acc-work", []string{workHost})
	open := e.project(t, "acc-open", nil)
	empty := e.project(t, "acc-empty", []string{})

	// 名单外：退出码 21，带允许的 id；没有子事项，父事项不变，留一条拒绝事件。
	parent := e.parent(t, work)
	before := countType(e.events(t), domain.EvItemCreated)
	de := e.wantExit(t, domain.ExitExecutorNotAllowed, e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": otherHost})
	if !slices.Equal(de.Allowed, []string{workHost}) {
		t.Fatalf("allowed = %v", de.Allowed)
	}
	es := e.events(t)
	if countType(es, domain.EvDelegationCreated) != 0 || countType(es, domain.EvItemCreated) != before {
		t.Fatal("a rejected delegation wrote an item")
	}
	if got := e.item(t, parent); got.Phase != domain.PhaseOpen {
		t.Fatalf("parent changed: %+v", got)
	}
	if countType(es, domain.EvCommandRejected) != 1 {
		t.Fatalf("rejections = %d", countType(es, domain.EvCommandRejected))
	}

	// 不在执行者清单里：退出码 5。
	oparent := e.parent(t, open)
	e.wantExit(t, domain.ExitNotFound, e.as("DEL"), "delegate.create", map[string]any{"parent": oparent, "assignee": "ghost.cli"})
	if kids := e.children(t, oparent); len(kids) != 0 {
		t.Fatalf("children = %v", kids)
	}

	// 未设置不限制。
	e.delegate(t, e.as("DEL"), oparent, otherHost)

	// 空名单：只能指派给本人。
	eparent := e.parent(t, empty)
	e.wantExit(t, domain.ExitExecutorNotAllowed, e.as("DEL"), "delegate.create", map[string]any{"parent": eparent, "assignee": workHost})
	mine := e.delegate(t, e.as("DEL"), eparent, "owner")
	if e.item(t, mine).Assignee != "owner" {
		t.Fatal("owner assignment failed")
	}
	e.consistent(t)
}

func (e *env) children(t *testing.T, id string) (out []string) {
	t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		ks, err := items.Children(tx, id)
		for _, k := range ks {
			out = append(out, k.ID)
		}
		return err
	})
	return
}

// 绕过名单的几条路：改派、先建后移入项目、子事项换项目、委派时指定别的项目。
func TestAllowedExecutorsCannotBeBypassed(t *testing.T) {
	e := setup(t)
	work := e.project(t, "acc-work", []string{workHost})
	open := e.project(t, "acc-open", nil)
	parent := e.parent(t, work)
	child := e.delegate(t, e.as("DEL"), parent, workHost)
	// 代本人操作的 delegate（改他人事项须带 on_behalf_of 与原话）。
	proxy := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelDelegate}

	// 改派到名单外。
	e.wantExit(t, domain.ExitExecutorNotAllowed, proxy, "item.update", map[string]any{"id": child, "assignee": otherHost})
	if e.item(t, child).Assignee != workHost {
		t.Fatal("assignee changed")
	}
	// 先建后移入项目。
	stray := e.id(t, e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open", "assignee": otherHost}))
	e.wantExit(t, domain.ExitExecutorNotAllowed, proxy, "item.update", map[string]any{"id": stray, "project": work})
	if e.item(t, stray).Project != "" {
		t.Fatal("stray moved into the project")
	}
	// 委派时指定别的项目：拒绝，不建子事项。
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost, "project": open})
	if kids := e.children(t, parent); len(kids) != 1 {
		t.Fatalf("children = %v", kids)
	}
	// 已建子事项换项目：本人也不行。
	e.wantExit(t, domain.ExitUsage, owner, "item.update", map[string]any{"id": child, "project": open})
	if e.item(t, child).Project != work {
		t.Fatal("child project changed")
	}
	// 写明与父事项相同的 project 是允许的。
	e.must(t, e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost, "project": work})
	e.consistent(t)
}

// 名单变更不撤销在途工作：回执仍有效，再次委派被拒绝。
func TestListChangeKeepsInFlightWork(t *testing.T) {
	e := setup(t)
	work := e.project(t, "acc-work", []string{workHost})
	parent := e.parent(t, work)
	child := e.delegate(t, e.as("DEL"), parent, workHost)

	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": work, "executors": []string{otherHost}})

	e.must(t, e.as("WRK"), "delegate.report", rep(child, "progress", Evidence{}))
	e.wantExit(t, domain.ExitExecutorNotAllowed, e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost})
	e.must(t, e.as("WRK"), "delegate.report", rep(child, "done", Evidence{PR: "https://x/pr/2"}))
	if got := e.item(t, parent); got.Phase != domain.PhaseOpen {
		t.Fatalf("parent = %+v", got)
	}
	e.consistent(t)
}

// 只有自己的事项可以委派：worker 不能从别人的事项建子事项；父事项必须是 open 或 waiting。
func TestCreateAuthorizationAndParentPhase(t *testing.T) {
	e := setup(t)
	parent := e.parent(t, "")
	e.wantExit(t, domain.ExitPermissionDenied, e.as("WRK"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost})
	e.wantExit(t, domain.ExitNotFound, e.as("DEL"), "delegate.create", map[string]any{"parent": "itm_nope", "assignee": workHost})
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.create", map[string]any{"parent": parent})
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost, "bogus": 1})

	e.must(t, owner, "item.transition", map[string]any{"id": parent, "to": "dropped", "reason": "no"})
	e.wantExit(t, domain.ExitConflict, e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost})
	if len(e.children(t, parent)) != 0 {
		t.Fatal("child created under a dropped parent")
	}
}

// 后台运行建立的子事项带 unattended 并沿链路继承；本人显式重新委派不再带。
func TestUnattendedInheritance(t *testing.T) {
	e := setup(t)
	parent := e.parent(t, "")
	// 直接构造运行身份的调用者：运行凭据的范围解析不在本模块，这里只验证标记继承。
	run := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run", Via: domain.ViaCLI, Run: "r1"}, Level: authz.LevelRun}
	e.wantExit(t, domain.ExitPermissionDenied, run, "delegate.create", map[string]any{"parent": parent, "assignee": workHost})

	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, _, snap, err := items.Revise(tx, parent, "test", time.Now(), func(it *domain.Item) error { it.Unattended = true; return nil })
		if err != nil {
			return err
		}
		_, err = command.Bump(tx, parent)
		if err != nil {
			return err
		}
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvItemUpdated, Object: parent, Actor: owner.Actor, Payload: snap, At: time.Now()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !e.item(t, e.delegate(t, e.as("DEL"), parent, workHost)).Unattended {
		t.Fatal("child of an unattended item lost the flag")
	}
	if e.item(t, e.delegate(t, owner, parent, workHost)).Unattended {
		t.Fatal("owner's explicit re-delegation kept the flag")
	}
}

func TestRegisterRequiresErrorSink(t *testing.T) {
	x := command.New(nil, nil)
	if err := Register(x, Options{Owner: "owner"}); err == nil {
		t.Fatal("Register without OnError must fail: a swallowed post-commit error leaves the parent stuck")
	}
}

// delegate.settle 不是后门：非内部身份只有本人（改设置）能调；内部身份无事可做时不写任何东西。
func TestSettleIsNotABackdoor(t *testing.T) {
	e := setup(t)
	parent := e.parent(t, "")
	child := e.delegate(t, e.as("DEL"), parent, workHost)
	e.wantExit(t, domain.ExitPermissionDenied, e.as("WRK"), "delegate.settle", map[string]any{"id": child})
	e.wantExit(t, domain.ExitWebOnly, e.as("DEL"), "delegate.settle", map[string]any{"id": child})
	n := len(e.events(t))
	r := e.must(t, system, "delegate.settle", map[string]any{"id": child})
	if string(r.Value) != `{"reopened":false}` || len(e.events(t)) != n {
		t.Fatalf("settle with a live child wrote: %s", r.Value)
	}
	e.consistent(t)
}

// 订阅者对每个事项命令都会触发 settle：与委派无关的事项不能产生错误，也不写任何东西。
func TestUnrelatedItemCommandsDoNotRaiseSettleErrors(t *testing.T) {
	e := setup(t)
	plain := e.parent(t, "")
	e.must(t, owner, "item.transition", map[string]any{"id": plain, "to": "done", "reason": "finished"})
	e.must(t, owner, "item.transition", map[string]any{"id": plain, "to": "open", "reason": "reopen"})
	if len(e.errs) != 0 {
		t.Fatalf("post-commit errors for a non-delegation item: %v", e.errs)
	}
	e.consistent(t)
}

// flow-brief：任务型运行持运行凭据，对本次运行的目标事项（不是委派子事项）apa report 生效；范围外的事项仍被拒绝。
func TestRunCredentialReportsOnRunTarget(t *testing.T) {
	e := setup(t)
	target := e.parent(t, "")
	other := e.parent(t, "")
	run := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run:r1", Via: domain.ViaCLI, Run: "r1"}, Level: authz.LevelRun,
		RunScope: func(_ *store.Tx, o authz.Object) (bool, bool, error) { return o.ID == target, false, nil }}

	e.must(t, run, "delegate.report", rep(target, "progress", Evidence{}))
	if got := e.item(t, target); got.Phase != domain.PhaseOpen || got.StatusLabel != "s" {
		t.Fatalf("progress not recorded on run target: %+v", got)
	}
	e.must(t, run, "delegate.report", rep(target, "blocked", Evidence{}))
	if got := e.item(t, target); got.StatusLabel != "blocked: s" || got.Phase != domain.PhaseOpen {
		t.Fatalf("blocked not recorded: %+v", got)
	}
	e.must(t, run, "delegate.report", rep(target, "done", Evidence{Link: "run output"}))
	if got := e.item(t, target); got.Phase != domain.PhaseDone {
		t.Fatalf("done must take the target out of open: %+v", got)
	}
	// 运行目标事项的回执不是委派回执：不产生 delegation.reported（否则委派核验规则会把它当子事项处理）。
	if n := countType(e.events(t), domain.EvDelegationReported); n != 0 {
		t.Fatalf("run-target receipts emitted %d delegation.reported", n)
	}
	e.wantExit(t, domain.ExitPermissionDenied, run, "delegate.report", rep(other, "done", Evidence{Link: "x"}))
	if got := e.item(t, other); got.Phase != domain.PhaseOpen {
		t.Fatalf("out-of-scope item changed: %+v", got)
	}
	e.consistent(t)
}

// 委派子事项的回执仍是 delegation.reported。
func TestChildReceiptStillEmitsDelegationReported(t *testing.T) {
	e := setup(t)
	child := e.delegate(t, e.as("DEL"), e.parent(t, ""), workHost)
	e.must(t, e.as("WRK"), "delegate.report", rep(child, "progress", Evidence{}))
	if n := countType(e.events(t), domain.EvDelegationReported); n != 1 {
		t.Fatalf("delegation.reported = %d", n)
	}
}

// doBare 不带本人原话地发请求：第一档的动作本来就不该需要它。
func (e *env) doBare(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner"})
}

func (e *env) eventAt(t *testing.T, seq int64) domain.Event {
	t.Helper()
	for _, ev := range e.events(t) {
		if ev.Seq == seq {
			return ev
		}
	}
	t.Fatalf("no event %d", seq)
	return domain.Event{}
}

// §14.5 第一档：发起委派的 delegate 凭据回执自己派出去的活不要本人原话，Event 不带 on_behalf_of；
// 别的 delegate 凭据（绑定不成立）按第二档，缺依据为 23、记 command.rejected、子事项不变。
func TestDelegateReceiptBindsToInitiatorOnly(t *testing.T) {
	e := setup(t)
	e.issue(t, "DEL2", auth.Spec{Kind: auth.Delegate})
	parent := e.parent(t, "")
	child := e.id(t, e.must(t, e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost}))

	before := countType(e.events(t), domain.EvCommandRejected)
	_, err := e.doBare(e.as("DEL2"), "delegate.report", rep(child, "progress", Evidence{}))
	if exitOf(err) != domain.ExitBasisRequired {
		t.Fatalf("foreign delegate without basis: %v", err)
	}
	if got := countType(e.events(t), domain.EvCommandRejected) - before; got != 1 {
		t.Fatalf("command.rejected recorded %d times", got)
	}
	if got := e.item(t, child); got.StatusLabel != "" {
		t.Fatalf("rejected receipt changed the child: %+v", got)
	}

	r, err := e.doBare(e.as("DEL"), "delegate.report", rep(child, "progress", Evidence{}))
	if err != nil {
		t.Fatalf("initiator receipt without words: %v", err)
	}
	if ev := e.eventAt(t, r.Seqs[0]); ev.Actor.OnBehalfOf != "" || ev.Actor.Principal == "" {
		t.Fatalf("tier one event must not carry on_behalf_of: %+v", ev.Actor)
	}

	// 第二档：别的凭据带原话与会话照常放行，Event 带 on_behalf_of。
	r = e.must(t, e.as("DEL2"), "delegate.report", rep(child, "blocked", Evidence{}))
	if ev := e.eventAt(t, r.Seqs[0]); ev.Actor.OnBehalfOf != "owner" || ev.Evidence == "" {
		t.Fatalf("tier two event: %+v", ev)
	}
}

// §6.14 第 3 条：报 done 对任何 kind 都必须带结果证据，缺失为 2 且事项不变。
func TestReportDoneNeedsResultEvidenceForAnyKind(t *testing.T) {
	e := setup(t)
	task := e.id(t, e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open", "assignee": workHost}))
	for _, id := range []string{task, e.delegate(t, e.as("DEL"), e.parent(t, ""), workHost)} {
		who := e.as("DEL")
		e.wantExit(t, domain.ExitUsage, who, "delegate.report", rep(id, "done", Evidence{}))
		e.wantExit(t, domain.ExitUsage, who, "delegate.report", rep(id, "done", Evidence{PR: "  "}))
		if got := e.item(t, id); got.Phase != domain.PhaseOpen {
			t.Fatalf("done without result moved the item: %+v", got)
		}
	}
	e.must(t, e.as("DEL"), "delegate.report", rep(task, "done", Evidence{Branch: "fix/x"}))
	if got := e.item(t, task); got.Phase != domain.PhaseDone {
		t.Fatalf("task with result evidence: %+v", got)
	}
}

// §14.5 失败可见：绑定不成立的回执既让子事项转 needs_input 又记 health.alert（同一事务）；
// 报 done 缺结果证据（退出码 2）只记 health.alert，事项不变。
func TestRejectedReceiptTurnsChildToNeedsInputAndAlerts(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.issue(t, "DEL2", auth.Spec{Kind: auth.Delegate})
	child := e.id(t, e.must(t, e.as("DEL"), "delegate.create", map[string]any{"parent": e.parent(t, ""), "assignee": workHost}))
	alerts := func() (out []domain.Event) {
		for _, ev := range e.events(t) {
			if ev.Type == domain.EvHealthAlert && ev.Object == child {
				out = append(out, ev)
			}
		}
		return
	}

	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.report", rep(child, "done", Evidence{}))
	if got := e.item(t, child); got.Phase != domain.PhaseOpen || len(alerts()) != 1 {
		t.Fatalf("done without evidence: phase=%s alerts=%d", got.Phase, len(alerts()))
	}

	if _, err := e.doBare(e.as("DEL2"), "delegate.report", rep(child, "progress", Evidence{})); exitOf(err) != domain.ExitBasisRequired {
		t.Fatalf("unbound receipt: %v", err)
	}
	got := e.item(t, child)
	if got.Phase != domain.PhaseNeedsInput || got.Ask == nil || got.Ask.RiskClass != domain.RiskClassWebOnly || got.StatusLabel != "" {
		t.Fatalf("child after rejected receipt: %+v", got)
	}
	if as := alerts(); len(as) != 2 || as[1].Actor.Principal == "" {
		t.Fatalf("alerts = %+v", as)
	}
	e.consistent(t)
}
