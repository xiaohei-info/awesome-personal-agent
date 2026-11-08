package invariants_test

import (
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func proxyKernel(t *testing.T) (*store.Store, *command.Executor, time.Time) {
	t.Helper()
	st := open(t, eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module,
		executors.Module, delegation.Module, proc.Module, runidentity.Module)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	x := command.New(st, func() time.Time { return now })
	for _, f := range []func(*command.Executor) error{
		projects.Register, decisions.Register,
		func(x *command.Executor) error { return items.Register(x, items.NewKinds()) },
		func(x *command.Executor) error {
			return executors.Register(x, executors.Options{Owner: "owner", Project: projects.Get})
		},
		func(x *command.Executor) error {
			return delegation.Register(x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Error(err) }})
		},
	} {
		if err := f(x); err != nil {
			t.Fatal(err)
		}
	}
	return st, x, now
}

func rejectedCount(t *testing.T, st *store.Store) (n int) {
	t.Helper()
	if err := st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 10000)
		for _, en := range es {
			if en.Event.Type == domain.EvCommandRejected {
				n++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return
}

func getItem(t *testing.T, st *store.Store, id string) (it domain.Item) {
	t.Helper()
	if err := st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return
}

// 不变量 3（后半）：delegate.report 报 done 必带结果证据，任何 kind 都如此，第一档（绑定的事项）也一样；缺失为 2，事项不变。
func TestInv3ReportDoneNeedsResultEvidence(t *testing.T) {
	st, x, _ := proxyKernel(t)
	for _, kind := range []string{"task", "delegation"} {
		id := idOf(mustDo(t, x, owner, "item.create", map[string]any{"title": kind, "kind": kind, "phase": "open", "assignee": "dlg"}))
		report := func(ev map[string]any) error {
			// 对绑定的事项（assignee 即 dlg）回执是第一档：不带本人原话。
			b := map[string]any{"id": id, "outcome": "done", "summary": "s", "evidence": ev}
			_, err := x.Do(bg, command.Request{Identity: delegate, Command: "delegate.report", Input: jsonOf(t, b), Owner: "owner"})
			return err
		}
		for _, ev := range []map[string]any{{}, {"pr": "  "}} {
			if err := report(ev); exit(err) != domain.ExitUsage {
				t.Errorf("%s: done without result evidence %v: want exit 2, got %v", kind, ev, err)
			}
			if got := getItem(t, st, id).Phase; got != domain.PhaseOpen {
				t.Errorf("%s: phase moved to %s", kind, got)
			}
		}
		if err := report(map[string]any{"commit": "abc123"}); err != nil {
			t.Errorf("%s: done with a commit: %v", kind, err)
		}
		if got := getItem(t, st, id).Phase; got != domain.PhaseDone {
			t.Errorf("%s: phase = %s", kind, got)
		}
	}
}

// 不变量 11：替本人做决定的代理写入必有依据——本人原话与会话引用，缺一不可；web_only 不因依据放行；运行凭据没有代答能力。
// 每次依据不足记 command.rejected，事项不变。（授权单放行与撤销由授权单节点验证。）
func TestInv11ProxyDecisionsNeedWordsAndSession(t *testing.T) {
	st, x, now := proxyKernel(t)
	asked := idOf(mustDo(t, x, owner, "item.create", map[string]any{"title": "t", "kind": "task", "phase": "open"}))
	mustDo(t, x, owner, "decision.request", map[string]any{"id": asked, "ask": ask(nil)})
	proposed := idOf(mustDo(t, x, owner, "item.create", map[string]any{"title": "p", "kind": "task", "phase": "proposed"}))
	noSession := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
	call := func(id command.Identity, ev, cmd string, in map[string]any) error {
		_, err := x.Do(bg, command.Request{Identity: id, Command: cmd, Input: jsonOf(t, in), Owner: "owner", Evidence: ev, WebURL: "http://web"})
		return err
	}
	cases := []struct {
		cmd  string
		in   map[string]any
		held func() bool // 事项仍停在原阶段
	}{
		{"decision.answer", map[string]any{"id": asked, "choice": "A"}, func() bool { return getItem(t, st, asked).Phase == domain.PhaseNeedsInput }},
		{"proposal.accept", map[string]any{"id": proposed}, func() bool { return getItem(t, st, proposed).Phase == domain.PhaseProposed }},
	}
	for _, c := range cases {
		for name, a := range map[string]struct {
			id command.Identity
			ev string
		}{"no words": {delegate, ""}, "blank words": {delegate, "  "}, "words without session": {noSession, "owner: yes"}} {
			before := rejectedCount(t, st)
			if err := call(a.id, a.ev, c.cmd, c.in); exit(err) != domain.ExitBasisRequired {
				t.Errorf("%s/%s: want exit 23, got %v", c.cmd, name, err)
			}
			if rejectedCount(t, st) != before+1 || !c.held() {
				t.Errorf("%s/%s: want one command.rejected and an unchanged item", c.cmd, name)
			}
		}
	}
	// 运行凭据：带齐原话也是 4。
	run := runCredential(t, st, now, asked)
	if err := call(run, "owner: yes", "decision.answer", cases[0].in); exit(err) != domain.ExitPermissionDenied {
		t.Errorf("run credential: want exit 4, got %v", err)
	}
	// 原话加会话：放行，Event 带 on_behalf_of、原话与会话。
	if err := call(delegate, "owner: yes", "decision.answer", cases[0].in); err != nil {
		t.Fatalf("words and session: %v", err)
	}
	var ok bool
	_ = st.View(bg, func(tx *store.Tx) error {
		es, _ := eventlog.ReadObject(tx, asked)
		for _, en := range es {
			a := en.Event.Actor
			ok = ok || (en.Event.ResolvedVia == "chat" && a.OnBehalfOf == "owner" && a.Session == "s1" && en.Event.Evidence == "owner: yes")
		}
		return nil
	})
	if !ok {
		t.Error("the answer Event must carry on_behalf_of, the owner's words and the session")
	}
}

func jsonOf(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runCredential 为 target 建一次后台运行并签发它的运行凭据。
func runCredential(t *testing.T, st *store.Store, now time.Time, target string) command.Identity {
	t.Helper()
	var secret string
	if err := st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: target, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, now)
		if err != nil {
			return err
		}
		secret, err = runidentity.Issue(tx, r.ID, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return runidentity.Identity{Secret: secret}
}
