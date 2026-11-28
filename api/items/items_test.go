package items

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
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

// setup 装配真实账本、命令执行器、事项命令和 HTTP 服务，并以本人口令登录一个 Web 会话。
func setup(t *testing.T, staleAfter ...func() time.Duration) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, kitems.Module, registry.Module, runner.Module, core.Module} {
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
	if err := decisions.Register(x); err != nil {
		t.Fatal(err)
	}
	if err := inbound.RegisterAttach(x); err != nil {
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

// agent 签一把归属 Agent id 的 delegate 钥匙，并由本人把该 Agent 的档位设为 tier。
func (e *env) agent(id string, tier policy.Card) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: auth.Delegate, AgentID: id, Harness: "h"})
		if err != nil {
			return err
		}
		secret = s
		_, err = policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "test tier", func(p *policy.Policy) error {
			p.AgentCards[id] = tier
			return nil
		})
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
	rec := e.do("POST", "/v1/items/"+w.ID+"/comments", `{"text":"联系了供应商","on":"`+yday+`"}`, wrk)
	var created struct {
		Comment domain.Progress `json:"comment"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != 201 || created.Comment.Text != "联系了供应商" || created.Comment.On != yday {
		t.Fatalf("comment must return 201 with the entry: %d %s", rec.Code, rec.Body)
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
	if rec := e.do("POST", "/v1/items/"+w.ID+"/comments", `{"text":"未来","on":"`+tmrw+`"}`, wrk); rec.Code != 400 {
		t.Fatalf("future on = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+w.ID+"/comments", `{"text":"x"}`, wrk2); rec.Code != 403 {
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
	g := e.create(e.web, `{"kind":"decision","title":"G","phase":"needs_input","ask":{"question":"q?","free_text":true}}`)
	rec := e.do("GET", "/v1/today", "", obs)
	var l map[string][]domain.Item
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	for _, k := range []string{"planned", "due", "pending", "stale", "overdue", "high_priority"} {
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

// ACT-WBN-12/13/14：全文覆盖标题、正文与进展且不改版本；排序单调；计划日期与标签筛选；非法参数 400。
func TestListSearchSortFilterOverHTTP(t *testing.T) {
	e := setup(t)
	obs := e.token(auth.Observer, "")
	c := e.create(e.web, `{"kind":"task","title":"C","phase":"open","body":"护照续签","planned_for":"2026-05-02","priority":"low","tags":["t1"]}`)
	d := e.create(e.web, `{"kind":"task","title":"D","phase":"open","planned_for":"2026-05-01","priority":"urgent"}`)
	n := e.create(e.web, `{"kind":"task","title":"护照照片","phase":"open"}`)
	e.create(e.web, `{"kind":"task","title":"other","phase":"open","planned_for":"2026-05-02"}`)
	if rec := e.do("POST", "/v1/items/"+d.ID+"/comments", `{"text":"等护照照片"}`, e.web); rec.Code != 201 {
		t.Fatalf("progress = %d %s", rec.Code, rec.Body)
	}
	list := func(query string) []string {
		rec := e.do("GET", "/v1/items?"+query, "", obs)
		if rec.Code != 200 {
			t.Fatalf("%s = %d %s", query, rec.Code, rec.Body)
		}
		var r struct{ Items []domain.Item }
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		var out []string
		for _, it := range r.Items {
			out = append(out, it.ID)
		}
		return out
	}
	if got := list("q=" + url.QueryEscape("护照")); len(got) != 3 || strings.Join(got, ",") != strings.Join(sorted(c.ID, d.ID, n.ID), ",") {
		t.Fatalf("q=护照 = %v", got)
	}
	if e.get(d.ID).Item.Version != 2 { // create(1) + progress(2)；搜索不能再改
		t.Fatalf("search changed a version: %d", e.get(d.ID).Item.Version)
	}
	if got := list("sort=planned_for"); got[0] != d.ID || got[len(got)-1] != n.ID { // 05-01 在前，未设计划日期的在末
		t.Fatalf("sort=planned_for = %v", got)
	}
	if got := list("sort=priority"); got[0] != d.ID || got[len(got)-1] != c.ID { // urgent 在前，low 在末
		t.Fatalf("sort=priority = %v", got)
	}
	if got := list("planned_for=2026-05-02"); len(got) != 2 {
		t.Fatalf("planned_for filter = %v", got)
	}
	if got := list("tag=t1&phase=open"); len(got) != 1 || got[0] != c.ID { // v0.1 参数语义不变
		t.Fatalf("tag+phase = %v", got)
	}
	for _, bad := range []string{"sort=nope", "planned_for=tomorrow", "q=!!"} {
		if rec := e.do("GET", "/v1/items?"+bad, "", obs); rec.Code != 400 {
			t.Fatalf("%s = %d %s", bad, rec.Code, rec.Body)
		}
	}
}

func sorted(s ...string) []string { sort.Strings(s); return s }

// ACT-IMP-10：GET /items 按 origin.source 与 origin.ref 精确筛选，可只给来源；筛选与其他条件同时满足。
func TestListFilterByOrigin(t *testing.T) {
	e := setup(t)
	a := e.create(e.web, `{"kind":"task","title":"a","origin":{"source":"hook","ref":"A-1"}}`)
	b := e.create(e.web, `{"kind":"task","title":"b","origin":{"source":"hook","ref":"B-1"}}`)
	e.create(e.web, `{"kind":"task","title":"c","origin":{"source":"other","ref":"A-1"}}`)
	e.create(e.web, `{"kind":"task","title":"d"}`)
	ids := func(q string) []string {
		var l struct{ Items []struct{ ID string } }
		_ = json.Unmarshal(e.do("GET", "/v1/items?"+q, "", e.web).Body.Bytes(), &l)
		var out []string
		for _, it := range l.Items {
			out = append(out, it.ID)
		}
		sort.Strings(out)
		return out
	}
	both := []string{a.ID, b.ID}
	sort.Strings(both)
	if got := ids("origin_source=hook"); strings.Join(got, ",") != strings.Join(both, ",") {
		t.Fatalf("by source: %v want %v", got, both)
	}
	if got := ids("origin_source=hook&origin_ref=B-1"); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("by source and ref: %v", got)
	}
	if got := ids("origin_source=nothing"); len(got) != 0 {
		t.Fatalf("unknown source: %v", got)
	}
}

// 「已完成」页的口径：phase=done 时给出 done_at——转入已完成的时刻；新建即已完成的迁移事项取 ext.workbench.completed 那天；
// 迁移来而没记完成日期的不给时刻、列入 done_unknown（不拿写入时刻或 updated 顶替）；补上完成日期后即归到那天。
func TestDoneAtOverHTTP(t *testing.T) {
	e := setup(t)
	a := e.create(e.web, `{"kind":"task","title":"a","phase":"open"}`)
	if rec := e.do("POST", "/v1/items/"+a.ID+"/transition", `{"to":"done","reason":"ok"}`, e.web); rec.Code != 200 {
		t.Fatalf("transition = %d %s", rec.Code, rec.Body)
	}
	m := e.create(e.web, `{"kind":"task","title":"m","phase":"done","ext":{"workbench":{"completed":"2026-11-18"}}}`)
	u := e.create(e.web, `{"kind":"task","title":"u","phase":"done","ext":{"workbench":{"completed":"","updated":"2026-10-19"}}}`)
	n := e.create(e.web, `{"kind":"task","title":"n","phase":"done"}`)
	read := func() (at map[string]time.Time, unknown []string) {
		t.Helper()
		var out struct {
			DoneAt      map[string]time.Time `json:"done_at"`
			DoneUnknown []string             `json:"done_unknown"`
		}
		rec := e.do("GET", "/v1/items?phase=done", "", e.web)
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.DoneUnknown == nil {
			t.Fatalf("done_at = %d %s", rec.Code, rec.Body)
		}
		return out.DoneAt, out.DoneUnknown
	}
	at, unknown := read()
	if len(at) != 3 || len(unknown) != 1 || unknown[0] != u.ID {
		t.Fatalf("done_at = %v, done_unknown = %v", at, unknown)
	}
	if _, ok := at[u.ID]; ok {
		t.Fatalf("migrated without completed date must have no done_at: %v", at[u.ID])
	}
	for _, id := range []string{a.ID, n.ID} {
		if d := at[id]; time.Since(d) > time.Minute {
			t.Fatalf("done_at of %s = %v", id, d)
		}
	}
	if d := at[m.ID].In(time.Local).Format("2006-01-02 15:04"); d != "2026-11-18 00:00" {
		t.Fatalf("migrated done_at = %s", d)
	}
	if rec := e.do("PATCH", "/v1/items/"+u.ID, `{"reason":"补完成日期","ext":{"workbench":{"completed":"2026-10-18","updated":"2026-10-19"}}}`, e.web); rec.Code != 200 {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body)
	}
	at, unknown = read()
	if len(unknown) != 0 || at[u.ID].In(time.Local).Format("2006-01-02 15:04") != "2026-10-18 00:00" {
		t.Fatalf("after fill: done_at[u] = %v, done_unknown = %v", at[u.ID], unknown)
	}
	if rec := e.do("GET", "/v1/items", "", e.web); strings.Contains(rec.Body.String(), "done_at") || strings.Contains(rec.Body.String(), "done_unknown") {
		t.Fatalf("done_at only with phase=done: %s", rec.Body)
	}
}

// 「已完成」页的进展维度：progress_from/progress_to 只列这段日子里有进展的事项（不看阶段），附上落在范围里的进展；
// 进展日是 on，没写 on 的取记录那天（本机日）。参数成对、跨度与 from/to 同口径，不合法 400。
func TestProgressRangeOverHTTP(t *testing.T) {
	e := setup(t)
	today := time.Now().Format("2006-01-02")
	lastWeek := time.Now().AddDate(0, 0, -8).Format("2006-01-02")
	a := e.create(e.web, `{"kind":"task","title":"a","phase":"open"}`)
	b := e.create(e.web, `{"kind":"task","title":"b","phase":"done"}`)
	c := e.create(e.web, `{"kind":"task","title":"c","phase":"open"}`)
	e.create(e.web, `{"kind":"task","title":"none","phase":"open"}`)
	for _, w := range []struct{ id, body string }{
		{a.ID, `{"text":"今天写的"}`},
		{a.ID, `{"text":"补记上周的","on":"` + lastWeek + `"}`},
		{b.ID, `{"text":"完成前的进展"}`},
		{c.ID, `{"text":"只有上周的","on":"` + lastWeek + `"}`},
	} {
		if rec := e.do("POST", "/v1/items/"+w.id+"/comments", w.body, e.web); rec.Code != 201 {
			t.Fatalf("progress = %d %s", rec.Code, rec.Body)
		}
	}
	var out struct {
		Items    []domain.Item
		Progress map[string][]domain.Progress `json:"progress"`
	}
	rec := e.do("GET", "/v1/items?progress_from="+today+"&progress_to="+today, "", e.web)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatalf("range = %d %s", rec.Code, rec.Body)
	}
	var ids []string
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	if strings.Join(sorted(ids...), ",") != strings.Join(sorted(a.ID, b.ID), ",") || len(out.Progress) != 2 {
		t.Fatalf("items with progress today = %v, progress = %v", ids, out.Progress)
	}
	if p := out.Progress[a.ID]; len(p) != 1 || p[0].Text != "今天写的" {
		t.Fatalf("a progress in range = %+v", p)
	}
	rec = e.do("GET", "/v1/items?progress_from="+lastWeek+"&progress_to="+lastWeek+"&phase=open", "", e.web)
	out.Progress = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Items) != 2 || len(out.Progress[c.ID]) != 1 || out.Progress[a.ID][0].Text != "补记上周的" {
		t.Fatalf("on decides the day and combines with phase: %s", rec.Body)
	}
	if rec := e.do("GET", "/v1/items", "", e.web); strings.Contains(rec.Body.String(), `"progress"`) {
		t.Fatalf("progress only with progress_from/to: %s", rec.Body)
	}
	for _, bad := range []string{"progress_from=" + today, "progress_from=x&progress_to=y", "progress_from=" + today + "&progress_to=" + lastWeek, "progress_from=2026-01-01&progress_to=2026-06-01"} {
		if rec := e.do("GET", "/v1/items?"+bad, "", e.web); rec.Code != 400 {
			t.Fatalf("%s = %d %s", bad, rec.Code, rec.Body)
		}
	}
}

// 今天的逾期与高优先级两组（对齐工作台今日焦点）：截止本机日早于今天且未结束的进 overdue（按截止先后）；
// 紧急与高、未结束的进 high_priority（紧急在前）；已完成的、普通优先级的不进；与 planned 重叠时各组都列，由读者去重。
func TestTodayOverdueAndHighPriority(t *testing.T) {
	e := setup(t)
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }
	late2 := e.create(e.web, `{"kind":"task","title":"晚两天","phase":"open","deadline":"`+day(-2)+`T18:00:00`+time.Now().Format("-07:00")+`"}`)
	late1 := e.create(e.web, `{"kind":"task","title":"晚一天","phase":"waiting","waiting_on":{"kind":"human","ref":"x"},"deadline":"`+day(-1)+`T09:00:00`+time.Now().Format("-07:00")+`"}`)
	e.create(e.web, `{"kind":"task","title":"已完成的逾期","phase":"done","deadline":"`+day(-3)+`T09:00:00Z"}`)
	e.create(e.web, `{"kind":"task","title":"明天到期","phase":"open","deadline":"`+day(1)+`T09:00:00Z"}`)
	high := e.create(e.web, `{"kind":"task","title":"高","phase":"open","priority":"high","planned_for":"`+day(0)+`"}`)
	urgent := e.create(e.web, `{"kind":"task","title":"紧急","phase":"open","priority":"urgent"}`)
	e.create(e.web, `{"kind":"task","title":"普通","phase":"open","priority":"normal"}`)
	e.create(e.web, `{"kind":"task","title":"高但完成了","phase":"done","priority":"high"}`)
	var l map[string][]domain.Item
	rec := e.do("GET", "/v1/today", "", e.web)
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	ids := func(g string) string {
		var out []string
		for _, it := range l[g] {
			out = append(out, it.ID)
		}
		return strings.Join(out, ",")
	}
	if got, want := ids("overdue"), late2.ID+","+late1.ID; got != want {
		t.Fatalf("overdue = %s, want %s (%s)", got, want, rec.Body)
	}
	if got, want := ids("high_priority"), urgent.ID+","+high.ID; got != want {
		t.Fatalf("high_priority = %s, want %s (%s)", got, want, rec.Body)
	}
	if ids("planned") != high.ID {
		t.Fatalf("planned keeps its own rule: %s", ids("planned"))
	}
}

// 建议页的分组依据：phase=proposed 时给出 origins（没有来源事件的建议不在其中）与现算的 groups（每条恰好在一组），其余阶段都不给。
// 分组与配对规则的测试在 kernel/inbound。
func TestOriginsOnlyForProposed(t *testing.T) {
	e := setup(t)
	e.create(e.web, `{"kind":"task","title":"p","phase":"proposed"}`)
	e.create(e.web, `{"kind":"decision","title":"q","phase":"proposed"}`)
	rec := e.do("GET", "/v1/items?phase=proposed", "", e.web)
	var out struct {
		Items   []struct{ ID string }      `json:"items"`
		Origins map[string]json.RawMessage `json:"origins"`
		Groups  []struct {
			Kind  string   `json:"kind"`
			Items []string `json:"items"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 || len(out.Items) != 2 || out.Origins == nil || len(out.Origins) != 0 {
		t.Fatalf("origins = %d %s", rec.Code, rec.Body)
	}
	if len(out.Groups) != 2 || out.Groups[0].Kind != "pinned" || out.Groups[1].Kind != "rest" || len(out.Groups[0].Items)+len(out.Groups[1].Items) != 2 {
		t.Fatalf("groups = %s", rec.Body)
	}
	if rec := e.do("GET", "/v1/items?phase=open", "", e.web); strings.Contains(rec.Body.String(), "origins") || strings.Contains(rec.Body.String(), "groups") {
		t.Fatalf("origins and groups only with phase=proposed: %s", rec.Body)
	}
}

// 已忽略与按批撤销经 HTTP：批量忽略的结果带批次号；phase=dropped 给出 dismissals（批次、理由、时刻）；undo 按批改回建议。
func TestDismissalsAndUndoOverHTTP(t *testing.T) {
	e := setup(t)
	p := e.create(e.web, `{"kind":"task","title":"p","phase":"proposed"}`)
	rec := e.do("POST", "/v1/proposals/batch", `{"mode":"explicit","actions":[{"id":"`+p.ID+`","do":"dismiss","reason":"批量忽略：邮件"}]}`, e.web)
	var res struct {
		Results []inbound.BatchResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Results) != 1 || !res.Results[0].OK || !strings.HasPrefix(res.Results[0].Batch, "pb_") {
		t.Fatalf("batch = %d %s", rec.Code, rec.Body)
	}
	batch := res.Results[0].Batch
	var out struct {
		Dismissals map[string]inbound.Dismissal `json:"dismissals"`
	}
	rec = e.do("GET", "/v1/items?phase=dropped", "", e.web)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Dismissals[p.ID].Batch != batch || out.Dismissals[p.ID].Reason != "批量忽略：邮件" || len(out.Dismissals) != 1 {
		t.Fatalf("dismissals = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items?phase=open", "", e.web); strings.Contains(rec.Body.String(), "dismissals") {
		t.Fatalf("dismissals only with phase=dropped: %s", rec.Body)
	}
	rec = e.do("POST", "/v1/proposals/batch", `{"mode":"undo","batch":"`+batch+`"}`, e.web)
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Results) != 1 || !res.Results[0].OK {
		t.Fatalf("undo = %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/items?phase=proposed", "", e.web); !strings.Contains(rec.Body.String(), p.ID) {
		t.Fatalf("back in proposals: %s", rec.Body)
	}
}
