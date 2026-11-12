package items

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
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

// setup 装配真实账本、命令执行器、事项命令和 HTTP 服务，并以本人口令登录一个 Web 会话。
func setup(t *testing.T, staleAfter ...func() time.Duration) *env {
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
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(x, st, staleAfter...)}})
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

// token 签发一个凭据，返回设置 Bearer 头的修改器。
func (e *env) token(kind auth.Kind, executor string) func(*http.Request) {
	e.t.Helper()
	var secret string
	scope := ""
	if kind == auth.Ext || kind == auth.Webhook {
		scope = "x"
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Executor: executor, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return hdr("Authorization", "Bearer "+secret)
}

type writeResp struct {
	ID       string `json:"id"`
	Version  int64  `json:"version"`
	Replayed bool   `json:"replayed"`
}

func code(t *testing.T, rec *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var e domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return e.Code
}

func (e *env) create(mod func(*http.Request), body string) writeResp {
	e.t.Helper()
	rec := e.do("POST", "/v1/items", body, mod)
	if rec.Code != 201 {
		e.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var w writeResp
	_ = json.Unmarshal(rec.Body.Bytes(), &w)
	return w
}

type getResp struct {
	Item      domain.Item       `json:"item"`
	Relations []domain.Relation `json:"relations"`
}

func (e *env) get(id string) getResp {
	e.t.Helper()
	rec := e.do("GET", "/v1/items/"+id, "", e.web)
	if rec.Code != 200 {
		e.t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	var g getResp
	_ = json.Unmarshal(rec.Body.Bytes(), &g)
	return g
}

// ACT-ITEM-08：创建、读取、列表、更新。
func TestCreateGetListUpdate(t *testing.T) {
	e := setup(t)
	w := e.create(e.web, `{"kind":"task","title":"write report","priority":"high","tags":["a"]}`)
	if w.ID == "" || w.Version != 1 || w.Replayed {
		t.Fatalf("create result = %+v", w)
	}
	g := e.get(w.ID)
	if g.Item.Title != "write report" || g.Item.Phase != domain.PhaseProposed || g.Item.Version != 1 || len(g.Relations) != 0 {
		t.Fatalf("item = %+v", g)
	}

	e.create(e.web, `{"kind":"note","title":"other"}`)
	rec := e.do("GET", "/v1/items?kind=task", "", e.web)
	var l struct{ Items []domain.Item }
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	if rec.Code != 200 || len(l.Items) != 1 || l.Items[0].ID != w.ID {
		t.Fatalf("filtered list = %d %s", rec.Code, rec.Body)
	}
	rec = e.do("GET", "/v1/items?kind=none", "", e.web)
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty list must be [] not null: %s", rec.Body)
	}

	rec = e.do("PATCH", "/v1/items/"+w.ID, `{"title":"renamed","reason":"typo"}`, both(e.web, hdr("If-Match", `"1"`)))
	var up writeResp
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	if rec.Code != 200 || up.Version != 2 || e.get(w.ID).Item.Title != "renamed" {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}

	// created_at、updated_at 随详情与列表返回：改动只推进 updated_at。
	after := e.get(w.ID).Item
	if !after.CreatedAt.Equal(g.Item.CreatedAt) || !after.UpdatedAt.After(g.Item.UpdatedAt) || !g.Item.CreatedAt.Equal(g.Item.UpdatedAt) || g.Item.CreatedAt.IsZero() {
		t.Fatalf("timestamps: before %+v after %+v", g.Item, after)
	}
	_ = json.Unmarshal(e.do("GET", "/v1/items?kind=task", "", e.web).Body.Bytes(), &l)
	if !l.Items[0].CreatedAt.Equal(after.CreatedAt) || !l.Items[0].UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("list timestamps: %+v", l.Items[0])
	}
}

// deadline 经 POST 写入、GET 读出，PATCH 可改可清除；格式错误是请求错误。
func TestDeadlineField(t *testing.T) {
	e := setup(t)
	w := e.create(e.web, `{"kind":"task","title":"t","phase":"open","deadline":"2030-01-02T03:04:05Z"}`)
	if d := e.get(w.ID).Item.Deadline; d == nil || d.Format(time.RFC3339) != "2030-01-02T03:04:05Z" {
		t.Fatalf("deadline after create = %v", d)
	}
	if rec := e.do("PATCH", "/v1/items/"+w.ID, `{"deadline":"2030-02-02T03:04:05Z"}`, e.web); rec.Code != 200 {
		t.Fatalf("change = %d %s", rec.Code, rec.Body)
	}
	if d := e.get(w.ID).Item.Deadline; d == nil || d.Format(time.RFC3339) != "2030-02-02T03:04:05Z" {
		t.Fatalf("deadline after change = %v", d)
	}
	if rec := e.do("PATCH", "/v1/items/"+w.ID, `{"deadline":"soon"}`, e.web); code(t, rec) != domain.CodeInvalidRequest {
		t.Fatalf("bad deadline = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PATCH", "/v1/items/"+w.ID, `{"deadline":null}`, e.web); rec.Code != 200 {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items/"+w.ID, "", e.web); strings.Contains(rec.Body.String(), `"deadline"`) {
		t.Fatalf("cleared deadline must be omitted: %s", rec.Body)
	}
}

// ACT-ITEM-09：转阶段与关联。
func TestTransitionAndRelations(t *testing.T) {
	e := setup(t)
	a := e.create(e.web, `{"kind":"task","title":"parent"}`)
	b := e.create(e.web, `{"kind":"task","title":"child"}`)

	rec := e.do("POST", "/v1/items/"+a.ID+"/transition", `{"to":"open","reason":"start"}`, e.web)
	if rec.Code != 200 || e.get(a.ID).Item.Phase != domain.PhaseOpen {
		t.Fatalf("transition = %d %s", rec.Code, rec.Body)
	}
	// 阶段进入约束由内核判定，路由原样返回 400。
	rec = e.do("POST", "/v1/items/"+a.ID+"/transition", `{"to":"waiting"}`, e.web)
	if rec.Code != 400 {
		t.Fatalf("waiting without waiting_on = %d %s", rec.Code, rec.Body)
	}

	rec = e.do("POST", "/v1/items/"+b.ID+"/relations", `{"to":"`+a.ID+`","type":"parent"}`, e.web)
	if rec.Code != 200 {
		t.Fatalf("relate = %d %s", rec.Code, rec.Body)
	}
	if rels := e.get(a.ID).Relations; len(rels) != 1 || rels[0].From != b.ID || rels[0].To != a.ID || rels[0].Type != "parent" {
		t.Fatalf("relations = %+v", rels)
	}
	if rec = e.do("POST", "/v1/items/"+b.ID+"/relations", `{"to":"`+a.ID+`","type":"parent"}`, e.web); rec.Code != 409 {
		t.Fatalf("duplicate relation = %d %s", rec.Code, rec.Body)
	}
	if rec = e.do("POST", "/v1/items/"+a.ID+"/relations", `{"to":"`+b.ID+`","type":"parent"}`, e.web); rec.Code != 400 {
		t.Fatalf("parent cycle = %d %s", rec.Code, rec.Body)
	}
	rec = e.do("POST", "/v1/items/"+b.ID+"/relations", `{"to":"`+a.ID+`","type":"parent","remove":true}`, e.web)
	if rec.Code != 200 || len(e.get(a.ID).Relations) != 0 {
		t.Fatalf("unrelate = %d %s", rec.Code, rec.Body)
	}
}

// 版本冲突：过期的 If-Match 返回 409 且不写入；非法的 If-Match 返回 400。
func TestVersionConflict(t *testing.T) {
	e := setup(t)
	w := e.create(e.web, `{"kind":"task","title":"t"}`)
	if rec := e.do("PATCH", "/v1/items/"+w.ID, `{"title":"one"}`, both(e.web, hdr("If-Match", "1"))); rec.Code != 200 {
		t.Fatalf("first patch = %d %s", rec.Code, rec.Body)
	}
	rec := e.do("PATCH", "/v1/items/"+w.ID, `{"title":"two"}`, both(e.web, hdr("If-Match", "1")))
	if rec.Code != 409 || code(t, rec) != domain.CodeConflict {
		t.Fatalf("stale patch = %d %s", rec.Code, rec.Body)
	}
	if e.get(w.ID).Item.Title != "one" {
		t.Fatal("stale write must not apply")
	}
	if rec = e.do("PATCH", "/v1/items/"+w.ID, `{"title":"x"}`, both(e.web, hdr("If-Match", "abc"))); rec.Code != 400 {
		t.Fatalf("bad If-Match = %d %s", rec.Code, rec.Body)
	}
	if rec = e.do("POST", "/v1/items/"+w.ID+"/transition", `{"to":"open"}`, both(e.web, hdr("If-Match", "1"))); rec.Code != 409 {
		t.Fatalf("stale transition = %d %s", rec.Code, rec.Body)
	}
}

// 幂等键：同键同输入只写一次并重放首次结果；同键异输入冲突。
func TestIdempotencyKey(t *testing.T) {
	e := setup(t)
	mod := both(e.web, hdr("Idempotency-Key", "k1"))
	body := `{"kind":"task","title":"once"}`
	first := e.create(mod, body)
	rec := e.do("POST", "/v1/items", `{"title":"once","kind":"task"}`, mod) // 键序不同，输入等价
	var again writeResp
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != 200 || !again.Replayed || again.ID != first.ID || again.Version != first.Version {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body)
	}
	var l struct{ Items []domain.Item }
	_ = json.Unmarshal(e.do("GET", "/v1/items", "", e.web).Body.Bytes(), &l)
	if len(l.Items) != 1 {
		t.Fatalf("idempotent create wrote %d items", len(l.Items))
	}
	if rec = e.do("POST", "/v1/items", `{"kind":"task","title":"different"}`, mod); rec.Code != 409 {
		t.Fatalf("same key, different input = %d %s", rec.Code, rec.Body)
	}
}

// 内核字段防写与输入严格性：全部 400，且不改动事项。
func TestKernelFieldsAndStrictInput(t *testing.T) {
	e := setup(t)
	w := e.create(e.web, `{"kind":"task","title":"t"}`)
	cases := []struct{ method, path, body string }{
		{"POST", "/v1/items", `{"kind":"task","title":"x","chain_depth":5}`},
		{"POST", "/v1/items", `{"kind":"task","title":"x","version":9}`},
		{"POST", "/v1/items", `{"kind":"task","title":"x","Unattended":true}`},
		{"POST", "/v1/items", `{"kind":"task","title":"x","created_at":"2026-01-01T00:00:00Z"}`},
		{"POST", "/v1/items", `{"kind":"task","title":"x","ask":{"question":"q","risk_class":"money"}}`},
		{"POST", "/v1/items", `{"kind":"task","title":"x","bogus":1}`},
		{"POST", "/v1/items", `null`},
		{"POST", "/v1/items", `[]`},
		{"POST", "/v1/items", `{"kind":"task","title":"x"} {}`},
		{"PATCH", "/v1/items/" + w.ID, `{"chain_depth":5}`},
		{"PATCH", "/v1/items/" + w.ID, `{"updated_at":"2026-01-01T00:00:00Z"}`},
		{"PATCH", "/v1/items/" + w.ID, `{"phase":"open"}`},
		{"PATCH", "/v1/items/" + w.ID, `{"id":"itm_other","title":"x"}`},
		{"PATCH", "/v1/items/" + w.ID, `{"ID":"itm_other","title":"x"}`},
		{"PATCH", "/v1/items/" + w.ID, `{}`},
		{"POST", "/v1/items/" + w.ID + "/transition", `{"to":"open","id":"itm_other"}`},
		{"POST", "/v1/items/" + w.ID + "/relations", `{"from":"itm_other","to":"x","type":"related"}`},
		{"POST", "/v1/items/" + w.ID + "/revert", `{"event":1,"seq":1}`},
	}
	for _, c := range cases {
		if rec := e.do(c.method, c.path, c.body, e.web); rec.Code != 400 {
			t.Errorf("%s %s %s = %d %s, want 400", c.method, c.path, c.body, rec.Code, rec.Body)
		}
	}
	var l struct{ Items []domain.Item }
	_ = json.Unmarshal(e.do("GET", "/v1/items", "", e.web).Body.Bytes(), &l)
	if len(l.Items) != 1 || l.Items[0].Title != "t" || l.Items[0].ChainDepth != 0 || l.Items[0].Version != 1 {
		t.Fatalf("rejected writes changed state: %+v", l.Items)
	}
}

// §14.3 授权在路由上的体现：401、读权限、改自己与他人的事项。
func TestAuthorization(t *testing.T) {
	e := setup(t)
	if rec := e.do("GET", "/v1/items", "", nil); rec.Code != 401 {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	obs, wrk, ext := e.token(auth.Observer, ""), e.token(auth.Worker, "wrk1"), e.token(auth.Ext, "")

	mine := e.create(e.web, `{"kind":"task","title":"mine","assignee":"wrk1"}`)
	theirs := e.create(e.web, `{"kind":"task","title":"theirs","assignee":"wrk2"}`)

	if rec := e.do("GET", "/v1/items/"+mine.ID, "", obs); rec.Code != 200 {
		t.Fatalf("observer read = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items", "", ext); rec.Code != 403 {
		t.Fatalf("ext read = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items/itm_nope", "", obs); rec.Code != 404 || code(t, rec) != domain.CodeNotFound {
		t.Fatalf("missing item = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PATCH", "/v1/items/"+mine.ID, `{"title":"x"}`, obs); rec.Code != 403 {
		t.Fatalf("observer write = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PATCH", "/v1/items/"+theirs.ID, `{"title":"x"}`, wrk); rec.Code != 403 {
		t.Fatalf("worker editing another's item = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PATCH", "/v1/items/"+mine.ID, `{"title":"done by wrk"}`, wrk); rec.Code != 200 {
		t.Fatalf("worker editing own item = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items", `{"kind":"task","title":"idea"}`, obs); rec.Code != 201 {
		t.Fatalf("observer proposing = %d %s", rec.Code, rec.Body)
	}
	// worker 不能自己确认建议：离开 proposed 属于代答。
	if rec := e.do("POST", "/v1/items/"+mine.ID+"/transition", `{"to":"open"}`, wrk); rec.Code != 403 {
		t.Fatalf("worker accepting proposal = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items", `{"kind":"task","title":"x"}`, hdr("Authorization", "Bearer apa_wrk_bad")); rec.Code != 401 {
		t.Fatalf("bad bearer = %d", rec.Code)
	}
}

// ACT-ITEM-10 / ACT-REV-03：撤销 agent 变更只接受 Web 会话，Bearer 一律 403。
// 导出 ext 只读事项，初筛中的决策同样屏蔽；其他 ext 仍不能读（§10.3、§14.3）。
func TestExportExtCanOnlyReadItems(t *testing.T) {
	e := setup(t)
	issue := func(scope string) func(*http.Request) {
		var secret string
		if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
			_, secret, err = e.auth.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: scope})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return hdr("Authorization", "Bearer "+secret)
	}
	id := e.create(e.web, `{"kind":"task","title":"secret title"}`).ID
	exp := issue("export")
	for _, path := range []string{"/v1/items", "/v1/items/" + id} {
		if rec := e.do("GET", path, "", exp); rec.Code != 200 || !strings.Contains(rec.Body.String(), "secret title") {
			t.Fatalf("export read %s: %d %s", path, rec.Code, rec.Body)
		}
		if rec := e.do("GET", path, "", issue("export.notes")); rec.Code != 200 {
			t.Fatalf("named export read %s: %d", path, rec.Code)
		}
		for _, other := range []string{"forward", "channel.chat", "exporter"} {
			if rec := e.do("GET", path, "", issue(other)); rec.Code != 403 {
				t.Fatalf("%s read %s: %d", other, path, rec.Code)
			}
		}
	}
	if rec := e.do("POST", "/v1/items", `{"kind":"task","title":"x"}`, exp); rec.Code != 403 {
		t.Fatalf("export write: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PATCH", "/v1/items/"+id, `{"title":"x"}`, exp); rec.Code != 403 {
		t.Fatalf("export update: %d %s", rec.Code, rec.Body)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := kitems.Revise(tx, id, "test", time.Now(), func(it *domain.Item) error {
			it.Ask = &domain.Ask{AskKernelState: domain.AskKernelState{ScreeningPending: true}}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("GET", "/v1/items/"+id, "", exp); rec.Code != 200 || strings.Contains(rec.Body.String(), "secret title") {
		t.Fatalf("screening not redacted: %d %s", rec.Code, rec.Body)
	}
}

func TestRevertIsWebOnly(t *testing.T) {
	e := setup(t)
	wrk, dlg, obs := e.token(auth.Worker, "wrk1"), e.token(auth.Delegate, ""), e.token(auth.Observer, "")
	w := e.create(e.web, `{"kind":"task","title":"orig","assignee":"wrk1"}`)
	if rec := e.do("PATCH", "/v1/items/"+w.ID, `{"title":"agent edit"}`, wrk); rec.Code != 200 {
		t.Fatalf("agent edit = %d %s", rec.Code, rec.Body)
	}
	var seq int64
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, w.ID)
		if err == nil {
			seq = es[len(es)-1].Event.Seq
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"event":` + itoa(seq) + `,"reason":"undo"}`
	path := "/v1/items/" + w.ID + "/revert"

	for name, mod := range map[string]func(*http.Request){"worker": wrk, "delegate": dlg, "observer": obs} {
		rec := e.do("POST", path, body, mod)
		if rec.Code != 403 {
			t.Fatalf("%s revert = %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	if e.get(w.ID).Item.Title != "agent edit" {
		t.Fatal("refused revert must not change the item")
	}
	// Web 会话缺 CSRF 令牌同样被骨干拒绝。
	if rec := e.do("POST", path, body, func(r *http.Request) { r.AddCookie(e.cookie) }); rec.Code != 403 {
		t.Fatalf("revert without csrf = %d %s", rec.Code, rec.Body)
	}

	rec := e.do("POST", path, body, e.web)
	if rec.Code != 200 {
		t.Fatalf("web revert = %d %s", rec.Code, rec.Body)
	}
	if got := e.get(w.ID).Item.Title; got != "orig" {
		t.Fatalf("title after revert = %q", got)
	}
	// 本人自己做的变更不属于"agent 变更"，不能撤销。
	if rec = e.do("POST", path, `{"event":`+itoa(seq+1)+`}`, e.web); rec.Code != 400 && rec.Code != 404 {
		t.Fatalf("reverting a non-agent event = %d %s", rec.Code, rec.Body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ACT-WBN-05/06：PATCH 写与清计划日期；非法值 400 且不写入。
func TestPlannedForOverHTTP(t *testing.T) {
	e := setup(t)
	wrk := e.token(auth.Worker, "wrk1")
	tmrw := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	w := e.create(e.web, `{"kind":"task","title":"f","phase":"open","assignee":"wrk1","planned_for":"`+tmrw+`"}`)
	if got := e.get(w.ID).Item.PlannedFor; got != tmrw {
		t.Fatalf("planned_for on create = %q", got)
	}
	for _, bad := range []string{"2026-13-40", "2026-11-12T10:00:00Z"} {
		if rec := e.do("PATCH", "/v1/items/"+w.ID, `{"planned_for":"`+bad+`"}`, wrk); rec.Code != 400 || !strings.Contains(rec.Body.String(), "YYYY-MM-DD") {
			t.Fatalf("%s = %d %s", bad, rec.Code, rec.Body)
		}
	}
	if g := e.get(w.ID); g.Item.PlannedFor != tmrw || g.Item.Version != 1 {
		t.Fatalf("rejected writes changed the item: %+v", g.Item)
	}
	if rec := e.do("PATCH", "/v1/items/"+w.ID, `{"planned_for":null}`, wrk); rec.Code != 200 {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body)
	}
	rec := e.do("GET", "/v1/items/"+w.ID, "", e.web)
	if strings.Contains(rec.Body.String(), "planned_for") {
		t.Fatalf("cleared planned_for must not be returned: %s", rec.Body)
	}
}

// ACT-WBN-09/10/11：POST progress 追加、GET 带 progress[]、未来日期 400、越权 403。
func TestProgressOverHTTP(t *testing.T) {
	e := setup(t)
	wrk, wrk2 := e.token(auth.Worker, "wrk1"), e.token(auth.Worker, "wrk2")
	w := e.create(e.web, `{"kind":"task","title":"f","phase":"open","assignee":"wrk1"}`)
	yday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tmrw := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	rec := e.do("POST", "/v1/items/"+w.ID+"/progress", `{"text":"联系了供应商","on":"`+yday+`"}`, wrk)
	var created struct {
		Progress domain.Progress `json:"progress"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != 201 || created.Progress.Text != "联系了供应商" || created.Progress.On != yday {
		t.Fatalf("progress must return 201 with the entry: %d %s", rec.Code, rec.Body)
	}
	var g struct {
		Item     domain.Item
		Progress []domain.Progress `json:"progress"`
	}
	rec = e.do("GET", "/v1/items/"+w.ID, "", e.web)
	_ = json.Unmarshal(rec.Body.Bytes(), &g)
	if g.Item.Phase != domain.PhaseOpen || len(g.Progress) != 1 || g.Progress[0].Text != "联系了供应商" || g.Progress[0].On != yday || g.Progress[0].Actor != "wrk1" || g.Progress[0].At.IsZero() {
		t.Fatalf("progress[] = %+v (%s)", g.Progress, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+w.ID+"/progress", `{"text":"未来","on":"`+tmrw+`"}`, wrk); rec.Code != 400 {
		t.Fatalf("future on = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+w.ID+"/progress", `{"text":"x"}`, wrk2); rec.Code != 403 {
		t.Fatalf("another worker = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items/"+w.ID, "", e.web); strings.Count(rec.Body.String(), `"text"`) != 1 {
		t.Fatalf("rejected progress must not be stored: %s", rec.Body)
	}
}

// ACT-WBN-07：GET /today 四组，observer 可读。
func TestTodayOverHTTP(t *testing.T) {
	e := setup(t)
	obs := e.token(auth.Observer, "")
	yday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tmrw := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	a := e.create(e.web, `{"kind":"task","title":"A","phase":"open","planned_for":"`+yday+`"}`)
	e.create(e.web, `{"kind":"task","title":"C","phase":"open","planned_for":"`+tmrw+`"}`)
	g := e.create(e.web, `{"kind":"decision","title":"G","phase":"needs_input","ask":{"question":"q?"}}`)
	rec := e.do("GET", "/v1/today", "", obs)
	var l map[string][]domain.Item
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	for _, k := range []string{"planned", "due", "pending", "stale"} {
		if l[k] == nil {
			t.Fatalf("group %s missing: %s", k, rec.Body)
		}
	}
	if len(l["planned"]) != 1 || l["planned"][0].ID != a.ID || l["planned"][0].PlannedFor != yday || len(l["pending"]) != 1 || l["pending"][0].ID != g.ID {
		t.Fatalf("today = %s", rec.Body)
	}
	if rec := e.do("GET", "/v1/today", "", nil); rec.Code != 401 {
		t.Fatalf("anonymous = %d", rec.Code)
	}
}

// ACT-STL-02：/today 的久未推进阈值取自规则（可热变），默认 14 天。
func TestTodayStaleUsesRuleThreshold(t *testing.T) {
	after := 14 * 24 * time.Hour
	e := setup(t, func() time.Duration { return after })
	obs := e.token(auth.Observer, "")
	it := e.create(e.web, `{"kind":"task","title":"S","phase":"open"}`)
	stale := func() []domain.Item {
		var l map[string][]domain.Item
		_ = json.Unmarshal(e.do("GET", "/v1/today", "", obs).Body.Bytes(), &l)
		return l["stale"]
	}
	if got := stale(); len(got) != 0 {
		t.Fatalf("fresh item listed as stale: %+v", got)
	}
	time.Sleep(20 * time.Millisecond)
	after = 10 * time.Millisecond // 规则热加载后阈值缩短
	if got := stale(); len(got) != 1 || got[0].ID != it.ID {
		t.Fatalf("stale group = %+v, want the item", got)
	}
}
