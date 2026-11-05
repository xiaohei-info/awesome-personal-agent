package why

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
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

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

// setup 装配真实账本、命令执行器、事项路由和回放路由，并以本人口令登录一个 Web 会话。
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
	a := auth.New(nil)
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), Routes(st)}})
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
	req.RemoteAddr = "127.0.0.1:5000"
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

func (e *env) token(kind auth.Kind) func(*http.Request) {
	e.t.Helper()
	scope := ""
	if kind == auth.Ext || kind == auth.Webhook {
		scope = "x"
	}
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

// createItem 经 Web 创建一个带原因的事项，事件正文含可识别的标记。
func (e *env) createItem(title string) string {
	e.t.Helper()
	rec := e.do("POST", "/v1/items", `{"kind":"task","title":"`+title+`","reason":"SECRET-REASON"}`, e.web)
	if rec.Code != 201 {
		e.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var w struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &w)
	return w.ID
}

type steps struct {
	ID               string `json:"id"`
	ScreeningPending bool   `json:"screening_pending"`
	Steps            []struct {
		Seq       int64
		Type      string
		Reason    string
		Payload   json.RawMessage
		Redacted  bool
		Object    string
		Actor     domain.Actor
		Tombstone bool
	}
	Events []struct {
		Seq      int64
		Type     string
		Object   string
		Actor    domain.Actor
		Reason   string
		Redacted bool
	}
}

func (e *env) get(path string, mod func(*http.Request)) (steps, *httptest.ResponseRecorder) {
	e.t.Helper()
	rec := e.do("GET", path, "", mod)
	var s steps
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	return s, rec
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return de.Code
}

// 回放：本人看到完整正文，observer 看到信封与授权依据但正文被裁剪，且回放是同一份时间线。
func TestWhyTrimsByCredential(t *testing.T) {
	e := setup(t)
	id := e.createItem("fix login")

	full, rec := e.get("/v1/why/"+id, e.web)
	if rec.Code != 200 || full.ID != id || len(full.Steps) == 0 {
		t.Fatalf("owner why = %d %s", rec.Code, rec.Body)
	}
	if full.Steps[0].Type != domain.EvItemCreated || full.Steps[0].Reason != "SECRET-REASON" || !strings.Contains(string(full.Steps[0].Payload), "fix login") || full.Steps[0].Redacted {
		t.Fatalf("owner step = %+v", full.Steps[0])
	}

	obs, rec := e.get("/v1/why/"+id, e.token(auth.Observer))
	if rec.Code != 200 || len(obs.Steps) != len(full.Steps) {
		t.Fatalf("observer why = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "SECRET-REASON") || strings.Contains(rec.Body.String(), "fix login") {
		t.Fatalf("observer body leaks content: %s", rec.Body)
	}
	if obs.Steps[0].Type != domain.EvItemCreated || obs.Steps[0].Actor.Principal == "" || !obs.Steps[0].Redacted {
		t.Fatalf("observer step = %+v", obs.Steps[0])
	}

	wrk, rec := e.get("/v1/why/"+id, e.token(auth.Worker))
	if rec.Code != 200 || wrk.Steps[0].Reason != "SECRET-REASON" {
		t.Fatalf("worker keeps reason: %d %s", rec.Code, rec.Body)
	}
}

// 回放错误语义：未登录 401、未知 id 404、ext/webhook 凭据 403。
func TestWhyErrors(t *testing.T) {
	e := setup(t)
	if _, rec := e.get("/v1/why/item_x", nil); rec.Code != 401 || codeOf(t, rec) != domain.CodeUnauthenticated {
		t.Fatalf("anonymous = %d %s", rec.Code, rec.Body)
	}
	if _, rec := e.get("/v1/why/item_missing", e.web); rec.Code != 404 || codeOf(t, rec) != domain.CodeNotFound {
		t.Fatalf("unknown id = %d %s", rec.Code, rec.Body)
	}
	id := e.createItem("x")
	for _, k := range []auth.Kind{auth.Ext, auth.Webhook} {
		if _, rec := e.get("/v1/why/"+id, e.token(k)); rec.Code != 403 || codeOf(t, rec) != domain.CodePermissionDenied {
			t.Fatalf("%s = %d %s", k, rec.Code, rec.Body)
		}
	}
}

// 运行 id 也能回放（不是事项，没有初筛状态）。
func TestWhyRunID(t *testing.T) {
	e := setup(t)
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvRunStarted, Object: "run_1",
			Actor: domain.Actor{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI}, Payload: json.RawMessage(`{"item":"i"}`)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	s, rec := e.get("/v1/why/run_1", e.token(auth.Observer))
	if rec.Code != 200 || len(s.Steps) != 1 || s.Steps[0].Type != domain.EvRunStarted {
		t.Fatalf("run why = %d %s", rec.Code, rec.Body)
	}
}

// web_only 初筛未返回：非 Web 凭据只拿到标记，Web 本人仍看到全部；审计同样不泄漏该事项的事件。
func TestScreeningPendingHidesTimelineFromNonWeb(t *testing.T) {
	e := setup(t)
	id := e.createItem("pay vendor")
	other := e.createItem("other")
	rec := e.do("POST", "/v1/items/"+id+"/transition", `{"to":"open","reason":"go"}`, e.web)
	if rec.Code != 200 {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		it, err := kitems.Get(tx, id)
		if err != nil {
			return err
		}
		it.Version = 0
		it.Ask = &domain.Ask{AskKernelState: domain.AskKernelState{ScreeningPending: true}}
		b, _ := json.Marshal(it)
		_, err = tx.Exec(`UPDATE items SET data = ? WHERE id = ?`, string(b), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	obs, rec := e.get("/v1/why/"+id, e.token(auth.Observer))
	if rec.Code != 200 || !obs.ScreeningPending || len(obs.Steps) != 0 {
		t.Fatalf("observer = %d %s", rec.Code, rec.Body)
	}
	if web, _ := e.get("/v1/why/"+id, e.web); web.ScreeningPending || len(web.Steps) == 0 {
		t.Fatalf("web must see timeline: %+v", web)
	}
	aud, rec := e.get("/v1/audit/events", e.token(auth.Observer))
	if rec.Code != 200 || len(aud.Events) == 0 {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body)
	}
	for _, ev := range aud.Events {
		if ev.Object == id {
			t.Fatalf("audit leaks pending item event: %+v", ev)
		}
	}
	if aud, _ := e.get("/v1/audit/events?object="+other, e.token(auth.Observer)); len(aud.Events) == 0 {
		t.Fatal("unrelated item must stay visible")
	}
	if aud, _ := e.get("/v1/audit/events?object="+id, e.web); len(aud.Events) == 0 {
		t.Fatal("web audit must include pending item")
	}
}

// 审计：按操作者、对象、类型筛选；游标分页；被拒绝的命令也能查到；内容按凭据裁剪。
func TestAuditFiltersPagesAndTrims(t *testing.T) {
	e := setup(t)
	a, b := e.createItem("alpha"), e.createItem("beta")
	bot := domain.Actor{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvCommandRejected, Object: a, Actor: bot,
			Reason: "SECRET-REJECT", Payload: json.RawMessage(`{"exit":20}`)})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	all, rec := e.get("/v1/audit/events", e.web)
	if rec.Code != 200 || len(all.Events) < 3 {
		t.Fatalf("all = %d %s", rec.Code, rec.Body)
	}
	for i := 1; i < len(all.Events); i++ {
		if all.Events[i].Seq <= all.Events[i-1].Seq {
			t.Fatal("events must be ordered by seq")
		}
	}
	byObj, _ := e.get("/v1/audit/events?object="+b, e.web)
	if len(byObj.Events) == 0 {
		t.Fatal("no events for b")
	}
	for _, ev := range byObj.Events {
		if ev.Object != b {
			t.Fatalf("object filter leaked %+v", ev)
		}
	}
	byActor, _ := e.get("/v1/audit/events?actor=bot", e.web)
	if len(byActor.Events) != 1 || byActor.Events[0].Type != domain.EvCommandRejected {
		t.Fatalf("actor filter = %+v", byActor.Events)
	}
	byType, _ := e.get("/v1/audit/events?type="+domain.EvCommandRejected+"&object="+a, e.web)
	if len(byType.Events) != 1 {
		t.Fatalf("type+object filter = %+v", byType.Events)
	}
	if none, rec := e.get("/v1/audit/events?actor=nobody", e.web); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"events":[]`) || len(none.Events) != 0 {
		t.Fatalf("empty result must be [] : %s", rec.Body)
	}

	first, _ := e.get("/v1/audit/events?limit=2", e.web)
	if len(first.Events) != 2 {
		t.Fatalf("limit=2 gave %d", len(first.Events))
	}
	rest, _ := e.get("/v1/audit/events?after="+itoa(first.Events[1].Seq), e.web)
	if len(rest.Events) != len(all.Events)-2 || rest.Events[0].Seq != all.Events[2].Seq {
		t.Fatalf("paging: rest=%d all=%d", len(rest.Events), len(all.Events))
	}

	obs, rec := e.get("/v1/audit/events?actor=bot", e.token(auth.Observer))
	if rec.Code != 200 || len(obs.Events) != 1 || !obs.Events[0].Redacted || strings.Contains(rec.Body.String(), "SECRET-REJECT") {
		t.Fatalf("observer audit = %d %s", rec.Code, rec.Body)
	}
	if web, _ := e.get("/v1/audit/events?actor=bot", e.web); web.Events[0].Reason != "SECRET-REJECT" {
		t.Fatalf("web audit reason = %+v", web.Events)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// 审计错误语义：参数非法 400、未登录 401、ext 凭据 403。
func TestAuditErrors(t *testing.T) {
	e := setup(t)
	for _, q := range []string{"limit=x", "after=x", "limit=-1", "limit=501", "after=-1"} {
		if _, rec := e.get("/v1/audit/events?"+q, e.web); rec.Code != 400 || codeOf(t, rec) != domain.CodeInvalidRequest {
			t.Fatalf("%s = %d %s", q, rec.Code, rec.Body)
		}
	}
	if _, rec := e.get("/v1/audit/events", nil); rec.Code != 401 {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	if _, rec := e.get("/v1/audit/events", e.token(auth.Ext)); rec.Code != 403 {
		t.Fatalf("ext = %d", rec.Code)
	}
}
