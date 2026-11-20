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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
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
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, kitems.Module, core.Module,
		proc.Module, registry.Module, runner.Module, runidentity.Module} {
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
	ID    string `json:"id"`
	Steps []struct {
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
		Seq        int64
		Type       string
		Object     string
		Actor      domain.Actor
		Reason     string
		Redacted   bool
		Imported   string
		RecordedAt string `json:"recorded_at"`
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

// 处理经过：本人与 observer 可读事项与来源消息的经过；运行凭据、ext 凭据不可读；不存在的消息 404。
func TestTraceRoutes(t *testing.T) {
	e := setup(t)
	id := e.createItem("fix login")
	var tr struct {
		Entries []struct{ Kind string }
	}
	rec := e.do("GET", "/v1/items/"+id+"/trace", "", e.web)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &tr) != nil || len(tr.Entries) != 1 || tr.Entries[0].Kind != "created" {
		t.Fatalf("owner trace: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items/"+id+"/trace", "", e.token(auth.Observer)); rec.Code != 200 {
		t.Fatalf("observer trace: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items/"+id+"/trace", "", e.token(auth.Ext)); rec.Code != 403 {
		t.Fatalf("ext trace: %d %s", rec.Code, rec.Body)
	}
	run := e.runCredential(id)
	for _, path := range []string{"/v1/items/" + id + "/trace", "/v1/source-events/sev_x/trace"} {
		if rec := e.do("GET", path, "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+run) }); rec.Code != 403 {
			t.Fatalf("run credential %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := e.do("GET", "/v1/source-events/sev_missing/trace", "", e.web); rec.Code != 404 {
		t.Fatalf("missing message: %d %s", rec.Code, rec.Body)
	}
}

func (e *env) runCredential(item string) string {
	e.t.Helper()
	var run string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: item, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, time.Now())
		if err != nil {
			return err
		}
		run, err = runidentity.Issue(tx, r.ID, time.Now())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return run
}

func TestAuditNewestFirstPagesByBefore(t *testing.T) {
	e := setup(t)
	a := e.createItem("alpha")
	e.createItem("beta")
	e.createItem("gamma")
	e.createItem("delta")
	all, _ := e.get("/v1/audit/events", e.web)
	n := len(all.Events)
	if n < 4 {
		t.Fatalf("need >= 4 events, got %d", n)
	}
	// 倒序第一页是最新的，序号严格递减。
	first, rec := e.get("/v1/audit/events?order=desc&limit=2", e.web)
	if rec.Code != 200 || len(first.Events) != 2 || first.Events[0].Seq != all.Events[n-1].Seq || first.Events[1].Seq != all.Events[n-2].Seq {
		t.Fatalf("desc first page = %d %s", rec.Code, rec.Body)
	}
	// before 翻页：接着是更早的，不重复、不漏，翻到头与升序全量互为逆序。
	var got []int64
	for _, ev := range first.Events {
		got = append(got, ev.Seq)
	}
	for cursor := got[len(got)-1]; ; {
		pg, _ := e.get("/v1/audit/events?order=desc&limit=2&before="+itoa(cursor), e.web)
		if len(pg.Events) == 0 {
			break
		}
		for _, ev := range pg.Events {
			got = append(got, ev.Seq)
		}
		cursor = pg.Events[len(pg.Events)-1].Seq
	}
	if len(got) != n {
		t.Fatalf("desc paging got %d of %d", len(got), n)
	}
	for i, seq := range got {
		if seq != all.Events[n-1-i].Seq {
			t.Fatalf("desc paging out of order at %d: %v", i, got)
		}
	}
	// 与筛选组合。
	byObj, _ := e.get("/v1/audit/events?order=desc&object="+a, e.web)
	for i, ev := range byObj.Events {
		if ev.Object != a || (i > 0 && ev.Seq >= byObj.Events[i-1].Seq) {
			t.Fatalf("desc+object = %+v", byObj.Events)
		}
	}
	// 旧语义不变：order=asc 等同缺省；游标与顺序混用、未知顺序是 400。
	asc, _ := e.get("/v1/audit/events?order=asc", e.web)
	if len(asc.Events) != n || asc.Events[0].Seq != all.Events[0].Seq {
		t.Fatal("order=asc must equal the default")
	}
	for _, bad := range []string{"order=desc&after=1", "before=5", "order=sideways", "order=desc&before=x"} {
		if _, rec := e.get("/v1/audit/events?"+bad, e.web); rec.Code != 400 {
			t.Fatalf("%s = %d", bad, rec.Code)
		}
	}
}

// GET /audit/events 返回导入事件的 imported、recorded_at，并可按 imported=<批次> 只取一个批次；普通事件不带这两个字段。
func TestAuditShowsImportMarksAndFiltersByBatch(t *testing.T) {
	e := setup(t)
	a := e.createItem("alpha")
	rec := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, batch := range []string{"imp_1", "imp_2"} {
			if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemProgressAdded, Object: a, Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb},
				Payload: json.RawMessage(`{}`), Imported: batch, RecordedAt: rec, At: rec.AddDate(-1, 0, 0)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	one, rec2 := e.get("/v1/audit/events?imported=imp_1", e.web)
	if rec2.Code != 200 || len(one.Events) != 1 || one.Events[0].Imported != "imp_1" || one.Events[0].RecordedAt == "" {
		t.Fatalf("batch filter = %d %s", rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), `"recorded_at":"2026-11-16T08:00:00Z"`) {
		t.Fatalf("recorded_at missing: %s", rec2.Body)
	}
	all, _ := e.get("/v1/audit/events", e.web)
	marked, plain := 0, 0
	for _, ev := range all.Events {
		if ev.Imported != "" {
			marked++
		} else if ev.RecordedAt == "" {
			plain++
		}
	}
	if marked != 2 || plain == 0 {
		t.Fatalf("unfiltered audit is the full read: marked=%d plain=%d", marked, plain)
	}
	if none, _ := e.get("/v1/audit/events?imported=nope", e.web); len(none.Events) != 0 {
		t.Fatalf("unknown batch: %+v", none.Events)
	}
}
