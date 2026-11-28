package items_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apifm "github.com/xiaohei-info/awesome-personal-agent/api/followmsg"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	km "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	ksearch "github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var bg = context.Background()
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// kernel 是真实账本 + 命令管线 + HTTP 路由 + 内核时钟；CLI 通过真实 HTTP 调它，时间由测试推进。
type kernel struct {
	t      *testing.T
	st     *store.Store
	x      *command.Executor
	auth   *auth.Auth
	url    string
	now    time.Time
	clk    *clock.Clock
	tokens map[auth.Kind]string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, kitems.Module, executors.Module, runner.Module, core.Module, km.Module, ksearch.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	k := &kernel{t: t, st: st, now: t0, auth: auth.New(nil), tokens: map[auth.Kind]string{}}
	k.x = command.New(st, func() time.Time { return k.now })
	if err := projects.Register(k.x); err != nil {
		t.Fatal(err)
	}
	if err := kitems.Register(k.x, kitems.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := decisions.Register(k.x); err != nil {
		t.Fatal(err)
	}
	if err := inbound.RegisterAttach(k.x); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: k.auth, Groups: []core.RouteGroup{apiitems.Routes(k.x, st), apifm.Routes(k.x), apiwhy.Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	k.url = ts.URL
	k.restartClock()
	for _, kind := range []auth.Kind{auth.Observer, auth.Worker, auth.Delegate} {
		if err := st.Update(bg, func(tx *store.Tx) error {
			_, s, err := k.auth.Issue(tx, auth.Spec{Kind: kind, Executor: map[auth.Kind]string{auth.Worker: "wrk1"}[kind], Audience: agentAudience(kind)})
			k.tokens[kind] = s
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// decider 是归属 agent_d、本人放开到「也替我拍板」的 delegate 钥匙；auth.Delegate 那把没有归属，只整理。
	if err := st.Update(bg, func(tx *store.Tx) error {
		_, s, err := k.auth.Issue(tx, auth.Spec{Kind: auth.Delegate, AgentID: "agent_d", Harness: "h", Audience: auth.AudienceInternal})
		if err != nil {
			return err
		}
		k.tokens[decider] = s
		_, err = policy.Update(tx, owner.Actor, "test tier", func(p *policy.Policy) error {
			p.AgentCards["agent_d"] = policy.DefaultCard().With(policy.GroupDecide)
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return k
}

// decider 是测试里的伪种类：tokens 里放开了二档的那把 delegate 钥匙。
const decider auth.Kind = "decider"

// restartClock 模拟内核重启：新建时钟实例，Timer 在账本里。
func (k *kernel) restartClock() {
	clk, err := clock.New(k.st, kitems.HandleTimer, func() time.Time { return k.now })
	if err != nil {
		k.t.Fatal(err)
	}
	k.clk = clk
}

func (k *kernel) advance(d time.Duration) int {
	k.t.Helper()
	k.now = k.now.Add(d)
	n, err := k.clk.Tick(bg)
	if err != nil {
		k.t.Fatal(err)
	}
	return n
}

type run struct {
	code        int
	out, errOut string
}

func (r run) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.out), &m); err != nil {
		t.Fatalf("stdout is not JSON: %q (%v) stderr=%q", r.out, err, r.errOut)
	}
	return m
}

func (k *kernel) apa(kind auth.Kind, args ...string) run {
	k.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": k.tokens[kind], "APA_SESSION": "s1", "APA_HOME": k.t.TempDir()}
	code := client.Main(args, client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: k.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return run{code, out.String(), errb.String()}
}

func (k *kernel) ok(kind auth.Kind, args ...string) run {
	k.t.Helper()
	r := k.apa(kind, args...)
	if r.code != 0 {
		k.t.Fatalf("apa %v: exit %d stderr=%s", args, r.code, r.errOut)
	}
	return r
}

func (k *kernel) idOf(r run) string { return r.json(k.t)["id"].(string) }

func (k *kernel) item(id string) map[string]any {
	k.t.Helper()
	return k.ok(auth.Observer, "item", "show", id).json(k.t)["item"].(map[string]any)
}

func (k *kernel) phase(id string) string { return k.item(id)["phase"].(string) }

func (k *kernel) errCode(r run) string {
	var e struct{ Code string }
	_ = json.Unmarshal([]byte(r.errOut), &e)
	return e.Code
}

var owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}

// own 以本人身份直接走内核命令（Web 才能做的事：开项目、确认建议、指派他人）。
func (k *kernel) own(cmd string, in map[string]any) command.Result {
	k.t.Helper()
	b, _ := json.Marshal(in)
	r, err := k.x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner"})
	if err != nil {
		k.t.Fatal(err)
	}
	return r
}

// openItem 建一个指派给 worker（wrk1）的 open 事项；worker 对自己的事项有写权限。
func (k *kernel) openItem(extra ...map[string]any) string {
	in := map[string]any{"kind": "task", "title": "t", "phase": "open", "assignee": "wrk1"}
	for _, e := range extra {
		for key, v := range e {
			in[key] = v
		}
	}
	var out struct{ ID string }
	_ = json.Unmarshal(k.own("item.create", in).Value, &out)
	return out.ID
}

// flow-item-lifecycle：propose 以 proposed 新建，show 读回；agent 的产出先进建议流。
func TestProposeCreatesProposedItemAndShowReadsItBack(t *testing.T) {
	k := newKernel(t)
	id := k.idOf(k.ok(auth.Observer, "propose", "--kind", "task", "--title", "write report", "--priority", "high", "--tags", "a,b"))
	it := k.item(id)
	if it["phase"] != "proposed" || it["title"] != "write report" || it["priority"] != "high" || it["version"].(float64) != 1 {
		t.Fatalf("item = %v", it)
	}
	if tags := it["tags"].([]any); len(tags) != 2 || tags[1] != "b" {
		t.Fatalf("tags = %v", tags)
	}
}

// ACT-ITEM-03：waiting 必须有 waiting_on。
func TestTransitionToWaitingNeedsWaitingOn(t *testing.T) {
	k := newKernel(t)
	id := k.openItem()
	r := k.apa(auth.Delegate, "item", "transition", id, "--to", "waiting", "--reason", "quote")
	if r.code == 0 || k.phase(id) != "open" {
		t.Fatalf("waiting without waiting_on must fail and keep the phase: %+v", r)
	}
	if e := (map[string]any{}); json.Unmarshal([]byte(r.errOut), &e) != nil || e["code"] == nil || e["why"] == nil || e["next"] == nil {
		t.Fatalf("error body must carry code/why/next: %q", r.errOut)
	}
	k.ok(auth.Delegate, "item", "transition", id, "--to", "waiting", "--reason", "quote", "--waiting-on", "external:报价单")
	it := k.item(id)
	if it["phase"] != "waiting" || it["waiting_on"].(map[string]any)["ref"] != "报价单" {
		t.Fatalf("item = %v", it)
	}
}

// ACT-ITEM-04：needs_input 必须有 ask；带 ask 时进入并保留问题。
func TestTransitionToNeedsInputNeedsAsk(t *testing.T) {
	k := newKernel(t)
	id := k.openItem()
	if r := k.apa(auth.Delegate, "item", "transition", id, "--to", "needs_input", "--reason", "q"); r.code == 0 || k.phase(id) != "open" {
		t.Fatalf("needs_input without ask must fail and keep the phase: %+v", r)
	}
	k.ok(auth.Delegate, "item", "update", id, "--ask", `{"question":"which one?","free_text":true}`, "--reason", "need a choice")
	k.ok(auth.Delegate, "item", "transition", id, "--to", "needs_input", "--reason", "q")
	it := k.item(id)
	if it["phase"] != "needs_input" || it["ask"].(map[string]any)["question"] != "which one?" {
		t.Fatalf("item = %v", it)
	}
}

// ACT-ITEM-05 与 ACT-ITEM-07：dropped 必须有 reason 且不删除；done、dropped 都可重开。
func TestDroppedNeedsReasonAndTerminalItemsReopen(t *testing.T) {
	k := newKernel(t)
	dropped, done := k.openItem(), k.openItem()
	if r := k.apa(auth.Delegate, "item", "transition", dropped, "--to", "dropped"); r.code == 0 || k.phase(dropped) != "open" {
		t.Fatalf("dropped without reason must fail: %+v", r)
	}
	k.ok(auth.Delegate, "item", "transition", dropped, "--to", "dropped", "--reason", "obsolete")
	k.ok(auth.Delegate, "item", "transition", done, "--to", "done", "--reason", "finished")
	if k.phase(dropped) != "dropped" || k.phase(done) != "done" {
		t.Fatal("terminal items must stay queryable in their terminal phase")
	}
	for _, id := range []string{dropped, done} {
		k.ok(auth.Delegate, "item", "transition", id, "--to", "open", "--reason", "back again")
		if k.phase(id) != "open" {
			t.Fatalf("%s did not reopen", id)
		}
	}
}

// ACT-ITEM-13：读不存在的对象退出码 5，且不输出空对象。
func TestShowMissingItemExits5(t *testing.T) {
	k := newKernel(t)
	r := k.apa(auth.Observer, "item", "show", "item_not_exist")
	if r.code != 5 || r.out != "" || k.errCode(r) != "not_found" {
		t.Fatalf("%+v", r)
	}
}

// 关系与筛选：relate 后 show 看到边，list 的筛选生效，--remove 删边。
func TestRelateAndListFilters(t *testing.T) {
	k := newKernel(t)
	parent, child := k.openItem(), k.openItem()
	other := k.idOf(k.ok(auth.Observer, "propose", "--kind", "note", "--title", "n"))
	k.ok(auth.Worker, "item", "relate", child, "--to", parent, "--type", "parent", "--reason", "split")
	rels := k.ok(auth.Observer, "item", "show", child).json(t)["relations"].([]any)
	if len(rels) != 1 || rels[0].(map[string]any)["to"] != parent {
		t.Fatalf("relations = %v", rels)
	}
	list := func(args ...string) []any {
		return k.ok(auth.Observer, append([]string{"item", "list"}, args...)...).json(t)["items"].([]any)
	}
	if n := len(list()); n != 3 {
		t.Fatalf("all = %d", n)
	}
	if l := list("--kind", "note"); len(l) != 1 || l[0].(map[string]any)["id"] != other {
		t.Fatalf("kind filter = %v", l)
	}
	if n := len(list("--phase", "open")); n != 2 {
		t.Fatalf("phase filter = %d", n)
	}
	// ACT-WBN-12/13/14：新参数 --q/--sort/--planned-for/--tag 与旧参数共存。
	k.ok(auth.Worker, "item", "update", child, "--planned-for", "2026-05-01", "--tags", "t1", "--body", "护照续签", "--reason", "plan")
	if l := list("--q", "护照"); len(l) != 1 || l[0].(map[string]any)["id"] != child {
		t.Fatalf("--q = %v", l)
	}
	if l := list("--planned-for", "2026-05-01", "--phase", "open", "--tag", "t1"); len(l) != 1 {
		t.Fatalf("--planned-for/--tag = %v", l)
	}
	if l := list("--sort", "planned_for", "--phase", "open"); len(l) != 2 || l[0].(map[string]any)["id"] != child {
		t.Fatalf("--sort planned_for = %v", l)
	}
	// 默认列表不带大字段，--full 带全量。
	body := func(args ...string) any { return list(append(args, "--q", "护照")...)[0].(map[string]any)["body"] }
	if body() != nil || body("--full") != "护照续签" {
		t.Fatalf("compact/full body = %v / %v", body(), body("--full"))
	}
	if r := k.apa(auth.Observer, "item", "list", "--sort", "bogus"); r.code == 0 {
		t.Fatal("unknown sort must fail")
	}
	// 有界：--limit 一页，带总数与续页命令；按续页能翻完（§17.2）。
	page := k.ok(auth.Observer, "item", "list", "--limit", "2").json(t)
	if len(page["items"].([]any)) != 2 || page["total"] != float64(3) || page["next_cursor"] != "2" ||
		!strings.Contains(page["next"].([]any)[0].(string), "--cursor '2'") {
		t.Fatalf("page 1 = %v", page)
	}
	if l := list("--limit", "2", "--cursor", "2"); len(l) != 1 {
		t.Fatalf("page 2 = %v", l)
	}
	if r := k.apa(auth.Observer, "item", "list", "--limit", "201"); r.code != domain.ExitUsage {
		t.Fatalf("limit over 200: %d", r.code)
	}
	k.ok(auth.Worker, "item", "relate", child, "--to", parent, "--type", "parent", "--remove", "--reason", "undo")
	if rels := k.ok(auth.Observer, "item", "show", child).json(t)["relations"].([]any); len(rels) != 0 {
		t.Fatalf("relation not removed: %v", rels)
	}
}

// delegate 经 CLI 改他人事项的标题与建关系是一档：直接生效，不要本人原话；Event 记的是 Agent 自己，不带 on_behalf_of，
// 依据记为一档整理。
func TestDelegateUpdateAndRelateAreOrganize(t *testing.T) {
	k := newKernel(t)
	a, b := k.openItem(), k.openItem()
	k.ok(auth.Delegate, "item", "update", a, "--title", "new", "--reason", "r")
	k.ok(auth.Delegate, "item", "relate", a, "--to", b, "--type", "relates", "--reason", "r")
	if k.item(a)["title"] != "new" {
		t.Fatal("tier-1 update must apply")
	}
	var n int
	if err := k.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, a)
		for _, e := range es {
			if e.Event.Type != domain.EvItemUpdated {
				continue
			}
			n++
			if e.Event.Actor.Type != domain.ActorAgent || e.Event.Actor.OnBehalfOf != "" || e.Event.Evidence != "" ||
				e.Event.Basis == nil || e.Event.Basis.Via != domain.BasisOrganize {
				t.Fatalf("event %d must record the Agent itself on tier 1: %+v", e.Event.Seq, e.Event)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("item.updated events = %d", n)
	}
}

// 权限与版本：observer 不能 add（4）；--version 过期返回 6；同一幂等键重放返回同一结果。
func TestAuthorizationVersionAndIdempotency(t *testing.T) {
	k := newKernel(t)
	if r := k.apa(auth.Observer, "item", "update", k.openItem(), "--title", "x", "--reason", "r"); r.code != 4 {
		t.Fatalf("observer update: %+v", r)
	}
	id := k.openItem()
	k.ok(auth.Worker, "item", "update", id, "--title", "v2", "--reason", "r", "--version", "1")
	if r := k.apa(auth.Worker, "item", "update", id, "--title", "v3", "--reason", "r", "--version", "1"); r.code != 6 {
		t.Fatalf("stale version must exit 6: %+v", r)
	}
	a := k.ok(auth.Worker, "propose", "--kind", "task", "--title", "idem", "--idempotency-key", "K1")
	b := k.ok(auth.Worker, "propose", "--kind", "task", "--title", "idem", "--idempotency-key", "K1")
	if k.idOf(a) != k.idOf(b) || b.json(t)["replayed"] != true {
		t.Fatalf("replay: %s vs %s", a.out, b.out)
	}
}

// ACT-CLK-01：到期由内核时钟触发，不需要任何会话；到期前不触发。
func TestDueItemIsWokenByKernelClock(t *testing.T) {
	k := newKernel(t)
	id := k.openItem()
	due := k.now.Add(time.Minute).Format(time.RFC3339)
	k.ok(auth.Worker, "item", "transition", id, "--to", "waiting", "--reason", "sleep", "--waiting-on", "time:"+due)
	if n := k.advance(30 * time.Second); n != 0 || k.phase(id) != "waiting" {
		t.Fatalf("fired early: n=%d", n)
	}
	if n := k.advance(2 * time.Minute); n != 1 || k.phase(id) != "open" {
		t.Fatalf("not woken: n=%d phase=%s", n, k.phase(id))
	}
	if k.item(id)["waiting_on"] != nil {
		t.Fatal("waiting_on must be cleared on wake")
	}
}

// ACT-CLK-02：停机期间错过的一次性定时，重启后全部触发。
func TestMissedDueItemsAllFireAfterRestart(t *testing.T) {
	k := newKernel(t)
	var ids []string
	due := k.now.Add(time.Minute).Format(time.RFC3339)
	for i := 0; i < 3; i++ {
		id := k.openItem()
		k.ok(auth.Worker, "item", "transition", id, "--to", "waiting", "--reason", "sleep", "--waiting-on", "time:"+due)
		ids = append(ids, id)
	}
	k.now = k.now.Add(3 * time.Minute) // 服务停了 3 分钟
	k.restartClock()
	if n, err := k.clk.Tick(bg); err != nil || n != 3 {
		t.Fatalf("fired %d err=%v", n, err)
	}
	for _, id := range ids {
		if k.phase(id) != "open" {
			t.Fatalf("%s missed", id)
		}
	}
}

// ACT-ALW-06：不能先建后移入项目——内核拒绝（退出码 21），project 不变。
func TestMovingIntoProjectWithDisallowedExecutorExits21(t *testing.T) {
	k := newKernel(t)
	var p struct{ ID string }
	_ = json.Unmarshal(k.own("project.create", map[string]any{"name": "acc-work"}).Value, &p)
	k.own("project.allowed_executors.set", map[string]any{"id": p.ID, "executors": []string{"work-host.codex"}})

	id := k.openItem(map[string]any{"assignee": "other-host.cli"})
	r := k.apa(auth.Delegate, "item", "update", id, "--project", p.ID, "--reason", "move")
	if r.code != 21 || k.errCode(r) != "executor_not_allowed" {
		t.Fatalf("%+v", r)
	}
	if k.item(id)["project"] != nil {
		t.Fatalf("project must not change: %v", k.item(id))
	}
}

// apa inbox：待决（needs_input）和建议（proposed）各一组；已完成的不列出。
func TestInboxListsPendingAndSuggestions(t *testing.T) {
	k := newKernel(t)
	sug := k.idOf(k.ok(auth.Observer, "propose", "--kind", "task", "--title", "suggest"))
	ask := k.openItem(map[string]any{"kind": "decision", "phase": "needs_input", "ask": map[string]any{"question": "A or B?", "free_text": true}})
	k.ok(auth.Worker, "item", "transition", k.openItem(), "--to", "done", "--reason", "x")
	m := k.ok(auth.Observer, "inbox").json(t)
	ids := func(key string) (out []string) {
		for _, v := range m[key].([]any) {
			out = append(out, v.(map[string]any)["id"].(string))
		}
		return
	}
	if p, s := ids("pending"), ids("suggestions"); len(p) != 1 || p[0] != ask || len(s) != 1 || s[0] != sug {
		t.Fatalf("inbox = %s", k.apa(auth.Observer, "inbox").out)
	}
}

// 用法错误一律退出码 2，且不发请求。
func TestUsageErrorsExit2(t *testing.T) {
	k := newKernel(t)
	for _, args := range [][]string{
		{"item"}, {"item", "bogus"}, {"item", "show"}, {"item", "show", "a", "b"}, {"item", "list", "--bogus"},
		{"item", "transition", "x"}, {"item", "relate", "x"}, {"item", "add", "--waiting-on", "bad"},
		{"item", "add", "--ask", "{not json"}, {"propose", "extra"}, {"inbox", "extra"},
		{"item", "update", "--title", "no id first"},
	} {
		if r := k.apa(auth.Worker, args...); r.code != 2 || k.errCode(r) != "invalid_request" {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

// item add 与 propose 同走创建：不带 --phase 即 proposed；worker 直接建 open 被内核拒绝（4）；字段原样入账。
func TestItemAddWritesFieldsAndKernelGuardsPhase(t *testing.T) {
	k := newKernel(t)
	id := k.idOf(k.ok(auth.Worker, "item", "add", "--kind", "task", "--title", "a", "--summary", "s", "--body", "b",
		"--status-label", "todo", "--assignee", "wrk1", "--spec", `{"x":1}`, "--reason", "why"))
	it := k.item(id)
	if it["phase"] != "proposed" || it["summary"] != "s" || it["body"] != "b" || it["status_label"] != "todo" || it["assignee"] != "wrk1" {
		t.Fatalf("item = %v", it)
	}
	if r := k.apa(auth.Worker, "item", "add", "--kind", "task", "--title", "a", "--phase", "open", "--reason", "r"); r.code != 4 {
		t.Fatalf("worker must not create open items: %+v", r)
	}
}

// update 空值清空字段（null），waiting_on 与阶段无关时也可清除，evidence 走请求头。
func TestUpdateEmptyValueClearsField(t *testing.T) {
	k := newKernel(t)
	id := k.openItem(map[string]any{"summary": "old"})
	k.ok(auth.Worker, "item", "update", id, "--summary", "", "--reason", "clear")
	if k.item(id)["summary"] != nil {
		t.Fatalf("summary not cleared: %v", k.item(id))
	}
}

func TestUpdateWithoutChangesIsRejectedByKernel(t *testing.T) {
	k := newKernel(t)
	if r := k.apa(auth.Worker, "item", "update", k.openItem(), "--reason", "nothing"); r.code != 2 {
		t.Fatalf("%+v", r)
	}
}

// 让事项成为承诺是二档：只整理的 delegate 直接建 open 事项为 23，账本不变；放开拍板的 Agent 可以，建议照旧谁都能提。
func TestCommitmentNeedsDecideTier(t *testing.T) {
	k := newKernel(t)
	add := []string{"item", "add", "--kind", "task", "--title", "c", "--phase", "open", "--reason", "r"}
	if r := k.apa(auth.Delegate, add...); r.code != 23 || k.errCode(r) != string(domain.CodeTierClosed) {
		t.Fatalf("organize-only commit: %+v", r)
	}
	if id := k.idOf(k.ok(decider, add...)); k.phase(id) != "open" {
		t.Fatal("decide-tier commit must apply")
	}
	if id := k.idOf(k.ok(auth.Delegate, "propose", "--kind", "task", "--title", "p", "--reason", "r")); k.phase(id) != "proposed" {
		t.Fatal("proposals stay open to every Agent")
	}
}

func (k *kernel) progress(id string) []any { // item show 的 progress[]：讨论区里的发言
	k.t.Helper()
	return k.ok(auth.Observer, "item", "show", id).json(k.t)["progress"].([]any)
}

func ymd(d time.Time) string { return d.In(time.Local).Format("2006-01-02") }

// ACT-WBN-05/06：--planned-for 写、清；非法值退出码 2 且事项不变。
func TestItemPlannedFor(t *testing.T) {
	k := newKernel(t)
	id := k.openItem()
	tmrw := ymd(k.now.AddDate(0, 0, 1))
	k.ok(auth.Worker, "item", "update", id, "--planned-for", tmrw)
	if got := k.item(id)["planned_for"]; got != tmrw {
		t.Fatalf("planned_for = %v", got)
	}
	v := k.item(id)["version"]
	for _, bad := range []string{"2026-13-40", "2026-11-12T10:00:00Z"} {
		r := k.apa(auth.Worker, "item", "update", id, "--planned-for", bad)
		if r.code != 2 || !strings.Contains(r.errOut, "YYYY-MM-DD") {
			t.Fatalf("%s: exit %d %s", bad, r.code, r.errOut)
		}
	}
	if k.item(id)["planned_for"] != tmrw || k.item(id)["version"] != v {
		t.Fatal("rejected updates changed the item")
	}
	k.ok(auth.Worker, "item", "update", id, "--planned-for", "")
	if _, has := k.item(id)["planned_for"]; has {
		t.Fatal("empty --planned-for must clear")
	}
	n := k.idOf(k.ok(auth.Worker, "item", "add", "--kind", "task", "--title", "WB-F2", "--planned-for", tmrw, "--phase", "proposed"))
	if k.item(n)["planned_for"] != tmrw {
		t.Fatal("item add --planned-for")
	}
}

// ACT-WBN-09/10/11：发言追加、未来日期退出码 2、越权 4、delegate 缺依据 23。
func TestItemComment(t *testing.T) {
	k := newKernel(t)
	id := k.openItem()
	yday, tmrw := ymd(k.now.AddDate(0, 0, -1)), ymd(k.now.AddDate(0, 0, 1))
	k.ok(auth.Worker, "item", "comment", id, "--text", "联系了供应商", "--on", yday)
	ps := k.progress(id)
	p := ps[len(ps)-1].(map[string]any)
	if k.phase(id) != "open" || len(ps) != 1 || p["text"] != "联系了供应商" || p["on"] != yday || p["actor"] != "wrk1" {
		t.Fatalf("progress = %+v phase=%s", ps, k.phase(id))
	}
	if r := k.apa(auth.Worker, "item", "comment", id, "--text", "未来", "--on", tmrw); r.code != 2 || len(k.progress(id)) != 1 {
		t.Fatalf("future on: exit %d %s", r.code, r.errOut)
	}
	if r := k.apa(auth.Worker, "item", "comment", id); r.code != 2 {
		t.Fatalf("missing --text: exit %d", r.code)
	}
	other := k.openItem(map[string]any{"assignee": "someone-else"})
	if r := k.apa(auth.Worker, "item", "comment", other, "--text", "x"); r.code != 4 {
		t.Fatalf("worker on another's item: exit %d %s", r.code, r.errOut)
	}
	// 写进展是一档：delegate 对任何事项都直接可写。
	k.ok(auth.Delegate, "item", "comment", other, "--text", "已订")
	if len(k.progress(other)) != 1 {
		t.Fatal("delegate comment missing")
	}
	var seen bool
	_ = k.st.View(bg, func(tx *store.Tx) error {
		es, _ := eventlog.ReadObject(tx, id)
		for _, e := range es {
			seen = seen || e.Event.Type == domain.EvItemProgressAdded
		}
		return nil
	})
	if !seen {
		t.Fatal("no item.progress_added event")
	}
}

// 讨论区：发言与回复按回复关系出现在讨论流里；旧进展（没有 reply_to）照常出现；决定与发言在同一条流；
// 回复只挂同一事项内已有的发言；一页不超过读取上限，用 --before 读更早的，单条过长只截正文。
func TestItemThread(t *testing.T) {
	k := newKernel(t)
	id, other := k.openItem(), k.openItem()
	k.own("item.progress", map[string]any{"id": id, "text": "旧进展"})
	first := k.ok(auth.Worker, "item", "comment", id, "--text", "第一条发言").json(t)["comment"].(map[string]any)["id"].(string)
	k.ok(auth.Delegate, "item", "comment", id, "--text", "回复第一条", "--reply-to", first)
	otherC := k.ok(auth.Worker, "item", "comment", other, "--text", "别处").json(t)["comment"].(map[string]any)["id"].(string)
	for _, bad := range []string{otherC, id + ".p000099"} {
		if r := k.apa(auth.Worker, "item", "comment", id, "--text", "x", "--reply-to", bad); r.code != 2 || !strings.Contains(r.errOut, "reply_to") {
			t.Fatalf("reply to %s: exit %d %s", bad, r.code, r.errOut)
		}
	}
	k.own("decision.request", map[string]any{"id": id, "ask": map[string]any{"question": "q?", "options": []map[string]any{{"id": "A", "label": "a"}, {"id": "B", "label": "b"}}}})

	th := k.ok(auth.Observer, "item", "thread", id).json(t)
	var said []string
	var replyOf string
	kinds := map[string]bool{}
	for _, raw := range th["entries"].([]any) {
		e := raw.(map[string]any)
		kinds[e["kind"].(string)] = true
		if _, has := e["steps"]; has {
			t.Fatalf("compact thread carries raw events: %v", e)
		}
		if e["kind"] == "progress" {
			a := e["args"].(map[string]any)
			said = append(said, a["text"].(string))
			if a["text"] == "回复第一条" {
				replyOf, _ = a["reply_to"].(string)
			}
		}
	}
	if strings.Join(said, "|") != "旧进展|第一条发言|回复第一条" || replyOf != first || !kinds["decision"] {
		t.Fatalf("thread = %v, reply_to = %q, kinds = %v", said, replyOf, kinds)
	}

	long := strings.Repeat("长", 1500)
	for range 4 {
		k.ok(auth.Worker, "item", "comment", other, "--text", long)
	}
	r := k.ok(auth.Observer, "item", "thread", other)
	page := r.json(t)
	if n := utf8.RuneCountInString(r.out); n > kcontext.InjectBudget || page["more"] == nil || len(page["entries"].([]any)) < 2 {
		t.Fatalf("first page: %d runes, more = %v, %d entries", n, page["more"], len(page["entries"].([]any)))
	}
	before := strconv.FormatInt(int64(page["before"].(float64)), 10)
	older := k.ok(auth.Observer, "item", "thread", other, "--before", before).json(t)
	if len(older["entries"].([]any)) == 0 {
		t.Fatal("older page is empty")
	}
	k.ok(auth.Worker, "item", "comment", other, "--text", strings.Repeat("超", 5000))
	r = k.ok(auth.Observer, "item", "thread", other, "--limit", "1")
	last := r.json(t)["entries"].([]any)[0].(map[string]any)["args"].(map[string]any)
	if n := utf8.RuneCountInString(r.out); n > kcontext.InjectBudget || last["truncated"] != true {
		t.Fatalf("oversized comment: %d runes, args = %v", n, last["truncated"])
	}
}

// ACT-WBN-07：apa today 输出四组，内容同 GET /today。
func TestToday(t *testing.T) {
	k := newKernel(t)
	now := time.Now()
	a := k.openItem(map[string]any{"planned_for": ymd(now.AddDate(0, 0, -1))})
	k.openItem(map[string]any{"planned_for": ymd(now.AddDate(0, 0, 1))})
	out := k.ok(auth.Observer, "today").json(t)
	for _, g := range []string{"planned", "due", "pending", "stale"} {
		if _, ok := out[g].([]any); !ok {
			t.Fatalf("group %s missing: %v", g, out)
		}
	}
	pl := out["planned"].([]any)
	if len(pl) != 1 || pl[0].(map[string]any)["id"] != a {
		t.Fatalf("planned = %v", pl)
	}
	if r := k.apa(auth.Observer, "today", "x"); r.code != 2 {
		t.Fatalf("today with args: exit %d", r.code)
	}
}

// ACT-MSG-14/17：apa item relate --key 登记与移除关联键，show 看到；观察者退出码 4、账本不变；delegate 是一档直接可做。
func TestRelateKeyRegistersAndRemoves(t *testing.T) {
	k := newKernel(t)
	id := k.openItem()
	keys := func() []any { return k.ok(auth.Observer, "item", "show", id).json(t)["correlation_keys"].([]any) }

	if r := k.apa(auth.Observer, "item", "relate", id, "--key", "fm-x"); r.code != 4 {
		t.Fatalf("observer: %+v", r)
	}
	if len(keys()) != 0 {
		t.Fatalf("rejected registrations left keys: %v", keys())
	}
	k.ok(auth.Delegate, "item", "relate", id, "--key", "fm-thread-1", "--domain", "acc-fm")
	got := keys()
	if len(got) != 1 || got[0].(map[string]any)["key"] != "fm-thread-1" || got[0].(map[string]any)["domain"] != "acc-fm" {
		t.Fatalf("keys = %v", got)
	}
	if r := k.apa(auth.Worker, "item", "relate", id, "--key", "fm-thread-1", "--domain", "acc-fm"); r.code != 6 {
		t.Fatalf("duplicate: %+v", r)
	}
	k.ok(auth.Delegate, "item", "relate", id, "--key", "fm-thread-1", "--domain", "acc-fm", "--remove")
	if len(keys()) != 0 {
		t.Fatalf("key not removed: %v", keys())
	}
	if r := k.apa(auth.Worker, "item", "relate", id, "--key", "k", "--to", id, "--type", "relates"); r.code != 2 {
		t.Fatalf("key with --to: %+v", r)
	}
}

// ACT-CLI-01/02：--deadline 取 RFC 3339 或日期（本机当天 23:59:59）；update 传空串清除并取消到期定时。
func TestItemDeadlineFlag(t *testing.T) {
	k := newKernel(t)
	want := time.Date(2026, 3, 5, 23, 59, 59, 0, time.Local)
	id := k.idOf(k.ok(decider, "item", "add", "--kind", "task", "--title", "d", "--phase", "open", "--assignee", "wrk1", "--deadline", "2026-03-05"))
	got, _ := time.Parse(time.RFC3339, k.item(id)["deadline"].(string))
	if !got.Equal(want) {
		t.Fatalf("date deadline = %v, want %v", got, want)
	}
	k.ok(auth.Worker, "item", "update", id, "--deadline", "2026-01-01T00:30:00Z", "--reason", "r")
	if k.item(id)["deadline"] != "2026-01-01T00:30:00Z" {
		t.Fatalf("rfc3339 deadline: %v", k.item(id)["deadline"])
	}
	k.ok(auth.Worker, "item", "update", id, "--deadline", "", "--reason", "r")
	if k.item(id)["deadline"] != nil {
		t.Fatalf("deadline not cleared: %v", k.item(id))
	}
	if n := k.advance(time.Hour); n != 0 {
		t.Fatalf("cleared deadline still fired: %d", n)
	}
	if r := k.apa(auth.Worker, "propose", "--kind", "task", "--title", "p", "--deadline", "tomorrow"); r.code != 2 {
		t.Fatalf("bad deadline: exit %d %s", r.code, r.errOut)
	}
	p := k.idOf(k.ok(auth.Worker, "propose", "--kind", "task", "--title", "p", "--deadline", "2026-03-05T10:00:00Z"))
	if k.item(p)["deadline"] != "2026-03-05T10:00:00Z" {
		t.Fatalf("propose deadline: %v", k.item(p))
	}
}

// apa item list --origin-source/--origin-ref 按事项的 origin 精确筛选（§7.1）。
func TestItemListByOrigin(t *testing.T) {
	k := newKernel(t)
	for _, ref := range []string{"O-0001", "O-0002", "O-0003"} {
		k.ok(auth.Worker, "propose", "--kind", "task", "--title", ref, "--origin", `{"source":"acc-src","ref":"`+ref+`"}`)
	}
	ids := func(r run) []string {
		var out []string
		for _, it := range r.json(t)["items"].([]any) {
			out = append(out, it.(map[string]any)["origin"].(map[string]any)["ref"].(string))
		}
		return out
	}
	if got := ids(k.ok(auth.Observer, "item", "list", "--origin-source", "acc-src", "--full")); len(got) != 3 {
		t.Fatalf("by source: %v", got)
	}
	if got := ids(k.ok(auth.Observer, "item", "list", "--origin-source", "acc-src", "--origin-ref", "O-0001", "--full")); len(got) != 1 || got[0] != "O-0001" {
		t.Fatalf("by source and ref: %v", got)
	}
	if got := ids(k.ok(auth.Observer, "item", "list", "--origin-source", "other", "--full")); len(got) != 0 {
		t.Fatalf("other source: %v", got)
	}
}

// ACT-CLI-03：--origin/--ext 是 JSON，非 JSON 退出码 2；import: 前缀被内核拒绝（退出码 2），item 不创建。
func TestItemOriginAndExtFlags(t *testing.T) {
	k := newKernel(t)
	id := k.idOf(k.ok(auth.Worker, "propose", "--kind", "task", "--title", "o", "--origin", `{"source":"notes","actor":"me","ref":"n-1"}`, "--ext", `{"k":"v"}`))
	it := k.item(id)
	if o := it["origin"].(map[string]any); o["source"] != "notes" || o["ref"] != "n-1" || o["actor"] != "me" {
		t.Fatalf("origin = %v", o)
	}
	if it["ext"].(map[string]any)["k"] != "v" {
		t.Fatalf("ext = %v", it["ext"])
	}
	for _, args := range [][]string{
		{"propose", "--kind", "task", "--title", "x", "--origin", "notjson"},
		{"propose", "--kind", "task", "--title", "x", "--ext", "{"},
		{"propose", "--kind", "task", "--title", "x", "--origin", `{"source":"import:notes"}`},
		{"item", "add", "--kind", "task", "--title", "x", "--origin", `{"source":"import:notes"}`},
	} {
		if r := k.apa(auth.Worker, args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, r.code, r.errOut)
		}
	}
	oid := k.openItem()
	if r := k.apa(auth.Worker, "item", "update", oid, "--origin", `{"source":"import:notes"}`, "--reason", "r"); r.code != 2 {
		t.Fatalf("update import origin: exit %d %s", r.code, r.errOut)
	}
	k.ok(auth.Worker, "item", "update", oid, "--origin", `{"source":"manual"}`, "--ext", `{"a":1}`, "--reason", "r")
	if k.item(oid)["origin"].(map[string]any)["source"] != "manual" {
		t.Fatalf("origin not updated: %v", k.item(oid))
	}
}

// agentAudience：测试里的三级 token 代表 APA 内部的 Agent，读来源不受档位限制（§14.2）；ext、webhook 是外部钥匙。
func agentAudience(k auth.Kind) string {
	if k == auth.Observer || k == auth.Worker || k == auth.Delegate {
		return auth.AudienceInternal
	}
	return ""
}

// item show 默认截正文前 2000 字、只给最近 5 条进展，写明总量并给出分段读取的命令；正文与挂靠消息用 --part 分段取其余，
// 更早的发言在讨论流（apa item thread）里读，--full 照旧给全部。
func TestItemShowIsBoundedAndSegmented(t *testing.T) {
	k := newKernel(t)
	id := k.openItem()
	long := strings.Repeat("长", 2500) + "尾"
	k.ok(auth.Worker, "item", "update", id, "--body", long, "--reason", "r")
	for i := range 7 {
		k.ok(auth.Worker, "item", "comment", id, "--text", "进展"+strconv.Itoa(i))
	}
	out := k.ok(auth.Observer, "item", "show", id).json(t)
	parts := out["parts"].(map[string]any)
	if b := out["item"].(map[string]any)["body"].(string); utf8.RuneCountInString(b) != 2000 || parts["body"].(map[string]any)["chars"] != float64(2501) {
		t.Fatalf("body head: %v", parts)
	}
	if ps := out["progress"].([]any); len(ps) != 5 || ps[4].(map[string]any)["text"] != "进展6" || parts["progress"].(map[string]any)["count"] != float64(7) {
		t.Fatalf("latest progress: %v", ps)
	}
	if n := out["next"].([]any); len(n) != 3 || n[0] != "apa item show "+id+" --part body --offset 2000" || n[1] != "apa item thread "+id {
		t.Fatalf("next: %v", n) // 更早的发言在讨论流里读
	}
	seg := k.ok(auth.Observer, "item", "show", id, "--part", "body", "--offset", "2000").json(t)
	if seg["body"] != strings.Repeat("长", 500)+"尾" || len(seg["next"].([]any)) != 0 {
		t.Fatalf("body segment: %v", seg)
	}
	if full := k.ok(auth.Observer, "item", "show", id, "--full").json(t); full["item"].(map[string]any)["body"] != long || len(full["progress"].([]any)) != 7 {
		t.Fatal("--full must give everything")
	}
	for _, bad := range []string{"nope", "progress"} {
		if r := k.apa(auth.Observer, "item", "show", id, "--part", bad); r.code != domain.ExitUsage {
			t.Fatalf("bad part %s: %d", bad, r.code)
		}
	}
}
