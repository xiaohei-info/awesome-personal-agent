package decisions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
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

const (
	ownerPW = "correct horse battery"
	webBase = "http://apa.example"
	askBody = `{"ask":{"question":"ship it?","options":[{"id":"A","label":"yes"},{"id":"B","label":"no","next":"dropped"}]}}`
)

var bg = context.Background()

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

// setup 装配真实账本、命令执行器、事项与决策命令和 HTTP 服务，并以本人口令登录一个 Web 会话。
func setup(t *testing.T) *env {
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
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), Routes(x, webBase)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, auth: a, srv: srv}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	e.csrf = out.CSRF
	return e
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000" // 回环来源，明文登录才被接受
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(r *http.Request) {
	r.AddCookie(e.cookie)
	r.Header.Set(core.CSRFHeader, e.csrf)
}

func hdr(kv ...string) func(*http.Request) {
	return func(r *http.Request) {
		for i := 0; i < len(kv); i += 2 {
			r.Header.Set(kv[i], kv[i+1])
		}
	}
}

func both(fs ...func(*http.Request)) func(*http.Request) {
	return func(r *http.Request) {
		for _, f := range fs {
			f(r)
		}
	}
}

func (e *env) token(kind auth.Kind, executor string) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Executor: executor})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return hdr("Authorization", "Bearer "+secret, "X-APA-Session", "s1", "X-APA-Session-Kind", "direct")
}

// item 以本人身份创建一个事项并返回 id。
func (e *env) item(body string) string {
	e.t.Helper()
	rec := e.do("POST", "/v1/items", body, e.web)
	if rec.Code != 201 {
		e.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var w struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &w)
	return w.ID
}

func (e *env) get(id string) domain.Item {
	e.t.Helper()
	var g struct{ Item domain.Item }
	_ = json.Unmarshal(e.do("GET", "/v1/items/"+id, "", e.web).Body.Bytes(), &g)
	return g.Item
}

// asked 建一个指派给 wrk1 的事项并由其 worker 凭据提出决策请求。
func (e *env) asked(extraItem string, wrk func(*http.Request)) string {
	e.t.Helper()
	id := e.item(`{"kind":"task","title":"t","phase":"open","assignee":"wrk1"` + extraItem + `}`)
	if rec := e.do("POST", "/v1/items/"+id+"/ask", askBody, wrk); rec.Code != 200 {
		e.t.Fatalf("ask: %d %s", rec.Code, rec.Body)
	}
	return id
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) domain.Error {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return de
}

// flow-decision-answer：worker 提问进 needs_input；delegate 凭本人原话代答生效并记 chat；只认第一次回答（409）。
func TestAskAndDecide(t *testing.T) {
	e := setup(t)
	wrk, dlg := e.token(auth.Worker, "wrk1"), e.token(auth.Delegate, "")
	id := e.asked("", wrk)
	if it := e.get(id); it.Phase != domain.PhaseNeedsInput || it.Ask == nil || it.Ask.Question != "ship it?" {
		t.Fatalf("after ask: %+v", it)
	}

	// 代答没有本人原话：403 basis_required（退出码 23）。
	if rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, dlg); rec.Code != 403 {
		t.Fatalf("decide without evidence = %d %s", rec.Code, rec.Body)
	}
	rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A","evidence":"owner: yes ship it"}`, dlg)
	if rec.Code != 200 {
		t.Fatalf("decide = %d %s", rec.Code, rec.Body)
	}
	it := e.get(id)
	if it.Phase != domain.PhaseOpen || it.Ask.Resolution == nil || it.Ask.Resolution.Choice != "A" ||
		it.Ask.Resolution.ResolvedVia != "chat" || it.Ask.Resolution.Evidence != "owner: yes ship it" {
		t.Fatalf("after decide: %+v", it.Ask.Resolution)
	}

	// 后到的回答（含 Web 本人）返回 6，已生效的结果不变。
	rec = e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"B"}`, e.web)
	if rec.Code != 409 || errBody(t, rec).Code != domain.CodeConflict {
		t.Fatalf("second answer = %d %s", rec.Code, rec.Body)
	}
	if e.get(id).Ask.Resolution.Choice != "A" {
		t.Fatal("first answer must stand")
	}
}

// Web 本人直接回答记 web，证据可走请求头；Idempotency-Key 重放不重复写入。
func TestDecideOnWebWithHeadersAndIdempotency(t *testing.T) {
	e := setup(t)
	wrk := e.token(auth.Worker, "wrk1")
	id := e.asked("", wrk)
	mod := both(e.web, hdr("Idempotency-Key", "k1", "If-Match", `"2"`))
	rec := e.do("POST", "/v1/items/"+id+"/decide", `{"text":"x"}`, mod) // 此决策不收自由文本
	if rec.Code != 400 {
		t.Fatalf("free text on option-only ask = %d %s", rec.Code, rec.Body)
	}
	first := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"B"}`, mod)
	again := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"B"}`, mod)
	var f, a struct {
		ID       string
		Version  int64
		Replayed bool
	}
	_ = json.Unmarshal(first.Body.Bytes(), &f)
	_ = json.Unmarshal(again.Body.Bytes(), &a)
	if first.Code != 200 || f.ID != id || f.Replayed || again.Code != 200 || !a.Replayed || a.Version != f.Version {
		t.Fatalf("first = %d %s, replay = %d %s", first.Code, first.Body, again.Code, again.Body)
	}
	it := e.get(id)
	if it.Phase != domain.PhaseDropped || it.Ask.Resolution.ResolvedVia != "web" {
		t.Fatalf("option B drops the item via web: %+v", it)
	}
	// 过期的 If-Match 返回 409。
	id2 := e.asked("", wrk)
	if rec := e.do("POST", "/v1/items/"+id2+"/decide", `{"choice":"A"}`, both(e.web, hdr("If-Match", "1"))); rec.Code != 409 {
		t.Fatalf("stale If-Match = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+id2+"/decide", `{"choice":"A"}`, both(e.web, hdr("If-Match", "abc"))); rec.Code != 400 {
		t.Fatalf("bad If-Match = %d %s", rec.Code, rec.Body)
	}
}

// flow-web-only：web_only 类代答返回 20 并带 Web 链接，事项停在 needs_input；本人在 Web 仍可回答。
func TestWebOnlyRejectsDelegateWithLink(t *testing.T) {
	e := setup(t)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "test", func(p *policy.Policy) error {
			p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Tag: "funds", Category: policy.CategoryFunds})
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wrk, dlg := e.token(auth.Worker, "wrk1"), e.token(auth.Delegate, "")
	id := e.asked(`,"tags":["funds"]`, wrk)

	rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, both(dlg, hdr("X-APA-Evidence", "owner: yes")))
	de := errBody(t, rec)
	if rec.Code != 403 || de.Code != domain.CodeWebOnly || de.ExitCode() != 20 || de.WebURL == nil || *de.WebURL != webBase+"/items/"+id {
		t.Fatalf("web_only = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "ship it?") {
		t.Fatal("error body must not carry ledger content")
	}
	if it := e.get(id); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		t.Fatalf("item must stay needs_input: %+v", it)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, e.web); rec.Code != 200 {
		t.Fatalf("owner on Web = %d %s", rec.Code, rec.Body)
	}
}

// 初筛未返回：代答返回 22 与 Web 链接；运行级以下的凭据（观察者）仍是 4。
func TestScreeningPendingAndPermission(t *testing.T) {
	e := setup(t)
	wrk, dlg, obs := e.token(auth.Worker, "wrk1"), e.token(auth.Delegate, ""), e.token(auth.Observer, "")
	id := e.asked("", wrk)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := kitems.Revise(tx, id, "test", time.Now(), func(x *domain.Item) error {
			a := *x.Ask
			a.ScreeningPending = true // 初筛模块的状态；本路由只读
			x.Ask = &a
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A","evidence":"owner: yes"}`, dlg)
	de := errBody(t, rec)
	if rec.Code != 403 || de.Code != domain.CodeRiskScreeningPending || de.WebURL == nil || *de.WebURL != webBase+"/items/"+id {
		t.Fatalf("screening pending = %d %s", rec.Code, rec.Body)
	}
	rec = e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A","evidence":"x"}`, obs)
	if rec.Code != 403 || errBody(t, rec).Code != domain.CodePermissionDenied {
		t.Fatalf("observer = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, e.web); rec.Code != 200 {
		t.Fatalf("owner on Web = %d %s", rec.Code, rec.Body)
	}
}

// ask 的授权与状态：观察者不能提问（4）；已有未决请求不能再问（6）；他人的事项 worker 不能问。
func TestAskAuthorizationAndState(t *testing.T) {
	e := setup(t)
	wrk, other, obs := e.token(auth.Worker, "wrk1"), e.token(auth.Worker, "wrk2"), e.token(auth.Observer, "")
	id := e.item(`{"kind":"task","title":"t","phase":"open","assignee":"wrk1"}`)
	if rec := e.do("POST", "/v1/items/"+id+"/ask", askBody, obs); rec.Code != 403 {
		t.Fatalf("observer ask = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/ask", askBody, other); rec.Code != 403 {
		t.Fatalf("other worker ask = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/ask", `{"ask":{"question":""}}`, wrk); rec.Code != 400 {
		t.Fatalf("empty question = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/ask", askBody, wrk); rec.Code != 200 {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/ask", askBody, wrk); rec.Code != 409 {
		t.Fatalf("second ask = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/nope/ask", askBody, wrk); rec.Code != 404 {
		t.Fatalf("missing item = %d %s", rec.Code, rec.Body)
	}
}

// flow-proposal-triage：delegate 凭原话接受或丢弃建议；worker 为 4；已处理的建议再处理为 6。
func TestAcceptAndDismissProposal(t *testing.T) {
	e := setup(t)
	wrk, dlg := e.token(auth.Worker, "wrk1"), e.token(auth.Delegate, "")
	a := e.item(`{"kind":"task","title":"a"}`)
	d := e.item(`{"kind":"task","title":"d"}`)
	if e.get(a).Phase != domain.PhaseProposed {
		t.Fatal("setup: items start as proposals")
	}
	if rec := e.do("POST", "/v1/items/"+a+"/accept", `{}`, dlg); rec.Code != 403 {
		t.Fatalf("accept without evidence = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+a+"/accept", `{"evidence":"x"}`, wrk); rec.Code != 403 {
		t.Fatalf("worker accept = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+a+"/accept", `{}`, both(dlg, hdr("X-APA-Evidence", "owner: take it"))); rec.Code != 200 {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+d+"/dismiss", `{"reason":"not now"}`, e.web); rec.Code != 200 {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body)
	}
	if e.get(a).Phase != domain.PhaseOpen || e.get(d).Phase != domain.PhaseDropped {
		t.Fatal("accept opens, dismiss drops")
	}
	if rec := e.do("POST", "/v1/items/"+a+"/dismiss", `{}`, e.web); rec.Code != 409 {
		t.Fatalf("dismiss after accept = %d %s", rec.Code, rec.Body)
	}
}

// 输入严格性：URL 提供 id；体必须是单个 JSON 对象；未知字段与非字符串证据都是 400，且不改动事项。
func TestStrictInput(t *testing.T) {
	e := setup(t)
	wrk := e.token(auth.Worker, "wrk1")
	id := e.asked("", wrk)
	for _, body := range []string{`{"choice":"A","id":"other"}`, `{"choice":"A","ID":"other"}`, `null`, `[]`,
		`{"choice":"A"} {}`, `{"choice":"A","bogus":1}`, `{"choice":"A","evidence":5}`, `{"choice":"A","choice":"B","text":"x"}`} {
		if rec := e.do("POST", "/v1/items/"+id+"/decide", body, e.web); rec.Code != 400 {
			t.Errorf("%s = %d %s, want 400", body, rec.Code, rec.Body)
		}
	}
	if it := e.get(id); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		t.Fatalf("rejected writes changed state: %+v", it)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, nil); rec.Code != 401 {
		t.Fatalf("anonymous = %d", rec.Code)
	}
}

// webBase 为空时链接取请求的 Host。
func TestWebURLFallsBackToHost(t *testing.T) {
	h := &handlers{}
	r := httptest.NewRequest("POST", "/v1/items/itm_1/decide", nil)
	r.SetPathValue("id", "itm_1")
	r.Host = "127.0.0.1:7777"
	if got := h.webURL(r); got != "http://127.0.0.1:7777/items/itm_1" {
		t.Fatalf("got %q", got)
	}
}

// ACT-DEC-05、ACT-DEC-07、ACT-WEB-03、ACT-WEB-05、ACT-PROP-07 的补充路径。
func TestWorkerCannotAnswerFreeTextAndWebOnlyExtras(t *testing.T) {
	e := setup(t)
	wrk, obs, dlg := e.token(auth.Worker, "wrk1"), e.token(auth.Observer, ""), e.token(auth.Delegate, "")
	id := e.asked("", wrk)
	if rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A","evidence":"x"}`, wrk); rec.Code != 403 || errBody(t, rec).Code != domain.CodePermissionDenied {
		t.Fatalf("worker decide = %d %s", rec.Code, rec.Body)
	}

	// 自由文本：本人在 Web 填写文本，resolution 记录文本。
	free := e.item(`{"kind":"task","title":"f","phase":"open","assignee":"wrk1"}`)
	ask := `{"ask":{"question":"q","free_text":true,"options":[{"id":"A","label":"a"}]}}`
	if rec := e.do("POST", "/v1/items/"+free+"/ask", ask, wrk); rec.Code != 200 {
		t.Fatalf("ask free_text = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+free+"/decide", `{"text":"another idea"}`, e.web); rec.Code != 200 {
		t.Fatalf("free text = %d %s", rec.Code, rec.Body)
	}
	if got := e.get(free).Ask.Resolution; got == nil || got.Text != "another idea" {
		t.Fatalf("resolution = %+v", got)
	}

	// web_only 建议：delegate 接受返回 20 并带链接；web_only 决策不允许超时默认值。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "test", func(p *policy.Policy) error {
			p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Tag: "funds", Category: policy.CategoryFunds})
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	prop := e.item(`{"kind":"task","title":"p","tags":["funds"]}`)
	rec := e.do("POST", "/v1/items/"+prop+"/accept", `{"evidence":"owner: ok"}`, dlg)
	if de := errBody(t, rec); rec.Code != 403 || de.Code != domain.CodeWebOnly || de.WebURL == nil || *de.WebURL != webBase+"/items/"+prop {
		t.Fatalf("web_only accept = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+prop+"/accept", `{}`, obs); rec.Code != 403 || errBody(t, rec).Code != domain.CodePermissionDenied {
		t.Fatalf("observer accept = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+prop+"/accept", `{}`, e.web); rec.Code != 200 {
		t.Fatalf("owner accept = %d %s", rec.Code, rec.Body)
	}
	fund := e.item(`{"kind":"task","title":"m","phase":"open","assignee":"wrk1","tags":["funds"]}`)
	withDefault := `{"ask":{"question":"q","options":[{"id":"A","label":"a"}],"default_on_timeout":"A","deadline":"2099-01-01T00:00:00Z"}}`
	if rec := e.do("POST", "/v1/items/"+fund+"/ask", withDefault, wrk); rec.Code != 400 {
		t.Fatalf("web_only default_on_timeout = %d %s", rec.Code, rec.Body)
	}
}
