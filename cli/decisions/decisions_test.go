package decisions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apidecisions "github.com/xiaohei-info/awesome-personal-agent/api/decisions"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kdec "github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

var bg = context.Background()

// world 是真实内核（账本、命令管线、决策与事项路由）经 HTTP 对外，CLI 以各级凭据调用它。
type world struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	ts     *httptest.Server
	cookie *http.Cookie
	csrf   string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, kitems.Module, core.Module} {
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
	if err := kitems.Register(x, kitems.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := kdec.Register(x); err != nil {
		t.Fatal(err)
	}
	a := auth.New(nil)
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), apidecisions.Routes(x, "http://apa.example")}})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, st: st, auth: a, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(w.ts.Close)
	resp := w.raw("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(resp.body, &out)
	for _, c := range resp.cookies {
		if c.Name == core.SessionCookie {
			w.cookie = c
		}
	}
	w.csrf = out.CSRF
	return w
}

type rawResp struct {
	code    int
	body    []byte
	cookies []*http.Cookie
}

func (w *world) raw(method, path, body string, mod func(*http.Request)) rawResp {
	w.t.Helper()
	req, _ := http.NewRequest(method, w.ts.URL+path, strings.NewReader(body))
	if mod != nil {
		mod(req)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer r.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(r.Body)
	return rawResp{r.StatusCode, b.Bytes(), r.Cookies()}
}

func (w *world) item(body string) string {
	w.t.Helper()
	r := w.raw("POST", "/v1/items", body, func(r *http.Request) {
		r.AddCookie(w.cookie)
		r.Header.Set(core.CSRFHeader, w.csrf)
	})
	var o struct{ ID string }
	if r.code != 201 || json.Unmarshal(r.body, &o) != nil {
		w.t.Fatalf("create item: %d %s", r.code, r.body)
	}
	return o.ID
}

func (w *world) get(id string) domain.Item {
	w.t.Helper()
	r := w.raw("GET", "/v1/items/"+id, "", func(r *http.Request) { r.AddCookie(w.cookie) })
	var g struct{ Item domain.Item }
	_ = json.Unmarshal(r.body, &g)
	return g.Item
}

func (w *world) token(kind auth.Kind, executor string) string {
	w.t.Helper()
	var secret string
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := w.auth.Issue(tx, auth.Spec{Kind: kind, Executor: executor})
		secret = s
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
	return secret
}

// agent 签一把归属 Agent id 的 delegate 钥匙，并由本人把该 Agent 的档位设为 tier。
func (w *world) agent(id, tier string) string {
	w.t.Helper()
	var secret string
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := w.auth.Issue(tx, auth.Spec{Kind: auth.Delegate, AgentID: id, Harness: "h"})
		if err != nil {
			return err
		}
		secret = s
		_, err = policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "test tier", func(p *policy.Policy) error {
			p.AgentTier.ByAgent[id] = policy.AgentAllowance{Tier: tier}
			return nil
		})
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
	vars := map[string]string{"APA_URL": w.ts.URL, "APA_TOKEN": token, "APA_SESSION": "s1", "APA_HOME": w.t.TempDir()}
	code := client.Main(args, client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: w.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return result{code, out.String(), errb.String()}
}

func errOf(t *testing.T, r result) domain.Error {
	t.Helper()
	var e domain.Error
	if err := json.Unmarshal([]byte(r.errOut), &e); err != nil {
		t.Fatalf("stderr is not an error body: %q", r.errOut)
	}
	return e
}

func (w *world) asked(extra, wrk string) string {
	w.t.Helper()
	id := w.item(`{"kind":"task","title":"t","phase":"open","assignee":"wrk1"` + extra + `}`)
	r := w.apa(wrk, "ask", id, "--question", "ship it?", "--option", "A=yes", "--option", "B=no", "--next", "B=dropped", "--recommend", "A")
	if r.code != 0 {
		w.t.Fatalf("ask: %+v", r)
	}
	return id
}

// flow-decision-answer / ACT-DEC-01、03、06：worker 经 apa ask 进 needs_input；回答是二档：只整理的 Agent 为 23，
// 放开拍板的 Agent apa decide 自答生效并记 agent，只认第一次（6）。
func TestAskDecideAndFirstAnswerWins(t *testing.T) {
	w := newWorld(t)
	wrk, closed, dlg := w.token(auth.Worker, "wrk1"), w.token(auth.Delegate, ""), w.agent("agent_a", policy.AllowDecide)
	id := w.asked("", wrk)
	it := w.get(id)
	if it.Phase != domain.PhaseNeedsInput || it.Ask.Question != "ship it?" || len(it.Ask.Options) != 2 ||
		it.Ask.Options[1].Next != domain.PhaseDropped || it.Ask.Recommend != "A" {
		t.Fatalf("after ask: %+v", it)
	}
	if r := w.apa(wrk, "ask", id, "--question", "again?"); r.code != 6 {
		t.Fatalf("second ask want 6: %+v", r)
	}
	// 档位未放开：退出码 23，状态不变；旧的 --evidence 不再是依据。
	if r := w.apa(closed, "decide", id, "--option", "A", "--evidence", "owner: yes"); r.code != 23 || errOf(t, r).Code != domain.CodeTierClosed {
		t.Fatalf("decide with tier closed want 23: %+v", r)
	}
	if w.get(id).Ask.Resolution != nil {
		t.Fatal("a refused answer must not resolve")
	}
	r := w.apa(dlg, "decide", id, "--option", "A")
	if r.code != 0 || !strings.Contains(r.out, `"id":"`+id+`"`) {
		t.Fatalf("decide: %+v", r)
	}
	got := w.get(id)
	if got.Phase != domain.PhaseOpen || got.Ask.Resolution.Choice != "A" || got.Ask.Resolution.ResolvedVia != kdec.ViaAgent {
		t.Fatalf("after decide: %+v", got.Ask.Resolution)
	}
	r = w.apa(dlg, "decide", id, "--option", "B")
	if r.code != 6 || errOf(t, r).Code != domain.CodeConflict || w.get(id).Ask.Resolution.Choice != "A" {
		t.Fatalf("late answer want 6 and first answer kept: %+v", r)
	}
}

// ACT-DEC-04、05：观察者回答在钥匙范围外（4）；worker 回答自己事项上的问题在范围内，但档位未放开（23）；自由文本答案记录原文。
func TestDecidePermissionAndFreeText(t *testing.T) {
	w := newWorld(t)
	wrk, dlg, obs := w.token(auth.Worker, "wrk1"), w.agent("agent_a", policy.AllowDecide), w.token(auth.Observer, "")
	id := w.asked("", wrk)
	if r := w.apa(obs, "decide", id, "--option", "A"); r.code != 4 {
		t.Fatalf("observer want 4: %+v", r)
	}
	if r := w.apa(wrk, "decide", id, "--option", "A"); r.code != 23 {
		t.Fatalf("worker on its own item want 23: %+v", r)
	}
	free := w.item(`{"kind":"task","title":"f","phase":"open","assignee":"wrk1"}`)
	if r := w.apa(wrk, "ask", free, "--question", "q", "--free-text", "--option", "A=a"); r.code != 0 {
		t.Fatal(r)
	}
	if r := w.apa(dlg, "decide", free, "--text", "another idea"); r.code != 0 {
		t.Fatal(r)
	}
	if got := w.get(free).Ask.Resolution; got == nil || got.Text != "another idea" {
		t.Fatalf("resolution = %+v", got)
	}
	if r := w.apa(wrk, "ask", "nope", "--question", "q"); r.code != 5 {
		t.Fatalf("missing item want 5: %+v", r)
	}
}

// flow-proposal-triage / ACT-PROP-05、06、07：接受与丢弃建议是二档：只整理的 Agent 为 23，worker 为 4；
// 放开拍板的 Agent 直接生效；已处理再处理为 6。
func TestAcceptDismissProposal(t *testing.T) {
	w := newWorld(t)
	wrk, closed, dlg := w.token(auth.Worker, "wrk1"), w.token(auth.Delegate, ""), w.agent("agent_a", policy.AllowDecide)
	a := w.item(`{"kind":"task","title":"a"}`)
	d := w.item(`{"kind":"task","title":"d"}`)
	if r := w.apa(closed, "accept", a); r.code != 23 {
		t.Fatalf("accept with tier closed want 23: %+v", r)
	}
	if r := w.apa(wrk, "accept", a); r.code != 4 {
		t.Fatalf("worker accept want 4: %+v", r)
	}
	if r := w.apa(dlg, "accept", a); r.code != 0 {
		t.Fatal(r)
	}
	if r := w.apa(dlg, "dismiss", d, "--reason", "not now"); r.code != 0 {
		t.Fatal(r)
	}
	if w.get(a).Phase != domain.PhaseOpen || w.get(d).Phase != domain.PhaseDropped {
		t.Fatal("accept opens, dismiss drops")
	}
	if r := w.apa(dlg, "dismiss", a); r.code != 6 {
		t.Fatalf("dismiss after accept want 6: %+v", r)
	}
}

// 参数校验是用法错误 2，且不发请求；kernel 不可达为 7。
func TestUsageErrorsAndUnreachable(t *testing.T) {
	w := newWorld(t)
	dlg := w.token(auth.Delegate, "")
	for _, args := range [][]string{
		{"ask"}, {"decide"}, {"accept"}, {"dismiss", "id", "extra"},
		{"ask", "i", "--question", "q", "--option", "noequals"},
		{"ask", "i", "--question", "q", "--next", "A"},
		{"ask", "i", "--question", "q", "--next", "A=dropped"},
		{"ask", "i", "--question", "q", "--deadline", "tomorrow"},
		{"decide", "i", "--bogus"},
	} {
		if r := w.apa(dlg, args...); r.code != 2 {
			t.Errorf("%v want 2: %+v", args, r)
		}
	}
	w.ts.Close()
	if r := w.apa(dlg, "accept", "i"); r.code != 7 {
		t.Fatalf("want 7: %+v", r)
	}
}
