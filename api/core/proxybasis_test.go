package core_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apidecisions "github.com/xiaohei-info/awesome-personal-agent/api/decisions"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"

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

// 第二档依据经 HTTP 骨干进入命令管线（§14.5、附录 B.2/B.3）：真实账本、真实授权单、真实命令管线，路由里没有判定。

var bg = context.Background()

type pbEnv struct {
	t      *testing.T
	st     *store.Store
	au     *auth.Auth
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
	dlg    string // delegate 凭据密钥
	wrk    string
	dlgP   string // delegate 的 principal，授权单据此绑定
}

func pbSetup(t *testing.T) *pbEnv {
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
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, "correct horse battery") }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), apidecisions.Routes(x, ""), apiwhy.Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &pbEnv{t: t, st: st, au: a, srv: srv}
	rec := e.do("POST", "/v1/auth/login", `{"password":"correct horse battery"}`, nil)
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
	issue := func(s auth.Spec) string {
		var secret string
		if err := st.Update(bg, func(tx *store.Tx) (err error) { _, secret, err = a.Issue(tx, s); return }); err != nil {
			t.Fatal(err)
		}
		return secret
	}
	e.dlg, e.wrk = issue(auth.Spec{Kind: auth.Delegate}), issue(auth.Spec{Kind: auth.Worker, Executor: "wrk1"})
	if err := st.View(bg, func(tx *store.Tx) error {
		c, err := command.Bearer{Auth: a, Secret: e.dlg}.Authenticate(tx)
		e.dlgP = c.Actor.Principal
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pbEnv) do(method, path, body string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	for _, m := range mod {
		if m != nil {
			m(req)
		}
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *pbEnv) web(r *http.Request) {
	r.AddCookie(e.cookie)
	r.Header.Set(core.CSRFHeader, e.csrf)
}

func hdrs(kv ...string) func(*http.Request) {
	return func(r *http.Request) {
		for i := 0; i < len(kv); i += 2 {
			r.Header.Set(kv[i], kv[i+1])
		}
	}
}

// as 带 delegate（或其他）密钥与会话引用。
func as(secret string) func(*http.Request) {
	return hdrs("Authorization", "Bearer "+secret, "X-APA-Session", "s1", "X-APA-Session-Kind", "direct")
}

func (e *pbEnv) newItem(body string) string {
	e.t.Helper()
	rec := e.do("POST", "/v1/items", body, e.web)
	if rec.Code != 201 {
		e.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var w struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &w)
	return w.ID
}

func (e *pbEnv) asked() string {
	e.t.Helper()
	id := e.newItem(`{"kind":"task","title":"t","phase":"open","assignee":"wrk1"}`)
	body := `{"ask":{"question":"ship it?","options":[{"id":"A","label":"yes"},{"id":"B","label":"no"}]}}`
	if rec := e.do("POST", "/v1/items/"+id+"/ask", body, as(e.wrk)); rec.Code != 200 {
		e.t.Fatalf("ask: %d %s", rec.Code, rec.Body)
	}
	return id
}

func (e *pbEnv) grant(mod func(*policy.GrantInput)) string {
	e.t.Helper()
	in := policy.GrantInput{Grantee: policy.Grantee{Credential: e.dlgP}, Scope: policy.Scope{Kinds: []string{"task"}},
		Actions: []string{"decision.answer"}, ExpiresAt: time.Now().Add(time.Hour)}
	if mod != nil {
		mod(&in)
	}
	var g policy.Grant
	owner := domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { g, err = policy.IssueGrant(tx, e.au, owner, time.Now(), in); return }); err != nil {
		e.t.Fatal(err)
	}
	return g.ID
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) domain.Error {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return de
}

func (e *pbEnv) events(obj string) []domain.Event {
	e.t.Helper()
	var out []domain.Event
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, obj)
		for _, en := range es {
			out = append(out, en.Event)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// ACT-AUTH-03、flow-decision-answer：X-APA-Grant 代答成功，Event 记 grant；原话与授权单同时携带是 400（退出码 2）。
func TestGrantHeaderAnswersAndBothBasesAreUsage(t *testing.T) {
	e := pbSetup(t)
	id, gid := e.asked(), ""
	gid = e.grant(nil)

	rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, as(e.dlg), hdrs("X-APA-Grant", gid, "X-APA-Evidence", "owner: yes"))
	if de := errBody(t, rec); rec.Code != 400 || de.ExitCode() != 2 {
		t.Fatalf("both bases = %d %s", rec.Code, rec.Body)
	}
	if it := e.getItem(id); it.Phase != domain.PhaseNeedsInput {
		t.Fatalf("rejected usage must not write: %+v", it)
	}

	rec = e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, as(e.dlg), hdrs("X-APA-Grant", gid))
	if rec.Code != 200 {
		t.Fatalf("grant answer = %d %s", rec.Code, rec.Body)
	}
	seen := false
	for _, ev := range e.events(id) {
		seen = seen || (ev.Type == domain.EvDecisionResolved && ev.Grant == gid && ev.ResolvedVia == domain.ResolvedViaGrant)
	}
	if !seen {
		t.Fatal("decision.resolved must record the grant")
	}

	// 审计读接口按授权单 id 查记录：只返回带该授权单的事件（签发与使用），不含其他事件。
	var got struct {
		Events []struct{ Type, Grant, Object string }
	}
	rec = e.do("GET", "/v1/audit/events?grant="+gid, "", e.web)
	if rec.Code != 200 {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Events) != 2 || got.Events[0].Type != domain.EvGrantIssued || got.Events[1].Type != domain.EvDecisionResolved ||
		got.Events[1].Grant != gid || got.Events[1].Object != id {
		t.Fatalf("grant usage = %s", rec.Body)
	}
	other := e.do("GET", "/v1/audit/events?grant=grt_nope", "", e.web)
	if !strings.Contains(other.Body.String(), `"events":[]`) && !strings.Contains(other.Body.String(), `"events":null`) {
		t.Fatalf("unknown grant must have no usage: %s", other.Body)
	}
}

func (e *pbEnv) getItem(id string) domain.Item {
	e.t.Helper()
	var g struct{ Item domain.Item }
	_ = json.Unmarshal(e.do("GET", "/v1/items/"+id, "", e.web).Body.Bytes(), &g)
	return g.Item
}

// 依据缺失的错误体（B.3）：退出码 23，why 只写机制，web_url 指向对象的 Web 页；任何路由都一样，不只是决策路由。
func TestMissingBasisErrorCarriesWebLinkOnEveryRoute(t *testing.T) {
	e := pbSetup(t)
	id := e.newItem(`{"kind":"task","title":"secret-title","phase":"open","assignee":"someone"}`)
	rec := e.do("PATCH", "/v1/items/"+id, `{"title":"x"}`, as(e.dlg))
	de := errBody(t, rec)
	if rec.Code != 403 || de.ExitCode() != 23 || de.WebURL == nil || !strings.HasSuffix(*de.WebURL, "/items/"+id) || de.Why == "" || de.Next == "" {
		t.Fatalf("missing basis = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "secret-title") {
		t.Fatal("error body must not carry ledger content")
	}
	// 授权单无效同样是 23 且带链接（不在 API 层判定，由内核给原因）。
	rec = e.do("PATCH", "/v1/items/"+id, `{"title":"x"}`, as(e.dlg), hdrs("X-APA-Grant", "grt_missing"))
	if de := errBody(t, rec); de.ExitCode() != 23 || de.WebURL == nil {
		t.Fatalf("bad grant = %d %s", rec.Code, rec.Body)
	}
}

type rejection struct {
	Why, Code, Grant string
	Actor            domain.Actor
}

// flow-decision-answer、flow-proposal-triage：已是 needs_input 或 proposed 的事项被拒绝代答/处理后阶段不变，
// 决策待办与建议流的读接口标明最近一次被拒绝的操作；本人处理后不再显示。
func TestRejectedProxyShowsInPendingAndProposalReads(t *testing.T) {
	e := pbSetup(t)
	id := e.asked()
	prop := e.newItem(`{"kind":"task","title":"p"}`)
	if e.getItem(prop).Phase != domain.PhaseProposed {
		t.Fatal("setup: proposal")
	}
	badGrant := e.grant(func(in *policy.GrantInput) { in.Actions = []string{"item.update"} }) // 动作不覆盖

	if rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, as(e.dlg), hdrs("X-APA-Grant", badGrant)); rec.Code != 403 {
		t.Fatalf("decide = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+prop+"/dismiss", `{"reason":"no"}`, as(e.dlg)); rec.Code != 403 {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body)
	}
	if e.getItem(id).Phase != domain.PhaseNeedsInput || e.getItem(prop).Phase != domain.PhaseProposed {
		t.Fatal("phases must not change")
	}

	list := func(phase string) map[string]rejection {
		var out struct {
			Rejections map[string]rejection `json:"last_rejections"`
		}
		rec := e.do("GET", "/v1/items?phase="+phase, "", e.web)
		if rec.Code != 200 {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Rejections
	}
	r := list("needs_input")[id]
	if r.Why == "" || r.Code != string(domain.CodeBasisRequired) || r.Grant != badGrant || r.Actor.Principal != e.dlgP {
		t.Fatalf("pending rejection = %+v", r)
	}
	if r := list("proposed")[prop]; r.Why == "" || r.Code != string(domain.CodeBasisRequired) {
		t.Fatalf("proposal rejection = %+v", r)
	}
	var one struct {
		Rejection *rejection `json:"last_rejection"`
	}
	_ = json.Unmarshal(e.do("GET", "/v1/items/"+id, "", e.web).Body.Bytes(), &one)
	if one.Rejection == nil || one.Rejection.Grant != badGrant {
		t.Fatalf("item read = %+v", one.Rejection)
	}

	// 本人在 Web 回答后，该拒绝不再挂在事项上。
	if rec := e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, e.web); rec.Code != 200 {
		t.Fatalf("owner decide = %d %s", rec.Code, rec.Body)
	}
	if got := list("open")[id]; got.Why != "" {
		t.Fatalf("stale rejection: %+v", got)
	}
}

// delegate 第一档写入（提 proposed 建议）不带 on_behalf_of（§14.5）。
func TestTierOneWriteHasNoOnBehalfOf(t *testing.T) {
	e := pbSetup(t)
	rec := e.do("POST", "/v1/items", `{"kind":"task","title":"idea"}`, as(e.dlg))
	if rec.Code != 201 {
		t.Fatalf("propose = %d %s", rec.Code, rec.Body)
	}
	var w struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &w)
	evs := e.events(w.ID)
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	for _, ev := range evs {
		if ev.Actor.OnBehalfOf != "" {
			t.Fatalf("tier-one event must not claim on_behalf_of: %+v", ev.Actor)
		}
	}
}
