package runidentity

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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	now   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const (
	workHost  = "work-host.cli"
	otherHost = "other-host.cli"
)

type env struct {
	st *store.Store
	x  *command.Executor
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, executors.Module, delegation.Module, proc.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, x: command.New(st, func() time.Time { return now })}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(projects.Register(e.x))
	must(items.Register(e.x, items.NewKinds()))
	must(executors.Register(e.x, executors.Options{Owner: "owner"}))
	must(decisions.Register(e.x))
	must(delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Error(err) }}))
	for _, id := range []string{workHost, otherHost} {
		e.must(t, owner, "executor.put", map[string]any{"id": id, "kind": "external-cli"})
	}
	return e
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "x", WebURL: "http://web"})
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) json.RawMessage {
	t.Helper()
	r, err := e.do(id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func exitOf(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func (e *env) want(t *testing.T, code int, id command.Identity, cmd string, in any) {
	t.Helper()
	if _, err := e.do(id, cmd, in); exitOf(err) != code {
		t.Fatalf("%s: want exit %d, got %v", cmd, code, err)
	}
}

func (e *env) newItem(t *testing.T, in map[string]any) string {
	t.Helper()
	in["kind"], in["title"] = "task", "t"
	if in["phase"] == nil {
		in["phase"] = "open"
	}
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "item.create", in), &out)
	return out.ID
}

func (e *env) childOf(t *testing.T, parent string) string {
	t.Helper()
	c := e.newItem(t, map[string]any{})
	e.must(t, owner, "item.relate", map[string]any{"from": c, "to": parent, "type": domain.RelParent})
	return c
}

// run 在账本里入队一次后台运行并签发运行凭据，返回凭据明文与运行 id。
func (e *env) run(t *testing.T, target string) (secret string, runID string) {
	t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: target, TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{"true"}}}, now)
		if err != nil {
			return err
		}
		runID = r.ID
		secret, err = Issue(tx, r.ID, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return secret, runID
}

func (e *env) as(secret string) command.Identity { return Identity{Secret: secret} }

func (e *env) item(t *testing.T, id string) (it domain.Item) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *env) events(t *testing.T, typ string) (n int) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			if x.Event.Type == typ {
				n++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// ACT-BGT-03：运行凭据只限目标事项及其子事项；写无关事项、改无关事项阶段、建项目都是退出码 4。
func TestRunCredentialIsScopedToTarget(t *testing.T) {
	e := setup(t)
	target := e.newItem(t, map[string]any{"assignee": workHost})
	child := e.childOf(t, target)
	grandchild := e.childOf(t, child)
	other := e.newItem(t, map[string]any{})
	secret, _ := e.run(t, target)
	run := e.as(secret)

	e.must(t, run, "item.update", map[string]any{"id": target, "summary": "progress"})
	e.must(t, run, "item.update", map[string]any{"id": grandchild, "summary": "deep"})
	if e.item(t, target).Summary != "progress" || e.item(t, grandchild).Summary != "deep" {
		t.Fatal("in-scope writes did not land")
	}
	e.must(t, run, "delegate.create", map[string]any{"parent": child, "assignee": workHost})

	e.want(t, domain.ExitPermissionDenied, run, "item.update", map[string]any{"id": other, "summary": "x"})
	e.want(t, domain.ExitPermissionDenied, run, "item.transition", map[string]any{"id": other, "to": "dropped", "reason": "x"})
	e.want(t, domain.ExitPermissionDenied, run, "delegate.create", map[string]any{"parent": other, "assignee": workHost})
	e.want(t, domain.ExitPermissionDenied, run, "project.create", map[string]any{"name": "p"})
	if got := e.item(t, other); got.Summary != "" || got.Phase != domain.PhaseOpen {
		t.Fatalf("unrelated item changed: %+v", got)
	}
}

// 运行凭据的身份写进事件：操作者带运行 id，信任级是 run 而不是 delegate。
func TestRunCredentialActorCarriesRunID(t *testing.T) {
	e := setup(t)
	target := e.newItem(t, map[string]any{})
	secret, runID := e.run(t, target)
	e.must(t, e.as(secret), "item.update", map[string]any{"id": target, "summary": "s"})
	var last domain.Event
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		last = es[len(es)-1].Event
		return err
	})
	if last.Actor.Run != runID || last.Actor.Type != domain.ActorAgent || last.Actor.Via != domain.ViaCLI {
		t.Fatalf("actor = %+v", last.Actor)
	}
}

// ACT-BGG-01：运行凭据不能代答、接受或丢弃，全部退出码 4，各记一条拒绝事件，事项不变。
func TestRunCredentialCannotDecide(t *testing.T) {
	e := setup(t)
	target := e.newItem(t, map[string]any{})
	secret, _ := e.run(t, target)
	run := e.as(secret)

	ask := e.newItem(t, map[string]any{"phase": "open"})
	e.must(t, owner, "decision.request", map[string]any{"id": ask, "ask": map[string]any{"question": "q?", "options": []map[string]any{{"id": "A", "label": "a"}, {"id": "B", "label": "b"}}}})
	prop := e.newItem(t, map[string]any{"phase": "proposed"})
	before := []domain.Item{e.item(t, ask), e.item(t, prop)}

	e.want(t, domain.ExitPermissionDenied, run, "decision.answer", map[string]any{"id": ask, "choice": "A"})
	e.want(t, domain.ExitPermissionDenied, run, "proposal.accept", map[string]any{"id": prop})
	e.want(t, domain.ExitPermissionDenied, run, "proposal.dismiss", map[string]any{"id": prop, "reason": "x"})

	if n := e.events(t, domain.EvCommandRejected); n != 3 {
		t.Fatalf("want 3 rejection events, got %d", n)
	}
	if after := []domain.Item{e.item(t, ask), e.item(t, prop)}; after[0].Phase != before[0].Phase || after[1].Phase != before[1].Phase ||
		after[0].Ask.Resolution != nil {
		t.Fatalf("items changed: %+v -> %+v", before, after)
	}
}

// ACT-CMD-03：运行凭据不能提交执行者声明。
func TestRunCredentialCannotDeclare(t *testing.T) {
	e := setup(t)
	secret, _ := e.run(t, e.newItem(t, map[string]any{}))
	e.want(t, domain.ExitPermissionDenied, e.as(secret), "executor.declare", map[string]any{"harness": "acme"})
}

// ACT-BGG-02：运行凭据委派给项目名单外的执行者是退出码 21，没有名单外子事项。
func TestRunCredentialDelegationHonoursAllowedExecutors(t *testing.T) {
	e := setup(t)
	var p struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": "acc-work"}), &p)
	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": p.ID, "executors": []string{workHost}})
	target := e.newItem(t, map[string]any{"project": p.ID})
	secret, _ := e.run(t, target)
	run := e.as(secret)

	e.want(t, domain.ExitExecutorNotAllowed, run, "delegate.create", map[string]any{"parent": target, "assignee": otherHost})
	var kids []domain.Item
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { kids, err = items.Children(tx, target); return })
	if len(kids) != 0 {
		t.Fatalf("out-of-list child written: %+v", kids)
	}
	e.must(t, run, "delegate.create", map[string]any{"parent": target, "assignee": workHost})
	if kids, _ := childrenOf(t, e, target); len(kids) != 1 || !kids[0].Unattended {
		t.Fatalf("in-list child missing or not unattended: %+v", kids)
	}
}

func childrenOf(t *testing.T, e *env, id string) (kids []domain.Item, err error) {
	t.Helper()
	err = e.st.View(bg, func(tx *store.Tx) (err error) { kids, err = items.Children(tx, id); return })
	return
}

// 运行结束（任何终态）、显式吊销、未知明文：凭据都失效，退出码 3。
func TestRunCredentialExpiresWithTheRun(t *testing.T) {
	e := setup(t)
	target := e.newItem(t, map[string]any{})
	update := map[string]any{"id": target, "summary": "s"}

	secret, runID := e.run(t, target)
	e.must(t, e.as(secret), "item.update", update) // queued 与 running 期间有效
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return proc.Settle(tx, runID, domain.RunFailed, nil, "boom", now)
	}); err != nil {
		t.Fatal(err)
	}
	e.want(t, domain.ExitUnauthenticated, e.as(secret), "item.update", update)

	secret2, run2 := e.run(t, target)
	if err := e.st.Update(bg, func(tx *store.Tx) error { return Revoke(tx, run2) }); err != nil {
		t.Fatal(err)
	}
	e.want(t, domain.ExitUnauthenticated, e.as(secret2), "item.update", update)

	e.want(t, domain.ExitUnauthenticated, e.as("apa_run_unknown"), "item.update", update)
	e.want(t, domain.ExitUnauthenticated, e.as(""), "item.update", update)
	// 其他长期凭据明文不会被当作运行凭据。
	e.want(t, domain.ExitUnauthenticated, e.as("apa_wrk_whatever"), "item.update", update)
}

// 重新签发（续接）让旧明文立即失效，新明文有效；账本里只有哈希。
func TestReissueReplacesCredentialAndStoresOnlyHash(t *testing.T) {
	e := setup(t)
	target := e.newItem(t, map[string]any{})
	old, runID := e.run(t, target)
	var fresh string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { fresh, err = Issue(tx, runID, now); return }); err != nil {
		t.Fatal(err)
	}
	u := map[string]any{"id": target, "summary": "s"}
	e.want(t, domain.ExitUnauthenticated, e.as(old), "item.update", u)
	e.must(t, e.as(fresh), "item.update", u)
	_ = e.st.View(bg, func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT hash FROM runidentity_credential")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			_ = rows.Scan(&h)
			if h == fresh || h == old {
				t.Fatal("plaintext stored in the ledger")
			}
		}
		return rows.Err()
	})
}

func TestIssueValidatesRun(t *testing.T) {
	e := setup(t)
	target := e.newItem(t, map[string]any{})
	issue := func(r domain.Run) error {
		return e.st.Update(bg, func(tx *store.Tx) error {
			rec, err := proc.Enqueue(tx, proc.Request{Run: r, Spec: proc.Spec{Command: []string{"true"}}}, now)
			if err != nil {
				return err
			}
			_, err = Issue(tx, rec.ID, now)
			return err
		})
	}
	if exitOf(issue(domain.Run{Category: domain.RunSource, Target: "src_1", TimeoutSec: 5})) != domain.ExitUsage {
		t.Fatal("a source pull is not an agent run and gets no credential")
	}
	if exitOf(issue(domain.Run{Category: domain.RunBackground, Target: "itm_missing", TimeoutSec: 5})) != domain.ExitUsage {
		t.Fatal("a run whose target is not an item gets no credential")
	}
	if err := issue(domain.Run{Category: domain.RunTask, Target: target, TimeoutSec: 5}); err != nil {
		t.Fatal(err)
	}
	err := e.st.Update(bg, func(tx *store.Tx) error { _, err := Issue(tx, "run_nope", now); return err })
	if exitOf(err) != domain.ExitNotFound {
		t.Fatalf("unknown run: %v", err)
	}
	// 已结束的运行不能再签发。
	_, runID := e.run(t, e.newItem(t, map[string]any{}))
	_ = e.st.Update(bg, func(tx *store.Tx) error { return proc.Settle(tx, runID, domain.RunFailed, nil, "x", now) })
	err = e.st.Update(bg, func(tx *store.Tx) error { _, err := Issue(tx, runID, now); return err })
	if exitOf(err) != domain.ExitConflict {
		t.Fatalf("finished run: %v", err)
	}
	if exitOf(e.st.Update(bg, func(tx *store.Tx) error { return Revoke(tx, "run_nope") })) != domain.ExitNotFound {
		t.Fatal("revoking an unknown run must be not_found")
	}
}

// 注入变量：凭据、运行 id、项目；缺项目时不注入 APA_PROJECT。
func TestEnv(t *testing.T) {
	r := domain.Run{ID: "run_1"}
	got := Env("http://apa", "sekret", r, "proj_1")
	want := map[string]string{"APA_URL": "http://apa", "APA_TOKEN": "sekret", "APA_RUN": "run_1", "APA_PROJECT": "proj_1"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q", k, got[k])
		}
	}
	if _, ok := Env("u", "t", r, "")["APA_PROJECT"]; ok {
		t.Fatal("empty project must not be injected")
	}
}

// 唤醒运行的 id 换运行凭据（§13.6）：只认 worker、delegate token，只限未结束的唤醒运行，到期或再次换取后旧凭据失效，不留明文。
func TestExchangeForWakeRun(t *testing.T) {
	e := setup(t)
	target := e.newItem(t, map[string]any{})
	other := e.newItem(t, map[string]any{})
	update := map[string]any{"id": target, "summary": "s"}
	enqueue := func(launch, on string) (id string) {
		t.Helper()
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Launch: launch, Target: on, TimeoutSec: 60},
				Spec: proc.Spec{Command: []string{"true"}}}, now)
			id = r.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	exchange := func(kind auth.Kind, run string) (secret string, exp time.Time, err error) {
		upErr := e.st.Update(bg, func(tx *store.Tx) error {
			secret, exp, err = Exchange(tx, command.Caller{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "gateway", Via: domain.ViaCLI}, Kind: kind}, run, now)
			return nil
		})
		if upErr != nil {
			t.Fatal(upErr)
		}
		return secret, exp, err
	}
	run := enqueue(LaunchWake, target)

	for _, k := range []auth.Kind{auth.Observer, auth.Ext, auth.Webhook, ""} {
		if _, _, err := exchange(k, run); exitOf(err) != domain.ExitPermissionDenied {
			t.Fatalf("kind %q: %v", k, err)
		}
	}
	if _, _, err := exchange(auth.Worker, "run_none"); exitOf(err) != domain.ExitNotFound {
		t.Fatalf("unknown run: %v", err)
	}
	if _, _, err := exchange(auth.Worker, enqueue("headless", other)); exitOf(err) != domain.ExitConflict {
		t.Fatalf("headless run: %v", err)
	}

	first, exp, err := exchange(auth.Delegate, run)
	if err != nil || !IsSecret(first) || !exp.Equal(now.Add(time.Minute)) {
		t.Fatalf("exchange: %q %v %v", first, exp, err)
	}
	at := func(s string, when time.Time) command.Identity {
		return Identity{Secret: s, Now: func() time.Time { return when }}
	}
	// 截止时刻按运行开始时间算，换取晚于开始时不顺延。
	var started string
	third := e.newItem(t, map[string]any{})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{External: true, Run: domain.Run{Category: domain.RunBackground, Launch: LaunchWake, Target: third, TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{"true"}}}, now.Add(-10*time.Second))
		started = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, late, err := exchange(auth.Delegate, started); err != nil || !late.Equal(now.Add(50*time.Second)) {
		t.Fatalf("expires must be start+timeout: %v %v", late, err)
	}
	e.must(t, at(first, now), "item.update", update)
	e.want(t, domain.ExitPermissionDenied, at(first, now), "item.update", map[string]any{"id": other, "summary": "s"})
	e.want(t, domain.ExitUnauthenticated, at(first, exp), "item.update", update)

	second, _, err := exchange(auth.Worker, run)
	if err != nil {
		t.Fatal(err)
	}
	e.want(t, domain.ExitUnauthenticated, at(first, now), "item.update", update)
	e.must(t, at(second, now), "item.update", update)
	if n := e.events(t, EvExchanged); n != 3 {
		t.Fatalf("exchanged events: %d", n)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, en := range es {
			if en.Event.Type == EvExchanged && string(en.Event.Payload) != "" && strings.Contains(string(en.Event.Payload), prefix) {
				t.Fatalf("secret in event: %s", en.Event.Payload)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := e.st.Update(bg, func(tx *store.Tx) error { return proc.Settle(tx, run, domain.RunFailed, nil, "boom", now) }); err != nil {
		t.Fatal(err)
	}
	e.want(t, domain.ExitUnauthenticated, at(second, now), "item.update", update)
	if _, _, err := exchange(auth.Worker, run); exitOf(err) != domain.ExitConflict {
		t.Fatalf("finished run: %v", err)
	}
}
