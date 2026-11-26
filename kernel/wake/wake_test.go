package wake

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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}
)

type env struct {
	st  *store.Store
	x   *command.Executor
	svc *Service
	au  *auth.Auth
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, proc.Module, delivery.Module, runidentity.Module, Module} {
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
	clk := func() time.Time { return e.now }
	e.x = command.New(st, clk)
	e.au = auth.New(clk)
	for _, err := range []error{projects.Register(e.x), items.Register(e.x, items.NewKinds()), decisions.Register(e.x)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	e.svc, err = New(Options{Delivery: delivery.New(delivery.Options{Now: clk}), Now: clk})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", WebURL: "http://web"})
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

// token 签发一个 token（agent 非空时归属该 Agent），返回凭据 id 与认证入口（与 HTTP 入口同一层次）。
func (e *env) token(t *testing.T, kind auth.Kind, agent string) (credID string, id command.Identity) {
	t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		sp := auth.Spec{Kind: kind, Label: "main", AgentID: agent}
		if agent != "" {
			sp.Harness = "gw"
		}
		c, s, err := e.au.Issue(tx, sp)
		credID, secret = c.ID, s
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return credID, onBehalf{command.Bearer{Auth: e.au, Secret: secret, Origin: auth.Origin{Harness: "h", Session: "other"}}}
}

// onBehalf 补上传输层为 delegate 填的 on_behalf_of。
type onBehalf struct{ command.Identity }

func (o onBehalf) Authenticate(tx *store.Tx) (command.Caller, error) {
	c, err := o.Identity.Authenticate(tx)
	c.Actor.OnBehalfOf = "owner"
	return c, err
}

// asked 建一个等待决定的事项。
func (e *env) asked(t *testing.T) string {
	t.Helper()
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open", "assignee": "owner"}), &out)
	e.must(t, owner, "decision.request", map[string]any{"id": out.ID, "ask": map[string]any{"question": "ship?",
		"options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no", "next": "dropped"}}}})
	return out.ID
}

// wakeRun 像后台触发方那样写一条运行：launch=wake 是外部运行（写入即 running），headless 是普通排队运行。
func (e *env) wakeRun(t *testing.T, launch string, timeout int) (runID, target string) {
	t.Helper()
	return e.wakeRunFor(t, "", launch, timeout)
}

// wakeRunFor 同 wakeRun；agent 非空时运行归属该 Agent（有快照），只有归属同一 Agent 的钥匙能换取。
func (e *env) wakeRunFor(t *testing.T, agent, launch string, timeout int) (runID, target string) {
	t.Helper()
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "wake target", "phase": "open"}), &out)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		req := proc.Request{Run: domain.Run{Category: domain.RunBackground, Runner: "main_session", Launch: launch,
			Harness: "gw", Target: out.ID, TimeoutSec: timeout}, External: launch == runidentity.LaunchWake}
		if agent != "" {
			req.Run.AgentSnapshot = &domain.AgentSnapshot{AgentID: agent, ConfigRev: 1}
		}
		if !req.External {
			req.Spec = proc.Spec{Command: []string{"true"}}
		}
		r, err := proc.Enqueue(tx, req, e.now)
		runID = r.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return runID, out.ID
}

func (e *env) open(t *testing.T, runID string) (d domain.Delivery, err error) {
	t.Helper()
	if uerr := e.st.Update(bg, func(tx *store.Tx) error { d, err = e.svc.Open(tx, runID); return err }); uerr != nil && err == nil {
		t.Fatal(uerr)
	}
	return d, err
}

// exchange 以 id 认证出的调用者换取凭据（与 POST /runs/{id}/credential 同一路径）。
func (e *env) exchange(t *testing.T, id command.Identity, runID string) (secret string, err error) {
	t.Helper()
	if uerr := e.st.Update(bg, func(tx *store.Tx) error {
		c, aerr := id.Authenticate(tx)
		if aerr != nil {
			t.Fatal(aerr)
		}
		secret, _, err = e.svc.Exchange(tx, c, runID)
		return err
	}); uerr != nil && err == nil {
		t.Fatal(uerr)
	}
	return secret, err
}

func (e *env) fail(t *testing.T, runID string) {
	t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return proc.Settle(tx, runID, domain.RunFailed, nil, "ended", e.now)
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) state(t *testing.T, runID string) domain.RunState {
	t.Helper()
	var rec proc.Record
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { rec, err = proc.Get(tx, runID); return }); err != nil {
		t.Fatal(err)
	}
	return rec.Run.State
}

// Attach 把回执收尾挂在命令提交后：运行者一回执，运行即结束并交回 onEnd。
func TestAttachSettlesOnCommit(t *testing.T) {
	e := setup(t)
	var ended []proc.Result
	e.svc.Attach(e.x, func(r proc.Result) { ended = append(ended, r) }, func(err error) { t.Error(err) })
	runID, target := e.wakeRun(t, "wake", 600)
	e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "unrelated", "phase": "open"})
	if len(ended) != 0 {
		t.Fatal("unrelated commit ended the run")
	}
	e.must(t, owner, "item.transition", map[string]any{"id": target, "to": "waiting", "reason": "delegated",
		"waiting_on": map[string]any{"kind": "external", "ref": "someone"}})
	if len(ended) != 1 || ended[0].Run.ID != runID || e.state(t, runID) != domain.RunSucceeded {
		t.Fatalf("ended: %+v", ended)
	}
}

// lease 是一条租约记录的观测字段。
type lease struct {
	credential string
	expires    int64
	released   int64
}

func (e *env) leases(t *testing.T, runID string) (out []lease) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT credential, expires_at, released_at FROM wake_lease WHERE run_id = ?`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l lease
			if err := rows.Scan(&l.credential, &l.expires, &l.released); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// registerFake 登记一条只做判定的命令，用来验证硬线（3b、系统内部）不受唤醒影响。
func registerFake(t *testing.T, x *command.Executor, name string, group authz.Group) {
	t.Helper()
	if err := x.Register(command.Spec{Name: name, Action: authz.ActionChangeSettings, Group: group, Page: "/settings",
		Object: func(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{ID: name}, nil },
		Apply:  func(*command.Ctx) (any, error) { return nil, nil }}); err != nil {
		t.Fatal(err)
	}
}

// 2026-11-24 本人决定取消租约降级：唤醒运行进行中、租约未释放时，持 delegate 钥匙的调用按钥匙本身的级别与 Agent 档位判定，
// 与没有租约的钥匙结果相同；运行身份只属于携带运行凭据的调用，它仍只在本次目标内。硬线与档位照旧。
func TestDelegateKeepsItsOwnLevelDuringWakeRun(t *testing.T) {
	e := setup(t)
	registerFake(t, e.x, "test.power", authz.GroupOwner)
	registerFake(t, e.x, "test.internal", authz.GroupInternal)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.AgentCards["agent_main"] = policy.DefaultCard().With(policy.GroupDecide)
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cred, dlg := e.token(t, auth.Delegate, "agent_main") // 档位放开二档
	_, plain := e.token(t, auth.Delegate, "")            // 未归属 Agent：只整理
	runID, target := e.wakeRunFor(t, "agent_main", "wake", 600)
	e.open(t, runID)
	secret, err := e.exchange(t, dlg, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.exchange(t, plain, runID); err == nil || exitOf(err) != domain.ExitPermissionDenied {
		// 未归属 Agent 的钥匙换不了（不是这个 Agent 的网关），也就不会有它的租约
		t.Fatalf("unbound exchange: %v", err)
	}
	if ls := e.leases(t, runID); len(ls) != 1 || ls[0].credential != cred || ls[0].released != 0 {
		t.Fatalf("lease must be recorded and still open: %+v", ls)
	}
	if e.state(t, runID) != domain.RunRunning {
		t.Fatal("run must still be running")
	}

	var other struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "unrelated", "phase": "open", "assignee": "owner"}), &other)
	for _, id := range []command.Identity{dlg, plain} {
		if _, err := e.do(id, "item.update", map[string]any{"id": other.ID, "title": "edited during wake"}); err != nil {
			t.Fatalf("delegate must edit any item during a wake run: %v", err)
		}
		if _, err := e.do(id, "item.update", map[string]any{"id": target, "title": "target edited"}); err != nil {
			t.Fatalf("delegate must edit the wake target: %v", err)
		}
	}
	if _, err := e.do(dlg, "decision.answer", map[string]any{"id": e.asked(t), "choice": "A"}); err != nil {
		t.Fatalf("delegate whose Agent may decide must answer during a wake run: %v", err)
	}
	// 档位照旧：未归属 Agent 的钥匙只整理，二档是 23（tier_closed），不是降级时的 4。
	if _, err := e.do(plain, "decision.answer", map[string]any{"id": e.asked(t), "choice": "A"}); exitOf(err) != domain.ExitTierClosed {
		t.Fatalf("unbound delegate deciding: %v", err)
	}
	// 硬线照旧：3b 与系统内部命令对 delegate 一律 20。
	for _, cmd := range []string{"test.power", "test.internal"} {
		var de *domain.Error
		if _, err := e.do(dlg, cmd, map[string]any{}); !errors.As(err, &de) || de.ExitCode() != domain.ExitWebOnly || de.Reason != domain.ReasonHardLine {
			t.Fatalf("%s by delegate: %v", cmd, err)
		}
	}

	// 运行凭据只带身份：写目标外的事项、代答都随所属 Agent，与它的钥匙相同（§14.2）。
	run := runidentity.Identity{Secret: secret, Now: func() time.Time { return e.now }}
	if _, err := e.do(run, "item.update", map[string]any{"id": target, "title": "by run"}); err != nil {
		t.Fatalf("run credential on its target: %v", err)
	}
	if _, err := e.do(run, "item.update", map[string]any{"id": other.ID, "title": "by run"}); err != nil {
		t.Fatalf("run credential outside its target: %v", err)
	}
	if _, err := e.do(run, "decision.answer", map[string]any{"id": e.asked(t), "choice": "A"}); err != nil {
		t.Fatalf("run credential deciding follows its Agent: %v", err)
	}
	if _, err := e.do(run, "test.power", map[string]any{}); exitOf(err) != domain.ExitWebOnly {
		t.Fatalf("run credential 3b: %v", err)
	}
}

// 租约随运行结束标记释放：回执、失败、超时任一终态都释放；截止时刻是运行的截止时刻（开始加运行超时），不是固定值；
// worker 换取不记租约。
func TestLeaseReleasedWhenRunEnds(t *testing.T) {
	e := setup(t)
	var ended []proc.Result
	e.svc.Attach(e.x, func(r proc.Result) { ended = append(ended, r) }, func(err error) { t.Error(err) })
	cred, dlg := e.token(t, auth.Delegate, "")
	_, worker := e.token(t, auth.Worker, "")
	start := func(timeout int) (string, string) {
		runID, target := e.wakeRun(t, "wake", timeout)
		e.open(t, runID)
		e.now = e.now.Add(10 * time.Second)
		if _, err := e.exchange(t, dlg, runID); err != nil {
			t.Fatal(err)
		}
		ls := e.leases(t, runID)
		if len(ls) != 1 || ls[0].credential != cred || ls[0].released != 0 || ls[0].expires != e.now.Add(-10*time.Second).Add(time.Duration(timeout)*time.Second).UnixNano() {
			t.Fatalf("lease at exchange: %+v", ls)
		}
		return runID, target
	}
	released := func(runID string, want domain.RunState) {
		t.Helper()
		if st := e.state(t, runID); st != want {
			t.Fatalf("run %s: %s, want %s", runID, st, want)
		}
		if ls := e.leases(t, runID); len(ls) != 1 || ls[0].released == 0 {
			t.Fatalf("lease of %s run not released: %+v", want, ls)
		}
	}
	settle := func() {
		if err := e.svc.Settle(bg, e.st, func(r proc.Result) { ended = append(ended, r) }); err != nil {
			t.Fatal(err)
		}
	}

	byReceipt, target := start(7200) // 截止时刻跟运行超时走，不是 30 分钟
	e.must(t, owner, "item.transition", map[string]any{"id": target, "to": "waiting", "reason": "delegated",
		"waiting_on": map[string]any{"kind": "external", "ref": "someone"}})
	released(byReceipt, domain.RunSucceeded)

	byFailure, _ := start(600)
	e.fail(t, byFailure)
	settle()
	released(byFailure, domain.RunFailed)

	byTimeout, _ := start(60)
	e.now = e.now.Add(time.Minute)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return proc.Settle(tx, byTimeout, domain.RunTimeout, nil, "timeout after 60s", e.now) // 调度器的超时落账
	}); err != nil {
		t.Fatal(err)
	}
	settle()
	released(byTimeout, domain.RunTimeout)
	if n := countEvents(t, e, EvReleased); n != 3 {
		t.Fatalf("lease_released events: %d", n)
	}

	byWorker, _ := e.wakeRun(t, "wake", 600)
	e.open(t, byWorker)
	if _, err := e.exchange(t, worker, byWorker); err != nil || len(e.leases(t, byWorker)) != 0 {
		t.Fatalf("worker exchange must not record a lease: %v %+v", err, e.leases(t, byWorker))
	}
}

// 唤醒投递：class=wake、路由 wake、带运行 id、不带凭据明文、可被网关领取；重复开启不重复产出；不是唤醒运行不产出。
func TestOpenProducesWakeDeliveryOnly(t *testing.T) {
	e := setup(t)
	runID, target := e.wakeRun(t, "wake", 600)
	d, err := e.open(t, runID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Class != domain.DeliveryWake || d.Route != delivery.RouteWake || d.Run != runID || d.Item != target || d.State != domain.DeliveryPending {
		t.Fatalf("delivery: %+v", d)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), "apa_run_") {
		t.Fatalf("delivery carries a credential: %s", b)
	}
	if again, err := e.open(t, runID); err != nil || again.ID != d.ID {
		t.Fatalf("reopen: %+v %v", again, err)
	}
	var claimed *domain.Delivery
	q := delivery.New(delivery.Options{Now: func() time.Time { return e.now }})
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { claimed, err = q.Claim(tx, delivery.RouteWake); return }); err != nil || claimed == nil || claimed.Run != runID {
		t.Fatalf("gateway claim: %+v %v", claimed, err)
	}

	headless, _ := e.wakeRun(t, "headless", 600)
	if _, err := e.open(t, headless); exitOf(err) != domain.ExitConflict {
		t.Fatalf("headless run: %v", err)
	}
	if _, err := e.open(t, "run_missing"); exitOf(err) != domain.ExitNotFound {
		t.Fatalf("missing run: %v", err)
	}
	e.fail(t, runID)
	if _, err := e.open(t, runID); exitOf(err) != domain.ExitConflict {
		t.Fatalf("finished run: %v", err)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("want error")
	}
}

func countEvents(t *testing.T, e *env, typ string) (n int) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			if x.Event.Type == typ {
				n++
			}
			if strings.Contains(string(x.Event.Payload), "apa_run_") {
				t.Fatalf("secret in event %s: %s", x.Event.Type, x.Event.Payload)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// 失败路径：没有目标事项的运行不产出投递；没有租约的 Release 是空操作。
func TestFailureAndPassthroughPaths(t *testing.T) {
	e := setup(t)
	noTarget := func() (id string) {
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Launch: "wake", TimeoutSec: 60}, External: true}, e.now)
			id = r.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}()
	if _, err := e.open(t, noTarget); exitOf(err) != domain.ExitUsage {
		t.Fatalf("no target: %v", err)
	}

	if err := e.st.Update(bg, func(tx *store.Tx) error { return e.svc.Release(tx, "run_none") }); err != nil {
		t.Fatalf("release without lease must be a no-op: %v", err)
	}
}
