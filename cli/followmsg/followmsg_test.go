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

	apidel "github.com/xiaohei-info/awesome-personal-agent/api/delegation"
	apifm "github.com/xiaohei-info/awesome-personal-agent/api/followmsg"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/delegation"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kdel "github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
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
		kitems.Module, executors.Module, kdel.Module, runner.Module, core.Module} {
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
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apidel.Routes(x), apifm.Routes(x)}})
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
	code := client.Main(args, client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: w.t.TempDir(),
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
	if ok, _, err := w.fm.Intercept(bg, got); err != nil || !ok {
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
