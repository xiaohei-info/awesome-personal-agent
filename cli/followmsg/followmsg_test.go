package followmsg_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apicontext "github.com/xiaohei-info/awesome-personal-agent/api/context"
	apidel "github.com/xiaohei-info/awesome-personal-agent/api/delegation"
	apifm "github.com/xiaohei-info/awesome-personal-agent/api/followmsg"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/background"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/context"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/delegation"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/followmsg"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/allowlist"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	kdel "github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const (
	workHost  = "work-host.cli"
	otherHost = "other-host.cli"
	pr        = "https://git.example/org/web/pull/1"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	src   = domain.Source{ID: "src_t", Type: domain.SourceType("script"), SourceKind: "review", Enabled: true}
)

// world 是真实内核（账本、命令管线、委派、跟进消息归属）经 HTTP 对外，CLI 以各级凭据调用它。
type world struct {
	t    *testing.T
	st   *store.Store
	au   *auth.Auth
	x    *command.Executor
	fm   *followmsg.Service
	ts   *httptest.Server
	proj string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		kitems.Module, executors.Module, kdel.Module, runner.Module, delivery.Module, kcontext.Module, proc.Module, background.Module, runidentity.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	errs := func(_ command.Commit, err error) { t.Error(err) }
	fm, err := followmsg.New(followmsg.Options{Store: st, Exec: x, Owner: "owner", OnError: func(err error) { t.Error(err) }})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{projects.Register(x), kitems.Register(x, kitems.NewKinds()),
		executors.Register(x, executors.Options{Owner: "owner", Project: projects.Get}),
		kdel.Register(x, kdel.Options{Owner: "owner", OnError: errs}), fm.Register(x)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	a := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apidel.Routes(x), apifm.Routes(x), apiitems.Routes(x, st), apicontext.Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, st: st, au: a, x: x, fm: fm, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(w.ts.Close)
	for _, h := range []string{workHost, otherHost} {
		w.cmd("executor.put", map[string]any{"id": h, "kind": "external-cli"})
	}
	var p struct{ ID string }
	_ = json.Unmarshal(w.cmd("project.create", map[string]any{"name": "acc-work"}), &p)
	w.proj = p.ID
	return w
}

func (w *world) cmd(name string, in any) json.RawMessage {
	w.t.Helper()
	b, _ := json.Marshal(in)
	res, err := w.x.Do(bg, command.Request{Identity: owner, Command: name, Input: b})
	if err != nil {
		w.t.Fatal(name, err)
	}
	return res.Value
}

func (w *world) token(kind auth.Kind, executor string) string {
	w.t.Helper()
	var secret string
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := w.au.Issue(tx, auth.Spec{Kind: kind, Executor: executor})
		secret = s
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return secret
}

type result struct {
	code        int
	out, errOut string
}

func (w *world) apa(token string, args ...string) result {
	w.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": w.ts.URL, "APA_TOKEN": token, "APA_HOME": w.t.TempDir()}
	code := client.Main(append([]string{"--evidence", "owner: go ahead"}, args...), client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: w.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return result{code, out.String(), errb.String()}
}

func errCode(t *testing.T, r result) domain.Code {
	t.Helper()
	var e domain.Error
	if err := json.Unmarshal([]byte(r.errOut), &e); err != nil {
		t.Fatalf("stderr is not an error body: %q", r.errOut)
	}
	return e.Code
}

// delegated 委派一个子事项给 workHost，并回执一次带上 PR，使 PR 成为关联键；返回子事项 id。
func (w *world) delegated(wrk string) string {
	w.t.Helper()
	var p, c struct{ ID string }
	_ = json.Unmarshal(w.cmd("item.create", map[string]any{"kind": "task", "title": "fix bug", "phase": "open", "project": w.proj}), &p)
	dlg := w.token(auth.Delegate, "")
	r := w.apa(dlg, "delegate", "--parent", p.ID, "--assignee", workHost)
	if err := json.Unmarshal([]byte(r.out), &c); err != nil || r.code != 0 {
		w.t.Fatalf("delegate: %+v", r)
	}
	if r := w.apa(wrk, "report", c.ID, "--outcome", "progress", "--summary", "s", "--pr", pr); r.code != 0 {
		w.t.Fatalf("report: %+v", r)
	}
	return c.ID
}

// message 落一条 PR 评论并让跟进消息归属把它挂到子事项上；返回事件 id。
func (w *world) message(id string) string {
	w.t.Helper()
	ev := domain.SourceEvent{ID: id, Type: "pr.comment", Subject: "review " + id, Detail: "请改命名", OccurredAt: time.Now(), CorrelationKeys: []string{pr}, Sender: "alice"}
	var got domain.SourceEvent
	err := w.st.Update(bg, func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, src, ev); err != nil {
			return err
		}
		es, err := runner.Events(tx, src.ID)
		for _, e := range es {
			if e.DedupeKey.EventID == id {
				got = e
			}
		}
		return err
	})
	if err != nil || got.ID == "" {
		w.t.Fatalf("append: %v", err)
	}
	if ok, _, err := w.fm.Intercept(bg, got, ""); err != nil || !ok {
		w.t.Fatalf("attach: %v %v", ok, err)
	}
	return got.ID
}

func (w *world) state(id string) string {
	w.t.Helper()
	var s string
	if err := w.st.View(bg, func(tx *store.Tx) error {
		e, err := runner.Event(tx, id)
		s = e.AttachState
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) forward(item string) followmsg.Forwarded {
	w.t.Helper()
	var f followmsg.Forwarded
	if err := w.st.Update(bg, func(tx *store.Tx) (err error) { f, err = followmsg.Forward(tx, item, nil); return }); err != nil {
		w.t.Fatal(err)
	}
	return f
}

// flow-follow-messages / ACT-MSG-05、09：绑定的 worker 交回一次并附理由，消息退出挂靠；
// 缺理由退出码 2，其他 worker 退出码 4 且状态不变，再交回同一消息退出码 6。
func TestHandbackOnceByOwningWorker(t *testing.T) {
	w := newWorld(t)
	wrk, foreign := w.token(auth.Worker, workHost), w.token(auth.Worker, otherHost)
	child := w.delegated(wrk)
	msg := w.message("c1")

	if r := w.apa(wrk, "handback", msg); r.code != 2 || errCode(t, r) != domain.CodeInvalidRequest {
		t.Fatalf("no reason: %+v", r)
	}
	if r := w.apa(wrk, "handback", "--reason", "x"); r.code != 2 {
		t.Fatalf("no message id: %+v", r)
	}
	if r := w.apa(foreign, "handback", msg, "--reason", "not mine"); r.code != 4 || errCode(t, r) != domain.CodePermissionDenied {
		t.Fatalf("foreign worker: %+v", r)
	}
	if s := w.state(msg); s != runner.AttachPending {
		t.Fatalf("rejected handbacks changed state to %s", s)
	}

	if r := w.apa(wrk, "handback", msg, "--reason", "不归我"); r.code != 0 || !strings.Contains(r.out, msg) {
		t.Fatalf("handback: %+v", r)
	}
	if s := w.state(msg); s != runner.AttachHandedBack {
		t.Fatalf("state after handback = %s", s)
	}
	if r := w.apa(wrk, "handback", msg, "--reason", "again"); r.code != 6 || errCode(t, r) != domain.CodeConflict {
		t.Fatalf("second handback: %+v", r)
	}
	if f := w.forward(child); len(f.Messages) != 0 {
		t.Fatalf("handed-back message still forwarded: %+v", f)
	}
	if r := w.apa(wrk, "handback", "no-such-msg", "--reason", "x"); r.code != 5 {
		t.Fatalf("unknown message: %+v", r)
	}
}

// ACT-MSG-03/04：apa report --ack-cursor 只确认游标之前已转发的消息，其后到的消息仍待转发；没带游标不确认。
func TestReportAckCursorProcessesOnlyEarlierMessages(t *testing.T) {
	w := newWorld(t)
	wrk := w.token(auth.Worker, workHost)
	child := w.delegated(wrk)
	c1 := w.message("c1")
	f := w.forward(child)
	if len(f.Messages) != 1 || f.Cursor == "" || w.state(c1) != runner.AttachForwarded {
		t.Fatalf("forwarded %+v", f)
	}
	c2 := w.message("c2")

	if r := w.apa(wrk, "report", child, "--outcome", "progress", "--summary", "s"); r.code != 0 || w.state(c1) != runner.AttachForwarded {
		t.Fatalf("report without cursor: %+v state %s", r, w.state(c1))
	}
	if r := w.apa(wrk, "report", child, "--outcome", "progress", "--summary", "s", "--ack-cursor", f.Cursor); r.code != 0 {
		t.Fatalf("report with cursor: %+v", r)
	}
	if a, b := w.state(c1), w.state(c2); a != runner.AttachProcessed || b != runner.AttachPending {
		t.Fatalf("c1=%s c2=%s", a, b)
	}
}

// 落四条消息（ACT-FWD-01）：alice 已核实、bob 已核实、alice 未核实、无发送者；白名单只有 alice。返回 alice 已核实这条的 id。
func (w *world) fourMessages(item string) string {
	w.t.Helper()
	var alice string
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		for _, m := range []struct {
			id, sender string
			verified   bool
		}{{"m1", "alice", true}, {"m2", "bob", true}, {"m3", "alice", false}, {"m4", "", false}} {
			ev := domain.SourceEvent{ID: m.id, Type: "pr.comment", Subject: m.id, Detail: "body " + m.id, OccurredAt: time.Now(), Sender: m.sender, SenderVerified: m.verified}
			if err := runner.AppendInternal(tx, src, ev); err != nil {
				return err
			}
			es, err := runner.Events(tx, src.ID)
			if err != nil {
				return err
			}
			id := es[len(es)-1].ID
			if m.id == "m1" {
				alice = id
			}
			if err := runner.SetAttachState(tx, id, runner.AttachPending, item); err != nil {
				return err
			}
		}
		_, err := policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "test", func(p *policy.Policy) error {
			p.ForwardAllowlist = map[string][]string{src.ID: {"alice"}}
			return nil
		})
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return alice
}

func (w *world) markUnattended(item string) {
	w.t.Helper()
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := kitems.Revise(tx, item, "test", time.Now(), func(it *domain.Item) error { it.Unattended = true; return nil })
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) runCredential(target string) string {
	w.t.Helper()
	var secret string
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: target, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, time.Now())
		if err != nil {
			return err
		}
		secret, err = runidentity.Issue(tx, r.ID, time.Now())
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return secret
}

// messages 经路由读挂靠消息，返回 HTTP 状态和消息主题。
func (w *world) messages(token, item string) (int, []allowlist.Attached) {
	w.t.Helper()
	req, _ := http.NewRequest("GET", w.ts.URL+"/v1/items/"+item+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct{ Messages []allowlist.Attached }
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out.Messages
}

func subjects(ms []allowlist.Attached) string {
	var s []string
	for _, m := range ms {
		s = append(s, m.Subject)
	}
	return strings.Join(s, ",")
}

// flow-forward-allowlist / ACT-FWD-01、03、04：路由给全部字段，无人值守链路只给白名单内的，且读取不推进状态。
func TestItemMessagesRouteFiltersUnattendedReaders(t *testing.T) {
	w := newWorld(t)
	wrk, dlg, obs := w.token(auth.Worker, workHost), w.token(auth.Delegate, ""), w.token(auth.Observer, "")
	item := w.delegated(wrk)
	alice := w.fourMessages(item)

	code, ms := w.messages(obs, item)
	if code != 200 || subjects(ms) != "m1,m2,m3,m4" {
		t.Fatalf("attended item: %d %v", code, subjects(ms))
	}
	if !ms[0].ForwardAllowed || ms[1].ForwardAllowed || ms[2].ForwardAllowed || ms[3].ForwardAllowed {
		t.Fatalf("forward_allowed: %+v", ms)
	}
	if m := ms[0]; m.Source != src.ID || m.Sender != "alice" || !m.SenderVerified || m.AttachState != runner.AttachPending || m.Detail != "body m1" {
		t.Fatalf("fields: %+v", m)
	}

	w.markUnattended(item)
	for _, tok := range []string{wrk, obs, w.runCredential(item)} {
		if code, ms := w.messages(tok, item); code != 200 || subjects(ms) != "m1" {
			t.Fatalf("filtered reader: %d %v", code, subjects(ms))
		}
	}
	if _, ms := w.messages(dlg, item); subjects(ms) != "m1,m2,m3,m4" {
		t.Fatalf("delegate: %v", subjects(ms))
	}
	if !allowlist.Filtered(authz.LevelRun, domain.Item{}) || allowlist.Filtered(authz.LevelWeb, domain.Item{ItemKernelState: domain.ItemKernelState{Unattended: true}}) {
		t.Fatal("Filtered: run must filter, Web must not")
	}
	if w.state(alice) != runner.AttachPending {
		t.Fatalf("read advanced state to %s", w.state(alice))
	}

	var ext string
	if err := w.st.Update(bg, func(tx *store.Tx) (err error) {
		_, ext, err = w.au.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "export"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := w.messages(ext, item); code != 403 {
		t.Fatalf("ext: %d", code)
	}
	if code, _ := w.messages(obs, "itm_none"); code != 404 {
		t.Fatalf("unknown item: %d", code)
	}
}

// ACT-FWD-02、03、04：`apa context --scope item` 与 `apa item show` 同样过滤；只有负责者推进状态，
// 回执确认不会把没见过的消息记成已处理，在场主会话的读取也不会。
func TestContextScopeItemAndItemShowFilter(t *testing.T) {
	w := newWorld(t)
	wrk, dlg := w.token(auth.Worker, workHost), w.token(auth.Delegate, "")
	item := w.delegated(wrk)
	alice := w.fourMessages(item)
	w.markUnattended(item)

	type ctxOut struct {
		Items    []struct{ ID string }
		Messages []struct {
			Item, Cursor string
			Messages     []struct{ Subject string }
		}
	}
	read := func(tok string, args ...string) ctxOut {
		t.Helper()
		r := w.apa(tok, append([]string{"context", "--scope", "item"}, args...)...)
		var o ctxOut
		if err := json.Unmarshal([]byte(r.out), &o); err != nil || r.code != 0 {
			t.Fatalf("context: %+v", r)
		}
		return o
	}
	got := func(o ctxOut) string {
		var s []string
		for _, f := range o.Messages {
			for _, m := range f.Messages {
				s = append(s, m.Subject)
			}
		}
		return strings.Join(s, ",")
	}

	// 在场主会话看到全部，但不推进状态，也拿不到确认游标。
	o := read(dlg, "--item", item)
	if got(o) != "m1,m2,m3,m4" || o.Messages[0].Cursor != "" || w.state(alice) != runner.AttachPending {
		t.Fatalf("delegate: %+v state=%s", o, w.state(alice))
	}
	// 运行身份不用指定事项：取目标事项，只有白名单内的，并推进。
	o = read(w.runCredential(item))
	if got(o) != "m1" || len(o.Items) != 1 || o.Items[0].ID != item || o.Messages[0].Cursor == "" || w.state(alice) != runner.AttachForwarded {
		t.Fatalf("run: %+v state=%s", o, w.state(alice))
	}
	// worker 不指定事项：取分配给自己的未关闭事项。
	o = read(wrk)
	if got(o) != "m1" || o.Messages[0].Item != item {
		t.Fatalf("worker: %+v", o)
	}
	r := w.apa(wrk, "context", "--scope", "item", "--format", "inject")
	if !strings.Contains(r.out, "事项 "+item+" 的跟进消息") || !strings.Contains(r.out, "body m1") || strings.Contains(r.out, "body m2") {
		t.Fatalf("inject: %+v", r)
	}
	var bob, noSender string
	if err := w.st.View(bg, func(tx *store.Tx) error {
		as, err := runner.Attachments(tx, item)
		for _, a := range as {
			switch a.Subject {
			case "m2":
				bob = a.ID
			case "m4":
				noSender = a.ID
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := w.apa(wrk, "report", item, "--outcome", "progress", "--summary", "s", "--ack-cursor", o.Messages[0].Cursor); r.code != 0 {
		t.Fatalf("report: %+v", r)
	}
	if w.state(alice) != runner.AttachProcessed || w.state(bob) != runner.AttachPending || w.state(noSender) != runner.AttachPending {
		t.Fatalf("after ack: %s %s %s", w.state(alice), w.state(bob), w.state(noSender))
	}

	// item show 走同一过滤。
	show := func(tok string) string {
		t.Helper()
		r := w.apa(tok, "item", "show", item)
		var o struct{ Messages []allowlist.Attached }
		if err := json.Unmarshal([]byte(r.out), &o); err != nil || r.code != 0 {
			t.Fatalf("show: %+v", r)
		}
		return subjects(o.Messages)
	}
	if s := show(wrk); s != "m1" {
		t.Fatalf("worker show: %s", s)
	}
	if s := show(dlg); s != "m1,m2,m3,m4" {
		t.Fatalf("delegate show: %s", s)
	}
}

// 普通作用域带会话时，新挂靠的消息随增量给出一次；白名单照样过滤。
func TestSessionContextCarriesNewlyAttachedMessages(t *testing.T) {
	w := newWorld(t)
	w.delegated(w.token(auth.Worker, workHost))
	session := func() string {
		t.Helper()
		r := w.apa(w.token(auth.Delegate, ""), "context", "--session", "s1", "--scope", "all", "--project", w.proj)
		if r.code != 0 {
			t.Fatalf("context: %+v", r)
		}
		return r.out
	}
	session() // 读掉委派本身的增量
	id := w.message("m9")
	if out := session(); !strings.Contains(out, "review m9") {
		t.Fatalf("attached message missing: %s", out)
	}
	if out := session(); strings.Contains(out, "review m9") {
		t.Fatalf("message repeated: %s", out)
	}
	if w.state(id) != runner.AttachPending {
		t.Fatalf("main session read advanced state: %s", w.state(id))
	}
}
