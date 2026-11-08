package chain

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const workHost = "work-host.cli"

type env struct {
	t      *testing.T
	st     *store.Store
	x      *command.Executor
	au     *auth.Auth
	clk    *clock.Clock
	now    time.Time
	secret map[string]string
}

func setup(t *testing.T, max int) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, executors.Module, delegation.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, au: auth.New(nil), now: t0, secret: map[string]string{}}
	e.x = command.New(st, func() time.Time { return e.now })
	e.x.MaxChainDepth = max
	for _, err := range []error{
		projects.Register(e.x), items.Register(e.x, items.NewKinds()), decisions.Register(e.x),
		executors.Register(e.x, executors.Options{Owner: "owner"}),
		delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Errorf("delegation: %v", err) }}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if e.clk, err = clock.New(st, items.HandleTimer, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	e.do(owner, "executor.put", map[string]any{"id": workHost, "kind": "external-cli"}, command.Chain{})
	for name, s := range map[string]auth.Spec{"WRK": {Kind: auth.Worker, Executor: workHost}, "DEL": {Kind: auth.Delegate}} {
		if err := st.Update(bg, func(tx *store.Tx) error {
			_, secret, err := e.au.Issue(tx, s)
			e.secret[name] = secret
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) as(name string) command.Identity {
	return command.Bearer{Auth: e.au, Secret: e.secret[name], Origin: auth.Origin{Harness: "h", Session: "s1"}, OnBehalfOf: "owner"}
}

func (e *env) try(id command.Identity, cmd string, in any, chain command.Chain) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", Chain: chain})
}

func (e *env) do(id command.Identity, cmd string, in any, chain command.Chain) command.Result {
	e.t.Helper()
	r, err := e.try(id, cmd, in, chain)
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func (e *env) newID(r command.Result) string {
	e.t.Helper()
	var out struct{ ID string }
	if err := json.Unmarshal(r.Value, &out); err != nil || out.ID == "" {
		e.t.Fatalf("no id in %s", r.Value)
	}
	return out.ID
}

func inherit(from string) command.Chain { return command.Chain{Mode: command.ChainInherit, From: from} }
func keep(from string) command.Chain    { return command.Chain{Mode: command.ChainKeep, From: from} }

func (e *env) depth(id string) int {
	e.t.Helper()
	var d int
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { d, err = items.ChainDepth(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) item(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) trips() []Trip {
	e.t.Helper()
	var ts []Trip
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ts, err = Trips(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return ts
}

// 一轮自动链路：委派（任务委派继承加一）→ 执行者回执（继承加一）→ 父事项回 open。
func (e *env) round(parent string) (child string, res command.Result) {
	e.t.Helper()
	res = e.do(e.as("DEL"), "delegate.create", map[string]any{"parent": parent, "assignee": workHost}, inherit(parent))
	if res.Suppressed {
		return "", res
	}
	child = e.newID(res)
	e.do(e.as("WRK"), "delegate.report", map[string]any{"id": child, "outcome": "done", "summary": "s",
		"evidence": map[string]any{"pr": "https://x/pr/" + child}}, inherit(child))
	return child, res
}

// ACT-DEP-01：深度沿委派与回执逐轮递增，不被重置。
func TestDepthRisesAlongDelegationAndReceipt(t *testing.T) {
	e := setup(t, 100)
	parent := e.newID(e.do(owner, "item.create", map[string]any{"kind": "task", "title": "p", "phase": "open"}, command.Chain{}))
	if d := e.depth(parent); d != 0 {
		t.Fatalf("owner-created item depth = %d", d)
	}
	last := 0
	for i := 1; i <= 3; i++ {
		child, _ := e.round(parent)
		if got := e.depth(child); got <= last {
			t.Fatalf("round %d: child depth %d did not grow past %d", i, got, last)
		}
		if got := e.depth(parent); got < last {
			t.Fatalf("round %d: parent depth fell from %d to %d", i, last, got)
		}
		last = e.depth(parent)
	}
	if last != 6 { // 每轮委派加一、回执加一
		t.Fatalf("after 3 rounds depth = %d, want 6", last)
	}
	// 每条链路事件的深度单调不减，没有任何一条被清零。
	prev := 0
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, en := range es {
			if en.Event.Type == domain.EvCommandRejected {
				continue
			}
			if en.Event.ChainDepth < prev {
				t.Fatalf("depth fell from %d to %d at seq %d (%s)", prev, en.Event.ChainDepth, en.Event.Seq, en.Event.Type)
			}
			prev = en.Event.ChainDepth
		}
		return nil
	})
}

// ACT-DEP-02：超阈值只记录不派发，健康视图能列出熔断（健康投递见 kernel/health）。
func TestTripRecordsOnly(t *testing.T) {
	e := setup(t, 2)
	parent := e.newID(e.do(owner, "item.create", map[string]any{"kind": "task", "title": "loop", "phase": "open"}, command.Chain{}))
	var tripped command.Result
	for i := 0; i < 5 && !tripped.Suppressed; i++ {
		_, tripped = e.round(parent)
	}
	if !tripped.Suppressed {
		t.Fatal("chain never tripped")
	}
	var children int
	_ = e.st.View(bg, func(tx *store.Tx) error {
		cs, err := items.List(tx, items.Filter{Kind: domain.KindDelegation})
		children = len(cs)
		return err
	})
	before := children

	ts := e.trips()
	if len(ts) != 1 || ts[0].Object != parent || ts[0].Command != "delegate.create" || ts[0].Threshold != 2 || ts[0].Depth <= 2 {
		t.Fatalf("trips = %+v", ts)
	}
	// 再来一次也只记录。
	if _, res := e.round(parent); !res.Suppressed {
		t.Fatal("still-over-threshold chain dispatched")
	}
	ts = e.trips()
	if len(ts) != 2 {
		t.Fatalf("trips = %+v", ts)
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		cs, _ := items.List(tx, items.Filter{Kind: domain.KindDelegation})
		if len(cs) != before {
			t.Fatalf("suppressed command still created children: %d -> %d", before, len(cs))
		}
		return nil
	})
}

// 后台运行拦下时写的事件（带规则 id，无命令名）同样被识别。
func TestBackgroundShapedTripIsRecognised(t *testing.T) {
	e := setup(t, 8)
	it := e.newID(e.do(owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open"}, command.Chain{}))
	payload, _ := json.Marshal(map[string]any{"rule": "pack/event_rules/r", "reason": ExceededReason, "chain_depth": 9, "threshold": 8})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: it, Actor: domain.Actor{Type: domain.ActorSystem, Principal: "rule:r", Via: domain.ViaRule},
			Reason: ExceededReason, Payload: payload, ChainDepth: 9, At: t0,
			Basis: &domain.Basis{PolicyVersion: 1, RuleID: "pack/event_rules/r"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// 同类型但别的原因的健康事件不算熔断。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: it, Actor: owner.Actor, Reason: "source down", Payload: json.RawMessage(`{}`), At: t0})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ts := e.trips()
	if len(ts) != 1 || ts[0].Rule != "pack/event_rules/r" || ts[0].Depth != 9 || ts[0].Threshold != 8 {
		t.Fatalf("trips = %+v", ts)
	}
}

// ACT-DEP-03：本人在 Web 重新委派开始新链路；提问、定时不清零。
func TestOwnerRedelegationStartsNewChainWhileAskAndTimerKeepDepth(t *testing.T) {
	e := setup(t, 2)
	parent := e.newID(e.do(owner, "item.create", map[string]any{"kind": "task", "title": "loop", "phase": "open"}, command.Chain{}))
	var res command.Result
	for i := 0; i < 5 && !res.Suppressed; i++ {
		_, res = e.round(parent)
	}
	if !res.Suppressed || e.depth(parent) <= 2 {
		t.Fatalf("not tripped: %+v depth=%d", res, e.depth(parent))
	}
	// 本人重新委派：新链路从头开始。
	child := e.newID(e.do(owner, "delegate.create", map[string]any{"parent": parent, "assignee": workHost}, command.Chain{}))
	if got := e.item(child).ChainDepth; got != 0 {
		t.Fatalf("redelegated child depth = %d, want 0", got)
	}
	if got := e.depth(child); got != 0 {
		t.Fatalf("redelegated child event depth = %d, want 0", got)
	}

	// 取一个深度大于 0 的事项：提问与回答都不清零。
	deep := e.newID(e.do(owner, "item.create", map[string]any{"kind": "task", "title": "deep", "phase": "open"}, command.Chain{}))
	sub := e.newID(e.do(e.as("DEL"), "delegate.create", map[string]any{"parent": deep, "assignee": workHost}, inherit(deep)))
	if e.depth(sub) == 0 {
		t.Fatal("setup: delegated child has depth 0")
	}
	want := e.depth(sub)
	ask := map[string]any{"question": "ship it?", "options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}
	e.do(e.as("WRK"), "decision.request", map[string]any{"id": sub, "ask": ask}, keep(sub))
	if got := e.depth(sub); got != want {
		t.Fatalf("after ask depth = %d, want %d", got, want)
	}
	e.do(owner, "decision.answer", map[string]any{"id": sub, "choice": "A"}, keep(sub))
	if got := e.depth(sub); got != want {
		t.Fatalf("after answer depth = %d, want %d", got, want)
	}

	// 定时到期：截止到期事件沿用最近深度。
	e.do(owner, "item.update", map[string]any{"id": sub, "deadline": t0.Add(time.Hour)}, keep(sub))
	e.now = t0.Add(2 * time.Hour)
	if n, err := e.clk.Tick(bg); n != 1 || err != nil {
		t.Fatalf("tick = %d %v", n, err)
	}
	if got := e.depth(sub); got != want {
		t.Fatalf("after timer depth = %d, want %d", got, want)
	}
}

// 非目标：调用方不能写深度。
func TestCallerCannotWriteDepth(t *testing.T) {
	e := setup(t, 8)
	if _, err := e.try(owner, "item.create", map[string]any{"kind": "task", "title": "x", "phase": "open", "chain_depth": 7}, command.Chain{}); err == nil {
		t.Fatal("chain_depth in input was accepted")
	}
	id := e.newID(e.do(owner, "item.create", map[string]any{"kind": "task", "title": "x", "phase": "open"}, command.Chain{}))
	if _, err := e.try(owner, "item.update", map[string]any{"id": id, "chain_depth": 7}, command.Chain{}); err == nil {
		t.Fatal("chain_depth in update was accepted")
	}
	if got := e.depth(id); got != 0 {
		t.Fatalf("depth = %d", got)
	}
}

// 对外入口不指明来源（Chain 零值）时由管线推定：委派、回执、提问、Web 回答都沿用当前深度不清零，
// 只有本人在 Web 重新委派开始新链路（ACT-DEP-03）。
func TestDefaultChainKeepsDepthUntilOwnerRedelegates(t *testing.T) {
	e := setup(t, 8)
	root := e.newID(e.do(owner, "item.create", map[string]any{"kind": "task", "title": "r", "phase": "open"}, command.Chain{}))
	sub := e.newID(e.do(e.as("DEL"), "delegate.create", map[string]any{"parent": root, "assignee": workHost}, inherit(root)))
	want := e.depth(sub)
	if want == 0 {
		t.Fatal("setup: child depth 0")
	}
	ask := map[string]any{"question": "ship it?", "options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}
	e.do(e.as("WRK"), "decision.request", map[string]any{"id": sub, "ask": ask}, command.Chain{})
	e.do(owner, "decision.answer", map[string]any{"id": sub, "choice": "A"}, command.Chain{})
	resolved := -1
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, sub)
		for _, en := range es {
			if en.Event.Type == domain.EvDecisionResolved {
				resolved = en.Event.ChainDepth
			}
		}
		return err
	})
	if resolved != want {
		t.Fatalf("decision.resolved depth = %d, want %d", resolved, want)
	}
	e.do(e.as("WRK"), "delegate.report", map[string]any{"id": sub, "outcome": "done", "summary": "s",
		"evidence": map[string]any{"pr": "https://x/pr/" + sub}}, command.Chain{})
	if got := e.depth(sub); got != want {
		t.Fatalf("report depth = %d, want %d", got, want)
	}
	again := e.newID(e.do(e.as("DEL"), "delegate.create", map[string]any{"parent": root, "assignee": workHost}, command.Chain{}))
	if got := e.depth(again); got != e.depth(root) || got == 0 {
		t.Fatalf("main-session delegation depth = %d, parent %d", got, e.depth(root))
	}
	fresh := e.newID(e.do(owner, "delegate.create", map[string]any{"parent": root, "assignee": workHost}, command.Chain{}))
	if got := e.depth(fresh); got != 0 {
		t.Fatalf("owner redelegation depth = %d, want 0", got)
	}
}
