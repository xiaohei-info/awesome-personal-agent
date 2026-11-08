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
	apigrants "github.com/xiaohei-info/awesome-personal-agent/api/settings/grants"
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
	gr, err := apigrants.Mount(x, a, st)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), apidecisions.Routes(x, "http://apa.example"), gr}})
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

func (w *world) webOnlyFunds() {
	w.t.Helper()
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "test", func(p *policy.Policy) error {
			p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Tag: "funds", Category: policy.CategoryFunds})
			return nil
		})
		return err
	}); err != nil {
		w.t.Fatal(err)
	}
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

// flow-decision-answer / ACT-DEC-01、03、06：worker 经 apa ask 进 needs_input；delegate 凭本人原话 apa decide 代答，只认第一次（6）。
func TestAskDecideAndFirstAnswerWins(t *testing.T) {
	w := newWorld(t)
	wrk, dlg := w.token(auth.Worker, "wrk1"), w.token(auth.Delegate, "")
	id := w.asked("", wrk)
	it := w.get(id)
	if it.Phase != domain.PhaseNeedsInput || it.Ask.Question != "ship it?" || len(it.Ask.Options) != 2 ||
		it.Ask.Options[1].Next != domain.PhaseDropped || it.Ask.Recommend != "A" {
		t.Fatalf("after ask: %+v", it)
	}
	if r := w.apa(wrk, "ask", id, "--question", "again?"); r.code != 6 {
		t.Fatalf("second ask want 6: %+v", r)
	}
	// 代答没有本人原话：退出码 23，状态不变。
	if r := w.apa(dlg, "decide", id, "--option", "A"); r.code != 23 {
		t.Fatalf("decide without evidence want 23: %+v", r)
	}
	r := w.apa(dlg, "decide", id, "--option", "A", "--evidence", "owner: yes ship it")
	if r.code != 0 || !strings.Contains(r.out, `"id":"`+id+`"`) {
		t.Fatalf("decide: %+v", r)
	}
	got := w.get(id)
	if got.Phase != domain.PhaseOpen || got.Ask.Resolution.Choice != "A" || got.Ask.Resolution.ResolvedVia != "chat" ||
		got.Ask.Resolution.Evidence != "owner: yes ship it" {
		t.Fatalf("after decide: %+v", got.Ask.Resolution)
	}
	r = w.apa(dlg, "decide", id, "--option", "B", "--evidence", "owner: no")
	if r.code != 6 || errOf(t, r).Code != domain.CodeConflict || w.get(id).Ask.Resolution.Choice != "A" {
		t.Fatalf("late answer want 6 and first answer kept: %+v", r)
	}
}

// ACT-DEC-04、05：worker 与观察者代答为 4；自由文本答案记录原文。
func TestDecidePermissionAndFreeText(t *testing.T) {
	w := newWorld(t)
	wrk, dlg, obs := w.token(auth.Worker, "wrk1"), w.token(auth.Delegate, ""), w.token(auth.Observer, "")
	id := w.asked("", wrk)
	for _, tok := range []string{wrk, obs} {
		if r := w.apa(tok, "decide", id, "--option", "A", "--evidence", "x"); r.code != 4 {
			t.Fatalf("want 4: %+v", r)
		}
	}
	free := w.item(`{"kind":"task","title":"f","phase":"open","assignee":"wrk1"}`)
	if r := w.apa(wrk, "ask", free, "--question", "q", "--free-text", "--option", "A=a"); r.code != 0 {
		t.Fatal(r)
	}
	if r := w.apa(dlg, "decide", free, "--text", "another idea", "--evidence", "owner: do X"); r.code != 0 {
		t.Fatal(r)
	}
	if got := w.get(free).Ask.Resolution; got == nil || got.Text != "another idea" {
		t.Fatalf("resolution = %+v", got)
	}
	if r := w.apa(wrk, "ask", "nope", "--question", "q"); r.code != 5 {
		t.Fatalf("missing item want 5: %+v", r)
	}
}

// flow-web-only / ACT-WEB-02、03、04：web_only 类代答与接受返回 20 并附 Web 链接；事项不变；CLI 无绕过选项。
func TestWebOnlyMapsTo20WithLink(t *testing.T) {
	w := newWorld(t)
	w.webOnlyFunds()
	wrk, dlg := w.token(auth.Worker, "wrk1"), w.token(auth.Delegate, "")
	id := w.asked(`,"tags":["funds"]`, wrk)
	r := w.apa(dlg, "decide", id, "--option", "A", "--evidence", "owner: yes")
	e := errOf(t, r)
	if r.code != 20 || e.Code != domain.CodeWebOnly || e.WebURL == nil || *e.WebURL != "http://apa.example/items/"+id {
		t.Fatalf("decide on web_only: %+v", r)
	}
	if strings.Contains(r.errOut, "ship it?") {
		t.Fatal("error body must not carry ledger content")
	}
	if it := w.get(id); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		t.Fatalf("item must stay needs_input: %+v", it)
	}
	// 没有绕过选项：任何 override 类标志都是用法错误。
	for _, f := range []string{"--force", "--web-only-override", "--bypass"} {
		if r := w.apa(dlg, "decide", id, "--option", "A", "--evidence", "x", f); r.code != 2 {
			t.Fatalf("%s must be rejected as usage: %+v", f, r)
		}
	}
	// web_only 提案：接受为 20 带链接。
	p := w.item(`{"kind":"task","title":"p","tags":["funds"]}`)
	r = w.apa(dlg, "accept", p, "--evidence", "owner: take it")
	if e := errOf(t, r); r.code != 20 || e.WebURL == nil || !strings.HasSuffix(*e.WebURL, "/items/"+p) {
		t.Fatalf("accept on web_only: %+v", r)
	}
}

// ACT-WEB-05：初筛未返回时代答为 22 并附 Web 链接。
func TestScreeningPendingMapsTo22(t *testing.T) {
	w := newWorld(t)
	wrk, dlg := w.token(auth.Worker, "wrk1"), w.token(auth.Delegate, "")
	id := w.asked("", wrk)
	if err := w.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := kitems.Revise(tx, id, "test", time.Now(), func(x *domain.Item) error {
			a := *x.Ask
			a.ScreeningPending = true
			x.Ask = &a
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := w.apa(dlg, "decide", id, "--option", "A", "--evidence", "owner: yes")
	if e := errOf(t, r); r.code != 22 || e.Code != domain.CodeRiskScreeningPending || e.WebURL == nil {
		t.Fatalf("want 22 with link: %+v", r)
	}
}

// flow-proposal-triage / ACT-PROP-05、06、07：delegate 凭原话接受或丢弃建议；worker 为 4；已处理再处理为 6。
func TestAcceptDismissProposal(t *testing.T) {
	w := newWorld(t)
	wrk, dlg := w.token(auth.Worker, "wrk1"), w.token(auth.Delegate, "")
	a := w.item(`{"kind":"task","title":"a"}`)
	d := w.item(`{"kind":"task","title":"d"}`)
	if r := w.apa(dlg, "accept", a); r.code != 23 {
		t.Fatalf("accept without evidence want 23: %+v", r)
	}
	if r := w.apa(wrk, "accept", a, "--evidence", "x"); r.code != 4 {
		t.Fatalf("worker accept want 4: %+v", r)
	}
	if r := w.apa(dlg, "accept", a, "--evidence", "owner: take it"); r.code != 0 {
		t.Fatal(r)
	}
	if r := w.apa(dlg, "dismiss", d, "--reason", "not now", "--evidence", "owner: drop it"); r.code != 0 {
		t.Fatal(r)
	}
	if w.get(a).Phase != domain.PhaseOpen || w.get(d).Phase != domain.PhaseDropped {
		t.Fatal("accept opens, dismiss drops")
	}
	if r := w.apa(dlg, "dismiss", a, "--evidence", "owner: drop it"); r.code != 6 {
		t.Fatalf("dismiss after accept want 6: %+v", r)
	}
}

// 参数校验是用法错误 2，且不发请求；kernel 不可达为 7。
func TestUsageErrorsAndUnreachable(t *testing.T) {
	w := newWorld(t)
	dlg := w.token(auth.Delegate, "")
	for _, args := range [][]string{
		{"ask"}, {"decide"}, {"accept", "--evidence", "x"}, {"dismiss", "id", "extra"},
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
	if r := w.apa(dlg, "accept", "i", "--evidence", "x"); r.code != 7 {
		t.Fatalf("want 7: %+v", r)
	}
}

// ask 带 deadline 与 default_on_timeout 时原样交给内核，决策请求落账。
func TestAskWithDeadlineAndDefault(t *testing.T) {
	w := newWorld(t)
	wrk := w.token(auth.Worker, "wrk1")
	id := w.item(`{"kind":"task","title":"t","phase":"open","assignee":"wrk1"}`)
	dl := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	r := w.apa(wrk, "ask", id, "--question", "q", "--option", "A=a", "--default-on-timeout", "A", "--deadline", dl, "--resume-note", "n", "--reason", "why")
	if r.code != 0 {
		t.Fatal(r)
	}
	if a := w.get(id).Ask; a == nil || a.DefaultOnTimeout != "A" || a.Deadline == nil || a.ResumeNote != "n" {
		t.Fatalf("ask = %+v", a)
	}
}

// 授权单（§14.5）：本人在 Web 签发后，delegate 凭据经 apa grant list 只读到自己名下有效的授权单；
// 其他凭据为退出码 4，撤销后不再列出。
func TestGrantListShowsOnlyOwnLiveGrants(t *testing.T) {
	w := newWorld(t)
	var d1, d2 auth.Credential
	var s1 string
	if err := w.st.Update(bg, func(tx *store.Tx) (err error) {
		if d1, s1, err = w.auth.Issue(tx, auth.Spec{Kind: auth.Delegate}); err != nil {
			return
		}
		d2, _, err = w.auth.Issue(tx, auth.Spec{Kind: auth.Delegate})
		return
	}); err != nil {
		t.Fatal(err)
	}
	issue := func(cred string) string {
		exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
		r := w.raw("POST", "/v1/settings/grants", `{"grantee":{"credential":"`+cred+`"},"scope":{"items":["it1"]},"actions":["decision.answer"],"expires_at":"`+exp+`"}`,
			func(r *http.Request) { r.AddCookie(w.cookie); r.Header.Set(core.CSRFHeader, w.csrf) })
		var g struct{ ID string }
		if r.code != 201 || json.Unmarshal(r.body, &g) != nil {
			t.Fatalf("issue: %d %s", r.code, r.body)
		}
		return g.ID
	}
	mine, _ := issue(d1.ID), issue(d2.ID)

	r := w.apa(s1, "grant", "list")
	var out struct{ Grants []policy.Grant }
	if err := json.Unmarshal([]byte(r.out), &out); r.code != 0 || err != nil || len(out.Grants) != 1 || out.Grants[0].ID != mine ||
		out.Grants[0].Actions[0] != "decision.answer" || out.Grants[0].ExpiresAt.IsZero() {
		t.Fatalf("grant list: %+v (%v)", r, err)
	}

	for _, tok := range []string{w.token(auth.Worker, "wrk1"), w.token(auth.Observer, "")} {
		if r := w.apa(tok, "grant", "list"); r.code != domain.ExitPermissionDenied {
			t.Errorf("non-delegate: %+v", r)
		}
	}
	if r := w.apa(s1, "grant"); r.code != domain.ExitUsage || !strings.Contains(errOf(t, r).Next, "usage: apa grant list") {
		t.Errorf("no subcommand: %+v", r)
	}
	if r := w.apa(s1, "grant", "list", "extra"); r.code != domain.ExitUsage {
		t.Errorf("extra arg: %+v", r)
	}

	w.raw("POST", "/v1/settings/grants/"+mine+"/revoke", `{}`, func(r *http.Request) { r.AddCookie(w.cookie); r.Header.Set(core.CSRFHeader, w.csrf) })
	if r := w.apa(s1, "grant", "list"); r.code != 0 || !strings.Contains(r.out, `"grants": []`) && !strings.Contains(r.out, `"grants":[]`) {
		t.Errorf("revoked grant must not be listed: %+v", r)
	}
}
