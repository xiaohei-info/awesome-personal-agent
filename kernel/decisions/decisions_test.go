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
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: p, Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: lvl}
}

var (
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
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
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: evidence, WebURL: "http://web/items"})
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

func (e *env) webOnlyRule(t *testing.T, r policy.WebOnlyRule) {
	t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.WebOnly.Rules = append(p.WebOnly.Rules, r)
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
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
	for name, who := range map[string]command.Identity{"observer": observer, "stranger worker": agent("other", authz.LevelWorker), "run": runner} {
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

// §14.5：delegate 代答必须附本人原话；记 resolved_via=chat、on_behalf_of 与证据。
func TestDelegateAnswerNeedsEvidence(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	if _, err := e.do(delegate, "decision.answer", "", map[string]any{"id": id, "choice": "A"}); exit(err) != domain.ExitBasisRequired {
		t.Fatalf("no evidence: %v", err)
	}
	if e.get(t, id).Phase != domain.PhaseNeedsInput {
		t.Fatal("answer without evidence must not take effect")
	}
	e.must(t, delegate, "decision.answer", `owner: "yes, ship it" (session s1)`, map[string]any{"id": id, "choice": "A"})
	it := e.get(t, id)
	if r := it.Ask.Resolution; r == nil || r.ResolvedVia != "chat" || r.Evidence != `owner: "yes, ship it" (session s1)` || r.AnsweredBy != "dlg" {
		t.Fatalf("resolution: %+v", r)
	}
	ev := e.last(t, id, domain.EvDecisionResolved)
	if ev.ResolvedVia != "chat" || ev.Actor.OnBehalfOf != "owner" || ev.Evidence != `owner: "yes, ship it" (session s1)` {
		t.Fatalf("event must carry on_behalf_of and evidence: %+v", ev)
	}
	e.verify(t)
}

func TestOnlyFirstAnswerCounts(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	e.must(t, delegate, "decision.answer", "owner said A", map[string]any{"id": id, "choice": "A"})
	for name, who := range map[string]command.Identity{"web": owner, "delegate": delegate} {
		_, err := e.do(who, "decision.answer", "owner said B", map[string]any{"id": id, "choice": "B"})
		if exit(err) != domain.ExitConflict {
			t.Fatalf("%s late answer: want exit 6, got %v", name, err)
		}
	}
	it := e.get(t, id)
	if it.Ask.Resolution.Choice != "A" || it.Ask.Resolution.ResolvedVia != "chat" || it.Phase != domain.PhaseOpen {
		t.Fatalf("first answer must stand: %+v", it.Ask.Resolution)
	}
	e.verify(t)
}

func TestWorkerObserverAndRunCannotAnswer(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	before := len(e.events(t, id))
	for name, who := range map[string]command.Identity{"worker": worker, "observer": observer, "run": runner} {
		if _, err := e.do(who, "decision.answer", "owner said A", map[string]any{"id": id, "choice": "A"}); exit(err) != domain.ExitPermissionDenied {
			t.Errorf("%s: want exit 4, got %v", name, err)
		}
	}
	if e.get(t, id).Phase != domain.PhaseNeedsInput {
		t.Fatal("item must stay needs_input")
	}
	// 只有运行身份的尝试留痕（§6.7）。
	if got := rejections(e.events(t, id)[before:]); got != 1 {
		t.Fatalf("want exactly the run rejection recorded, got %d", got)
	}
}

// §14.4：web_only 类 CLI 回答返回 20 并留痕，事项停在 needs_input，Web 仍可回答；不允许超时默认值。
func TestWebOnlyRejectsDelegateAndDefault(t *testing.T) {
	e := setup(t)
	e.webOnlyRule(t, policy.WebOnlyRule{Tag: "funds", Category: policy.CategoryFunds})
	id := e.item(t, map[string]any{"assignee": "wrk", "tags": []string{"funds"}})
	deadline := t0.Add(time.Minute)
	if _, err := e.do(worker, "decision.request", "", map[string]any{"id": id, "ask": ask(map[string]any{"default_on_timeout": "A", "deadline": deadline})}); exit(err) != domain.ExitUsage {
		t.Fatalf("web_only with default_on_timeout must be refused: %v", err)
	}
	e.must(t, worker, "decision.request", "", map[string]any{"id": id, "ask": ask(nil)})
	if rc := e.get(t, id).Ask.RiskClass; rc != policy.CategoryFunds {
		t.Fatalf("risk class comes from policy rules: %q", rc)
	}
	_, err := e.do(delegate, "decision.answer", "owner said A", map[string]any{"id": id, "choice": "A"})
	var de *domain.Error
	if exit(err) != domain.ExitWebOnly || !errors.As(err, &de) || de.WebURL == nil || *de.WebURL != "http://web/items" {
		t.Fatalf("want exit 20 with web link, got %v", err)
	}
	if it := e.get(t, id); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		t.Fatalf("item must stay needs_input: %+v", it)
	}
	if rejections(e.events(t, id)) != 1 {
		t.Fatal("exit 20 must be recorded as command.rejected")
	}
	e.must(t, owner, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
	if e.get(t, id).Phase != domain.PhaseOpen {
		t.Fatal("the owner can answer web_only on Web")
	}
}

// §14.4：初筛未返回时代答返回 22 并留痕；本人可在 Web 直接回答。
func TestScreeningPendingRejectsDelegate(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	it := e.get(t, id)
	it.Ask.ScreeningPending = true // 初筛模块的状态；本模块只读
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := items.Revise(tx, id, "test", time.Now(), func(x *domain.Item) error { x.Ask = it.Ask; return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(delegate, "decision.answer", "owner said A", map[string]any{"id": id, "choice": "A"}); exit(err) != domain.ExitScreeningPending {
		t.Fatalf("want exit 22, got %v", err)
	}
	if rejections(e.events(t, id)) != 1 || e.get(t, id).Phase != domain.PhaseNeedsInput {
		t.Fatal("exit 22 must be recorded and leave the item waiting")
	}
	e.must(t, owner, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
}

func TestProposalAcceptAndDismiss(t *testing.T) {
	e := setup(t)
	a := e.item(t, map[string]any{"phase": "proposed"})
	d := e.item(t, map[string]any{"phase": "proposed"})
	if _, err := e.do(delegate, "proposal.accept", "", map[string]any{"id": a}); exit(err) != domain.ExitBasisRequired {
		t.Fatalf("delegate needs the owner's words: %v", err)
	}
	if _, err := e.do(worker, "proposal.accept", "x", map[string]any{"id": a}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker cannot accept: %v", err)
	}
	e.must(t, delegate, "proposal.accept", "owner: take it", map[string]any{"id": a})
	e.must(t, owner, "proposal.dismiss", "", map[string]any{"id": d, "reason": "not now"})
	if e.get(t, a).Phase != domain.PhaseOpen || e.get(t, d).Phase != domain.PhaseDropped {
		t.Fatal("accept opens, dismiss drops")
	}
	ev := e.last(t, a, domain.EvItemPhaseChanged)
	if ev.ResolvedVia != "chat" || ev.Evidence != "owner: take it" || ev.Actor.OnBehalfOf != "owner" {
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
	// 只认第一次：已处理的建议再来是 6；运行身份是 4 并留痕。
	if _, err := e.do(owner, "proposal.dismiss", "", map[string]any{"id": a}); exit(err) != domain.ExitConflict {
		t.Fatalf("second decision on a proposal: %v", err)
	}
	p := e.item(t, map[string]any{"phase": "proposed"})
	if _, err := e.do(runner, "proposal.accept", "x", map[string]any{"id": p}); exit(err) != domain.ExitPermissionDenied || rejections(e.events(t, p)) != 1 {
		t.Fatalf("run identity: want exit 4 recorded, got %v", err)
	}
	if e.get(t, p).Phase != domain.PhaseProposed {
		t.Fatal("rejected accept must not change the item")
	}
	e.verify(t)
}

// ACT-TMO-01：到期按默认值决定，resolved_via=timeout，Event 的 actor 为 system 且 via=clock。
func TestTimeoutAppliesDefault(t *testing.T) {
	e := setup(t)
	deadline := t0.Add(time.Minute)
	id := e.asked(t, map[string]any{"default_on_timeout": "B", "deadline": deadline})

	e.now = t0.Add(30 * time.Second)
	if n, err := e.clk.Tick(bg); n != 0 || err != nil {
		t.Fatalf("before the deadline nothing fires: %d %v", n, err)
	}
	e.now = t0.Add(2 * time.Minute)
	if n, err := e.clk.Tick(bg); n != 1 || err != nil {
		t.Fatalf("tick: %d %v", n, err)
	}
	it := e.get(t, id)
	r := it.Ask.Resolution
	if r == nil || r.Choice != "B" || r.ResolvedVia != "timeout" || it.Phase != domain.PhaseDropped {
		t.Fatalf("default must apply: phase=%s res=%+v", it.Phase, r)
	}
	ev := e.last(t, id, domain.EvDecisionResolved)
	if ev.ResolvedVia != "timeout" || ev.Actor.Type != domain.ActorSystem || ev.Actor.Via != domain.ViaClock {
		t.Fatalf("event actor must be system via clock: %+v", ev)
	}
	// 版本与命令管线一致：超时决定后，乐观版本检查看到新版本。
	var v int64
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { v, err = command.Version(tx, id); return })
	if v != 3 { // create、request、timeout
		t.Fatalf("version %d", v)
	}
	if _, err := e.do(owner, "decision.answer", "", map[string]any{"id": id, "choice": "A"}); exit(err) != domain.ExitConflict {
		t.Fatalf("answer after timeout: %v", err)
	}
	e.verify(t)
}

func TestAnswerBeforeDeadlineCancelsTimeout(t *testing.T) {
	e := setup(t)
	id := e.asked(t, map[string]any{"default_on_timeout": "B", "deadline": t0.Add(time.Minute)})
	e.must(t, owner, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
	e.now = t0.Add(5 * time.Minute)
	if n, err := e.clk.Tick(bg); n != 0 || err != nil {
		t.Fatalf("answered decision leaves no timer: %d %v", n, err)
	}
	if r := e.get(t, id).Ask.Resolution; r.Choice != "A" || r.ResolvedVia != "web" {
		t.Fatalf("the owner's answer stands: %+v", r)
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

func TestTimeoutWaitsForScreeningAndSkipsWebOnly(t *testing.T) {
	e := setup(t)
	id := e.asked(t, map[string]any{"default_on_timeout": "A", "deadline": t0.Add(time.Minute)})
	set := func(f func(a *domain.Ask)) {
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			_, _, _, err := items.Revise(tx, id, "test", time.Now(), func(x *domain.Item) error { a := *x.Ask; f(&a); x.Ask = &a; return nil })
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(func(a *domain.Ask) { a.ScreeningPending = true })
	e.now = t0.Add(2 * time.Minute)
	if _, err := e.clk.Tick(bg); err == nil {
		t.Fatal("screening pending: the timeout must be retried, not dropped")
	}
	if e.get(t, id).Phase != domain.PhaseNeedsInput {
		t.Fatal("must wait for screening")
	}
	set(func(a *domain.Ask) { a.ScreeningPending = false })
	if n, err := e.clk.Tick(bg); n != 1 || err != nil {
		t.Fatalf("after screening: %d %v", n, err)
	}
	if r := e.get(t, id).Ask.Resolution; r == nil || r.Choice != "A" {
		t.Fatalf("default applied after screening: %+v", r)
	}

	// 策略后来把它划为 web_only：到期也不替本人决定。
	id2 := e.asked(t, map[string]any{"default_on_timeout": "A", "deadline": e.now.Add(time.Minute)})
	e.webOnlyRule(t, policy.WebOnlyRule{Kind: "task", Category: policy.CategoryIrreversible})
	e.now = e.now.Add(2 * time.Minute)
	if _, err := e.clk.Tick(bg); err != nil {
		t.Fatal(err)
	}
	if it := e.get(t, id2); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		t.Fatalf("web_only must never be decided by default: %+v", it)
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

// 事项截止与决策超时同为 kind=due，同一事项上并存时各走各的，任一方到期或撤销都不影响另一方。
func TestItemDeadlineAndAskTimeoutCoexist(t *testing.T) {
	timers := func(e *env, id string) (ids []string) {
		t.Helper()
		if err := e.st.View(bg, func(tx *store.Tx) error {
			r, err := tx.Query(`SELECT id FROM clock_timer WHERE target = ? AND kind = ? ORDER BY id`, id, string(domain.TimerDue))
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				var x string
				if err := r.Scan(&x); err != nil {
					return err
				}
				ids = append(ids, x)
			}
			return r.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	early, late := t0.Add(time.Hour), t0.Add(2*time.Hour)
	setupBoth := func(t *testing.T, itemDue, askDue time.Time) (*env, string) {
		e := setup(t)
		id := e.item(t, map[string]any{"deadline": itemDue})
		e.must(t, owner, "decision.request", "", map[string]any{"id": id, "ask": ask(map[string]any{"deadline": askDue, "default_on_timeout": "A"})})
		if got := timers(e, id); len(got) != 2 || got[0] != TimerID(id) {
			t.Fatalf("both timers must exist: %v", got)
		}
		return e, id
	}
	tick := func(t *testing.T, e *env, at time.Time, want int) {
		t.Helper()
		e.now = at
		if n, err := e.clk.Tick(bg); n != want || err != nil {
			t.Fatalf("tick at %v: fired %d, err %v", at, n, err)
		}
	}

	t.Run("item deadline first", func(t *testing.T) {
		e, id := setupBoth(t, early, late)
		tick(t, e, early, 1)
		it := e.get(t, id)
		if it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
			t.Fatalf("the item deadline must not settle the decision: %+v", it)
		}
		if got := timers(e, id); len(got) != 1 || got[0] != TimerID(id) {
			t.Fatalf("the decision timeout must survive: %v", got)
		}
		tick(t, e, late, 1)
		if it := e.get(t, id); it.Phase != domain.PhaseOpen || it.Ask.Resolution == nil || it.Ask.Resolution.ResolvedVia != ViaTimeout {
			t.Fatalf("timeout default: %+v", it)
		}
		e.verify(t)
	})

	t.Run("ask timeout first", func(t *testing.T) {
		e, id := setupBoth(t, late, early)
		tick(t, e, early, 1)
		it := e.get(t, id)
		if it.Phase != domain.PhaseOpen || it.Deadline == nil || !it.Deadline.Equal(late) {
			t.Fatalf("settling must keep the item deadline: %+v", it)
		}
		if got := timers(e, id); len(got) != 1 || got[0] == TimerID(id) {
			t.Fatalf("the item deadline timer must survive: %v", got)
		}
		tick(t, e, late, 1)
		if ev := e.last(t, id, domain.EvItemDeadlinePassed); ev.Actor.Via != domain.ViaClock {
			t.Fatalf("deadline event: %+v", ev)
		}
		e.verify(t)
	})

	t.Run("answer cancels only the timeout", func(t *testing.T) {
		e, id := setupBoth(t, late, early)
		e.must(t, owner, "decision.answer", "", map[string]any{"id": id, "choice": "A"})
		if got := timers(e, id); len(got) != 1 || got[0] == TimerID(id) {
			t.Fatalf("answer must cancel only the timeout: %v", got)
		}
	})
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

// §3.3 第 4 步、§6.4 回退：聊天代答（actor=agent）的 decision.resolved 可在 Web 一键撤销，
// 撤销写一条反向事件：ask 回到未回答、阶段回 needs_input，原事件保留；之后可重新回答。
func TestRevertChatAnswer(t *testing.T) {
	e := setup(t)
	id := e.asked(t, nil)
	e.must(t, delegate, "decision.answer", `owner: "就选 A"`, map[string]any{"id": id, "choice": "A"})
	seq := e.last(t, id, domain.EvDecisionResolved).Seq

	if _, err := e.do(delegate, "item.revert", "", map[string]any{"id": id, "event": seq}); exit(err) != domain.ExitWebOnly {
		t.Fatalf("revert is web-only: %v", err)
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

	e.must(t, delegate, "decision.answer", `owner: "改选 B"`, map[string]any{"id": id, "choice": "B"})
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

// 撤销聊天代答后，带 default_on_timeout 的请求要重新按截止时间自动决定。
func TestRevertChatAnswerRearmsTimeout(t *testing.T) {
	e := setup(t)
	id := e.asked(t, map[string]any{"default_on_timeout": "B", "deadline": t0.Add(time.Minute)})
	e.must(t, delegate, "decision.answer", `owner: "选 A"`, map[string]any{"id": id, "choice": "A"})
	e.must(t, owner, "item.revert", "", map[string]any{"id": id, "event": e.last(t, id, domain.EvDecisionResolved).Seq})

	e.now = t0.Add(2 * time.Minute)
	if n, err := e.clk.Tick(bg); n != 1 || err != nil {
		t.Fatalf("tick: %d %v", n, err)
	}
	if r := e.get(t, id).Ask.Resolution; r == nil || r.Choice != "B" || r.ResolvedVia != "timeout" {
		t.Fatalf("default must apply after revert: %+v", r)
	}
	e.verify(t)
}
