package decisions

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func agent(p string, lvl authz.Level) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: p, Via: domain.ViaCLI, Session: "s1"}, Level: lvl, CredentialAgentID: "agent_" + p}
}

// setTier 以本人身份把 agent 的档位设成 tier（3b 设置，测试里直接写策略）。
func (e *env) setTier(t *testing.T, agent, tier string) {
	t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.AgentTier.ByAgent[agent] = policy.AgentAllowance{Tier: tier}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

var (
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}
	worker   = agent("wrk", authz.LevelWorker)
	delegate = agent("dlg", authz.LevelDelegate)
	observer = agent("obs", authz.LevelObserver)
	runner   = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run", Via: domain.ViaCLI, Run: "run_1"}, Level: authz.LevelRun}
)

type env struct {
	st  *store.Store
	x   *command.Executor
	clk *clock.Clock
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, now: t0}
	e.x = command.New(st, func() time.Time { return e.now })
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := Register(e.x); err != nil {
		t.Fatal(err)
	}
	handler := func(tx *store.Tx, c clock.Command) error {
		if Owns(c) {
			return HandleTimer(tx, c)
		}
		return items.HandleTimer(tx, c)
	}
	if e.clk, err = clock.New(st, handler, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(id command.Identity, cmd, evidence string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", WebURL: "http://web/items"})
}

func (e *env) must(t *testing.T, id command.Identity, cmd, evidence string, in any) {
	t.Helper()
	if _, err := e.do(id, cmd, evidence, in); err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func (e *env) item(t *testing.T, in map[string]any) string {
	t.Helper()
	in["title"], in["kind"] = "t", "task"
	if in["phase"] == nil {
		in["phase"] = "open"
	}
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: owner, Command: "item.create", Input: b})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ ID string }
	_ = json.Unmarshal(r.Value, &out)
	return out.ID
}

func (e *env) get(t *testing.T, id string) domain.Item {
	t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) events(t *testing.T, id string) []domain.Event {
	t.Helper()
	var out []domain.Event
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, id)
		for _, x := range es {
			out = append(out, x.Event)
		}
		return err
	})
	return out
}

func (e *env) last(t *testing.T, id, typ string) domain.Event {
	t.Helper()
	es := e.events(t, id)
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Type == typ {
			return es[i]
		}
	}
	t.Fatalf("no %s event on %s", typ, id)
	return domain.Event{}
}

func (e *env) verify(t *testing.T) {
	t.Helper()
	if err := e.st.View(bg, items.Verify); err != nil {
		t.Fatalf("projection diverges from events: %v", err)
	}
}

func ask(extra map[string]any) map[string]any {
	a := map[string]any{"question": "ship it?", "options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no", "next": "dropped"}}}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

// asked 建一个 open 事项并提交普通决策请求，返回 id。
func (e *env) asked(t *testing.T, extra map[string]any) string {
	t.Helper()
	id := e.item(t, map[string]any{"assignee": "wrk"})
	e.must(t, worker, "decision.request", "", map[string]any{"id": id, "ask": ask(extra)})
	return id
}

func rejections(es []domain.Event) (n int) {
	for _, e := range es {
		if e.Type == domain.EvCommandRejected {
			n++
		}
	}
	return
}

func TestRequestMovesItemToNeedsInput(t *testing.T) {
	e := setup(t)
	id := e.item(t, map[string]any{"assignee": "wrk", "phase": "waiting", "waiting_on": map[string]any{"kind": "human", "ref": "bob"}})
	e.must(t, worker, "decision.request", "", map[string]any{"id": id, "ask": ask(map[string]any{"recommend": "A"}), "reason": "need a call"})
	it := e.get(t, id)
	if it.Phase != domain.PhaseNeedsInput || it.WaitingOn != nil || it.Ask == nil || it.Ask.Question != "ship it?" || it.Ask.Resolution != nil {
		t.Fatalf("item after request: %+v", it)
	}
	ev := e.last(t, id, domain.EvDecisionRequested)
	if ev.Reason != "need a call" || ev.Actor.Principal != "wrk" {
		t.Fatalf("event: %+v", ev)
	}
	e.verify(t)

	// 一次一个问题：未决时不能再问。
	if _, err := e.do(worker, "decision.request", "", map[string]any{"id": id, "ask": ask(nil)}); exit(err) != domain.ExitConflict {
		t.Fatalf("second request must conflict: %v", err)
	}
}

func TestRequestValidationAndAuthorization(t *testing.T) {
	e := setup(t)
	id := e.item(t, map[string]any{"assignee": "wrk"})
	soon := t0.Add(time.Minute)
	bad := map[string]map[string]any{
		"no question":           {"question": " ", "free_text": true},
		"nothing to pick":       {"question": "q"},
		"dup option":            {"question": "q", "options": []map[string]any{{"id": "A", "label": "a"}, {"id": "A", "label": "b"}}},
		"bad next":              {"question": "q", "options": []map[string]any{{"id": "A", "label": "a", "next": "waiting"}}},
		"unknown recommend":     {"question": "q", "options": []map[string]any{{"id": "A", "label": "a"}}, "recommend": "Z"},
		"default not an option": ask(map[string]any{"default_on_timeout": "Z", "deadline": soon}),
		"default without limit": ask(map[string]any{"default_on_timeout": "A"}),
		"kernel field in ask":   ask(map[string]any{"risk_class": "none"}),
		"screening flag in ask": ask(map[string]any{"screening_pending": true}),
	}
	for name, a := range bad {
		if _, err := e.do(worker, "decision.request", "", map[string]any{"id": id, "ask": a}); exit(err) != domain.ExitUsage {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
	}
	if e.get(t, id).Phase != domain.PhaseOpen {
		t.Fatal("refused requests must not move the item")
	}
	for name, who := range map[string]command.Identity{"observer": observer, "stranger worker": agent("other", authz.LevelWorker)} {
		if _, err := e.do(who, "decision.request", "", map[string]any{"id": id, "ask": ask(nil)}); exit(err) != domain.ExitPermissionDenied {
			t.Errorf("%s: want exit 4, got %v", name, err)
		}
	}
	if _, err := e.do(worker, "decision.request", "", map[string]any{"id": "itm_missing", "ask": ask(nil)}); exit(err) != domain.ExitNotFound {
		t.Fatalf("missing item: %v", err)
	}
	prop := e.item(t, map[string]any{"phase": "proposed", "assignee": "wrk"})
	if _, err := e.do(worker, "decision.request", "", map[string]any{"id": prop, "ask": ask(nil)}); exit(err) != domain.ExitConflict {
		t.Fatalf("proposed item cannot ask: %v", err)
	}
}

func TestAnswerByOwnerOnWeb(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	e.must(t, owner, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
	it := e.get(t, id)
	r := it.Ask.Resolution
	if it.Phase != domain.PhaseOpen || r == nil || r.Choice != "A" || r.ResolvedVia != "web" || r.AnsweredBy != "owner" {
		t.Fatalf("after answer: phase=%s res=%+v", it.Phase, r)
	}
	ev := e.last(t, id, domain.EvDecisionResolved)
	if ev.ResolvedVia != "web" || ev.Actor.Type != domain.ActorHuman {
		t.Fatalf("event: %+v", ev)
	}
	e.verify(t)
}

func TestOptionNextPhaseAndFreeText(t *testing.T) {
	e := setup(t)
	id := e.asked(t, map[string]any{"free_text": true})
	e.must(t, owner, "decision.answer", "", map[string]any{"id": id, "choice": "B"})
	if e.get(t, id).Phase != domain.PhaseDropped {
		t.Fatal("option next=dropped must drop the item")
	}
	free := e.asked(t, map[string]any{"free_text": true})
	e.must(t, owner, "decision.answer", "", map[string]any{"id": free, "text": "something else"})
	if r := e.get(t, free).Ask.Resolution; r == nil || r.Text != "something else" || r.Choice != "" {
		t.Fatalf("free text resolution: %+v", r)
	}
	strict := e.asked(t, nil)
	for name, in := range map[string]map[string]any{
		"free text not allowed": {"id": strict, "text": "x"},
		"both":                  {"id": strict, "choice": "A", "text": "x"},
		"neither":               {"id": strict},
		"unknown choice":        {"id": strict, "choice": "Z"},
	} {
		if _, err := e.do(owner, "decision.answer", "", in); exit(err) != domain.ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
	}
	if e.get(t, strict).Phase != domain.PhaseNeedsInput {
		t.Fatal("refused answers must leave the item waiting")
	}
	e.verify(t)
}

// 二档：档位只整理的 delegate 回答问题得 23 并留痕、不生效；本人把它调到「也替我拍板」后可以答，
// 记 resolved_via=agent、answered_by 为 Agent，事件依据是档位放开，不写 on_behalf_of。
func TestDelegateAnswerFollowsTier(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	if _, err := e.do(delegate, "decision.answer", "", map[string]any{"id": id, "choice": "A"}); exit(err) != domain.ExitTierClosed {
		t.Fatalf("organize-only delegate: %v", err)
	}
	if e.get(t, id).Phase != domain.PhaseNeedsInput || rejections(e.events(t, id)) != 1 {
		t.Fatal("a refused answer must not take effect and must be recorded")
	}
	e.setTier(t, "agent_dlg", policy.AllowDecide)
	e.must(t, delegate, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
	it := e.get(t, id)
	if r := it.Ask.Resolution; r == nil || r.ResolvedVia != ViaAgent || r.AnsweredBy != "dlg" || r.Evidence != "" {
		t.Fatalf("resolution: %+v", r)
	}
	ev := e.last(t, id, domain.EvDecisionResolved)
	if ev.ResolvedVia != ViaAgent || ev.Actor.OnBehalfOf != "" || ev.Basis == nil || ev.Basis.Via != domain.BasisAllowance {
		t.Fatalf("event must record the allowance, not a proxy: %+v basis=%+v", ev, ev.Basis)
	}
	e.verify(t)
}

func TestOnlyFirstAnswerCounts(t *testing.T) {
	e := setup(t)
	e.setTier(t, "agent_dlg", policy.AllowDecide)
	id := e.asked(t, nil)
	e.must(t, delegate, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
	for name, who := range map[string]command.Identity{"web": owner, "delegate": delegate} {
		_, err := e.do(who, "decision.answer", "", map[string]any{"id": id, "choice": "B"})
		if exit(err) != domain.ExitConflict {
			t.Fatalf("%s late answer: want exit 6, got %v", name, err)
		}
	}
	it := e.get(t, id)
	if it.Ask.Resolution.Choice != "A" || it.Ask.Resolution.ResolvedVia != ViaAgent || it.Phase != domain.PhaseOpen {
		t.Fatalf("first answer must stand: %+v", it.Ask.Resolution)
	}
	e.verify(t)
}

// worker 在自己的事项上回答是二档，档位未放开得 23；运行凭据随所属 Agent，同样 23；observer 在钥匙范围外（4）。三种拒绝都留痕。
func TestWorkerObserverAndRunCannotAnswer(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	before := len(e.events(t, id))
	for name, c := range map[string]struct {
		who  command.Identity
		want int
	}{"worker": {worker, domain.ExitTierClosed}, "observer": {observer, domain.ExitPermissionDenied}, "run": {runner, domain.ExitTierClosed}} {
		if _, err := e.do(c.who, "decision.answer", "", map[string]any{"id": id, "choice": "A"}); exit(err) != c.want {
			t.Errorf("%s: want exit %d, got %v", name, c.want, err)
		}
	}
	if e.get(t, id).Phase != domain.PhaseNeedsInput {
		t.Fatal("item must stay needs_input")
	}
	if got := rejections(e.events(t, id)[before:]); got != 3 {
		t.Fatalf("every refusal must be recorded, got %d", got)
	}
}

func TestProposalAcceptAndDismiss(t *testing.T) {
	e := setup(t)
	a := e.item(t, map[string]any{"phase": "proposed"})
	d := e.item(t, map[string]any{"phase": "proposed"})
	if _, err := e.do(delegate, "proposal.accept", "", map[string]any{"id": a}); exit(err) != domain.ExitTierClosed {
		t.Fatalf("organize-only delegate cannot make a commitment: %v", err)
	}
	if _, err := e.do(worker, "proposal.accept", "", map[string]any{"id": a}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker: not its item: %v", err)
	}
	e.setTier(t, "agent_dlg", policy.AllowDecide)
	e.must(t, delegate, "proposal.accept", "", map[string]any{"id": a})
	e.must(t, owner, "proposal.dismiss", "", map[string]any{"id": d, "reason": "not now"})
	if e.get(t, a).Phase != domain.PhaseOpen || e.get(t, d).Phase != domain.PhaseDropped {
		t.Fatal("accept opens, dismiss drops")
	}
	ev := e.last(t, a, domain.EvItemPhaseChanged)
	if ev.ResolvedVia != ViaAgent || ev.Evidence != "" || ev.Actor.OnBehalfOf != "" || ev.Basis == nil || ev.Basis.Via != domain.BasisAllowance {
		t.Fatalf("accept event: %+v", ev)
	}
	if ev := e.last(t, d, domain.EvItemPhaseChanged); ev.ResolvedVia != "web" || ev.Reason != "not now" {
		t.Fatalf("dismiss event: %+v", ev)
	}
	// 理由选填：不带理由也能忽略，事件理由取缺省。
	n := e.item(t, map[string]any{"phase": "proposed"})
	e.must(t, owner, "proposal.dismiss", "", map[string]any{"id": n})
	if ev := e.last(t, n, domain.EvItemPhaseChanged); e.get(t, n).Phase != domain.PhaseDropped || ev.Reason != "proposal dismissed" {
		t.Fatalf("dismiss without reason: %+v", ev)
	}
	// 只认第一次：已处理的建议再来是 6；运行身份随所属 Agent 是 23 并留痕。
	if _, err := e.do(owner, "proposal.dismiss", "", map[string]any{"id": a}); exit(err) != domain.ExitConflict {
		t.Fatalf("second decision on a proposal: %v", err)
	}
	p := e.item(t, map[string]any{"phase": "proposed"})
	if _, err := e.do(runner, "proposal.accept", "", map[string]any{"id": p}); exit(err) != domain.ExitTierClosed || rejections(e.events(t, p)) != 1 {
		t.Fatalf("run identity: want exit 23 recorded, got %v", err)
	}
	if e.get(t, p).Phase != domain.PhaseProposed {
		t.Fatal("rejected accept must not change the item")
	}
	e.verify(t)
}

// 超时默认已取消（§14.3）：带 default_on_timeout 的请求整条被拒（2），事项不变；问题只等回答。
func TestDefaultOnTimeoutIsRejected(t *testing.T) {
	e := setup(t)
	id := e.item(t, map[string]any{"assignee": "wrk"})
	if _, err := e.do(worker, "decision.request", "", map[string]any{"id": id, "ask": ask(map[string]any{"default_on_timeout": "B", "deadline": t0.Add(time.Minute)})}); exit(err) != domain.ExitUsage {
		t.Fatalf("default_on_timeout: %v", err)
	}
	if it := e.get(t, id); it.Phase != domain.PhaseOpen || it.Ask != nil {
		t.Fatalf("rejected request changed the item: %+v", it)
	}
}

// ACT-TMO-02 的内核部分：没有默认值就不自动决定。
func TestNoDefaultKeepsWaiting(t *testing.T) {
	e := setup(t)
	id := e.asked(t, map[string]any{"deadline": t0.Add(time.Minute)})
	e.now = t0.Add(2 * time.Minute)
	if _, err := e.clk.Tick(bg); err != nil {
		t.Fatal(err)
	}
	if it := e.get(t, id); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		t.Fatalf("no default: must keep waiting: %+v", it)
	}
}

func TestHandleTimerRejectsForeignTimers(t *testing.T) {
	e := setup(t)
	err := e.st.Update(bg, func(tx *store.Tx) error {
		return HandleTimer(tx, clock.Command{TimerID: "wait_x", Target: "x", Kind: domain.TimerWaitUntil})
	})
	if exit(err) != domain.ExitUsage {
		t.Fatalf("foreign timer: %v", err)
	}
	// 事项已不存在：Timer 过期，不报错。
	err = e.st.Update(bg, func(tx *store.Tx) error {
		return HandleTimer(tx, clock.Command{TimerID: TimerID("gone"), Target: "gone", Kind: domain.TimerDue})
	})
	if err != nil {
		t.Fatalf("missing target: %v", err)
	}
}

// 升级前留下的决策超时 Timer 到期时什么也不决定：问题继续等，Timer 被清掉；事项自己的截止照常。
func TestLegacyAskTimerDecidesNothing(t *testing.T) {
	e := setup(t)
	id := e.asked(t, map[string]any{"deadline": t0.Add(time.Minute)})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := clock.Add(tx, domain.Timer{ID: TimerID(id), Kind: domain.TimerDue, Target: id, At: t0.Add(time.Minute)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.now = t0.Add(2 * time.Minute)
	if n, err := e.clk.Tick(bg); n != 1 || err != nil {
		t.Fatalf("legacy timer must fire once: %d %v", n, err)
	}
	if it := e.get(t, id); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		t.Fatalf("legacy timer decided: %+v", it)
	}
	if n, err := e.clk.Tick(bg); n != 0 || err != nil {
		t.Fatalf("legacy timer must be gone: %d %v", n, err)
	}
}

func TestIdempotentAnswerReplaysFirstResult(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	b, _ := json.Marshal(map[string]any{"id": id, "choice": "A"})
	req := command.Request{Identity: owner, Command: "decision.answer", Input: b, IdemKey: "k1"}
	if _, err := e.x.Do(bg, req); err != nil {
		t.Fatal(err)
	}
	r, err := e.x.Do(bg, req)
	if err != nil || !r.Replayed {
		t.Fatalf("same key must replay the first result: %v %+v", err, r)
	}
}

// §3.3 第 4 步、§6.4 回退：Agent 自答（二档放开）的 decision.resolved 在「Agent 改动」里可按次撤回，
// 撤销写一条反向事件：ask 回到未回答、阶段回 needs_input，原事件保留；之后可重新回答。撤回是 3a，档位只整理的 Agent 不能撤。
func TestRevertChatAnswer(t *testing.T) {
	e := setup(t)
	e.setTier(t, "agent_dlg", policy.AllowDecide)
	id := e.asked(t, nil)
	e.must(t, delegate, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
	seq := e.last(t, id, domain.EvDecisionResolved).Seq

	if _, err := e.do(delegate, "item.revert", "", map[string]any{"id": id, "event": seq}); exit(err) != domain.ExitTierClosed {
		t.Fatalf("revert is 3a: %v", err)
	}
	e.must(t, owner, "item.revert", "", map[string]any{"id": id, "event": seq})
	it := e.get(t, id)
	if it.Phase != domain.PhaseNeedsInput || it.Ask == nil || it.Ask.Resolution != nil {
		t.Fatalf("revert must restore the unanswered ask: %+v", it)
	}
	if got := e.last(t, id, domain.EvItemPhaseChanged); got.Actor.Principal != "owner" {
		t.Fatalf("revert is a forward event by the owner: %+v", got)
	}
	e.verify(t)

	e.must(t, delegate, "decision.answer", "", map[string]any{"id": id, "choice": "B"})
	if it := e.get(t, id); it.Ask.Resolution == nil || it.Ask.Resolution.Choice != "B" {
		t.Fatalf("re-answer after revert: %+v", it.Ask)
	}
	// 人在 Web 上的回答不是 agent 变更，不能撤销。
	h := e.asked(t, nil)
	e.must(t, owner, "decision.answer", "", map[string]any{"id": h, "choice": "A"})
	if _, err := e.do(owner, "item.revert", "", map[string]any{"id": h, "event": e.last(t, h, domain.EvDecisionResolved).Seq}); exit(err) != domain.ExitUsage {
		t.Fatalf("human answer: %v", err)
	}
	e.verify(t)
}
