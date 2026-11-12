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
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
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
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", WebURL: "http://web"})
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

// delegate 签发一个 delegate token，返回凭据 id 与走租约包装的认证入口。
func (e *env) delegate(t *testing.T) (credID string, id command.Identity) {
	credID, _, id = e.token(t, auth.Delegate)
	return
}

// token 签发一个 token，返回凭据 id、明文与走租约包装的认证入口（与 HTTP 入口同一层次）。
func (e *env) token(t *testing.T, kind auth.Kind) (credID, secret string, id command.Identity) {
	t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		c, s, err := e.au.Issue(tx, auth.Spec{Kind: kind, Label: "main"})
		credID, secret = c.ID, s
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return credID, secret, e.svc.Identity(onBehalf{command.Bearer{Auth: e.au, Secret: secret, Origin: auth.Origin{Harness: "h", Session: "other", SessionKind: "direct"}}})
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
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "wake target", "phase": "open"}), &out)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		req := proc.Request{Run: domain.Run{Category: domain.RunBackground, Runner: "main_session", Launch: launch,
			Harness: "gw", Target: out.ID, TimeoutSec: timeout}, External: launch == runidentity.LaunchWake}
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

// exchange 以 id 认证出的调用者换取凭据（与 POST /runs/{id}/credential 同一路径：先过租约包装，再交 Exchange）。
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

// ACT-WAK-04：网关插件用 delegate 换取凭据即为该 token 开租约；租约期间同一 DEL 的另一会话代答是退出码 4，
// 本人在 Web 仍可决定；回执（目标事项离开 open）后运行按成功结束、租约释放，DEL 恢复代答。
func TestLeaseBlocksDelegateAnswerUntilReceipt(t *testing.T) {
	e := setup(t)
	credID, dlg := e.delegate(t)
	answer := func(id command.Identity, item string) error {
		_, err := e.do(id, "decision.answer", map[string]any{"id": item, "choice": "A"})
		return err
	}

	before := e.asked(t)
	if err := answer(dlg, before); err != nil { // 无租约：delegate 带本人原话可以代答
		t.Fatalf("answer before lease: %v", err)
	}

	runID, target := e.wakeRun(t, "wake", 600)
	if _, err := e.open(t, runID); err != nil {
		t.Fatal(err)
	}
	if err := answer(dlg, e.asked(t)); err != nil { // 投递产出但还没人换取：没有租约
		t.Fatalf("answer before exchange: %v", err)
	}
	secret, err := e.exchange(t, dlg, runID)
	if err != nil || !runidentity.IsSecret(secret) {
		t.Fatalf("exchange: %q %v", secret, err)
	}
	other := e.asked(t)
	if err := answer(dlg, other); exitOf(err) != domain.ExitPermissionDenied {
		t.Fatalf("answer during lease: want exit 4, got %v", err)
	}
	if _, err := e.do(dlg, "proposal.accept", map[string]any{"id": other}); exitOf(err) != domain.ExitPermissionDenied {
		t.Fatalf("accept during lease: %v", err)
	}
	// 授权单对租约期间的 token 同样无效：仍是退出码 4，不是 23。
	var g policy.Grant
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		g, err = policy.IssueGrant(tx, e.au, owner.Actor, e.now, policy.GrantInput{Grantee: policy.Grantee{Credential: credID},
			Scope: policy.Scope{Kinds: []string{"task"}}, Actions: []string{"decision.answer"}, ExpiresAt: e.now.Add(time.Hour)})
		return
	}); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"id": other, "choice": "A"})
	if _, err := e.x.Do(bg, command.Request{Identity: dlg, Command: "decision.answer", Input: in, Grant: g.ID}); exitOf(err) != domain.ExitPermissionDenied {
		t.Fatalf("grant during lease: want exit 4, got %v", err)
	}
	renewed, err := e.exchange(t, dlg, runID) // 租约内插件仍可续换，旧凭据随即作废
	if err != nil {
		t.Fatalf("re-exchange during lease: %v", err)
	}
	if _, err := e.do(runidentity.Identity{Secret: secret, Now: func() time.Time { return e.now }}, "item.update",
		map[string]any{"id": target, "summary": "s"}); exitOf(err) != domain.ExitUnauthenticated {
		t.Fatalf("old credential after re-exchange: %v", err)
	}
	e.must(t, owner, "decision.answer", map[string]any{"id": other, "choice": "A"}) // 本人仍可在 Web 决定

	var ended []proc.Result
	settle := func() {
		t.Helper()
		if err := e.svc.Settle(bg, e.st, func(r proc.Result) { ended = append(ended, r) }); err != nil {
			t.Fatal(err)
		}
	}
	settle()
	if len(ended) != 0 || e.state(t, runID) != domain.RunRunning {
		t.Fatalf("target still open must not end the run: %+v", ended)
	}
	// 运行者以运行凭据回执：目标事项转 done
	e.must(t, runidentity.Identity{Secret: renewed, Now: func() time.Time { return e.now }}, "item.transition",
		map[string]any{"id": target, "to": "done", "reason": "handled"})
	settle()
	if len(ended) != 1 || ended[0].Run.ID != runID || ended[0].Run.State != domain.RunSucceeded {
		t.Fatalf("receipt must end the run once: %+v", ended)
	}
	settle()
	if len(ended) != 1 {
		t.Fatal("settle must not end a run twice")
	}
	if err := answer(dlg, e.asked(t)); err != nil {
		t.Fatalf("answer after receipt: %v", err)
	}
	if n := countEvents(t, e, EvOpened); n != 1 {
		t.Fatalf("opened events: %d", n)
	}
	if n := countEvents(t, e, EvReleased); n != 1 {
		t.Fatalf("released events: %d", n)
	}
	if n := countEvents(t, e, domain.EvCommandRejected); n < 1 {
		t.Fatal("rejected delegate answer left no audit event")
	}
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

// 租约在运行结束或运行超时后自动恢复，不依赖调用方收尾；只约束换取凭据的那个 token；worker 换取不开租约。
func TestLeaseEndsWithRunOrTimeoutAndIsPerToken(t *testing.T) {
	e := setup(t)
	cred, dlg := e.delegate(t)
	_, bystander := e.delegate(t)
	workerID, _, worker := e.token(t, auth.Worker)
	leased := func(c string) bool {
		var l bool
		if err := e.st.View(bg, func(tx *store.Tx) (err error) { l, err = e.svc.Leased(tx, c); return }); err != nil {
			t.Fatal(err)
		}
		return l
	}
	denied := func(id command.Identity) bool {
		_, err := e.do(id, "decision.answer", map[string]any{"id": e.asked(t), "choice": "A"})
		return exitOf(err) == domain.ExitPermissionDenied
	}

	byRun, _ := e.wakeRun(t, "wake", 600)
	e.open(t, byRun)
	if _, err := e.exchange(t, worker, byRun); err != nil || leased(workerID) {
		t.Fatalf("worker exchange: %v leased=%v", err, leased(workerID))
	}
	if _, err := e.exchange(t, dlg, byRun); err != nil {
		t.Fatal(err)
	}
	if !leased(cred) || !denied(dlg) || denied(bystander) {
		t.Fatal("lease must hold exactly the exchanging token")
	}
	e.fail(t, byRun)
	if leased(cred) || denied(dlg) {
		t.Fatal("run end must end the lease")
	}

	byTime, _ := e.wakeRun(t, "wake", 60)
	e.open(t, byTime)
	e.now = e.now.Add(30 * time.Second)
	if _, err := e.exchange(t, dlg, byTime); err != nil || !leased(cred) {
		t.Fatalf("new wake run must lease again: %v", err)
	}
	e.now = e.now.Add(31 * time.Second) // 租约截止取运行开始加超时，不是换取时刻加超时
	if leased(cred) || denied(dlg) {
		t.Fatal("run timeout must end the lease")
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

// 失败路径：没有目标事项的运行、未知凭据、非 delegate 调用者都不被租约改变；没有租约的 Release 是空操作。
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

	bad := e.svc.Identity(command.Bearer{Auth: e.au, Secret: "apa_dlg_unknown"})
	if _, err := e.do(bad, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open"}); exitOf(err) != domain.ExitUnauthenticated {
		t.Fatalf("unknown token: %v", err)
	}
	e.must(t, e.svc.Identity(owner), "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open"})

	if err := e.st.Update(bg, func(tx *store.Tx) error { return e.svc.Release(tx, "run_none") }); err != nil {
		t.Fatalf("release without lease must be a no-op: %v", err)
	}
}
