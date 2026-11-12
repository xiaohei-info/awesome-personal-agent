package purge

import (
	"context"
	"encoding/json"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	kpurge "github.com/xiaohei-info/awesome-personal-agent/kernel/purge"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const (
	ownerPW = "correct horse battery"
	marker  = "MARKER-PURGE-API"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

type env struct {
	t      *testing.T
	st     *store.Store
	au     *auth.Auth
	x      *command.Executor
	srv    *core.Server
	blob   string
	prj    string
	cookie *http.Cookie
	csrf   string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, materials.Module, proc.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	l := config.LayoutOf(home)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	x := command.New(st, func() time.Time { now = now.Add(time.Minute); return now })
	for _, err := range []error{projects.Register(x), items.Register(x, items.NewKinds()), materials.Register(x), blobs.Register(x, l.Blobs), kpurge.Register(x, l.Blobs)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, st: st, au: auth.New(nil), x: x, blob: l.Blobs}
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	if e.srv, err = core.New(core.Deps{Store: st, Auth: e.au, Groups: []core.RouteGroup{Routes(x)}}); err != nil {
		t.Fatal(err)
	}
	e.prj = e.cmd(owner, "project.create", map[string]any{"name": "P"})["id"].(string)

	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	e.csrf = out.CSRF
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	return e
}

func (e *env) cmd(id command.Identity, name string, in any) map[string]any {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: name, Input: b, Owner: "owner", Evidence: "owner said so"})
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	_ = json.Unmarshal(r.Value, &out)
	return out
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(method, path, body string) *httptest.ResponseRecorder {
	return e.do(method, path, body, func(r *http.Request) {
		r.AddCookie(e.cookie)
		r.Header.Set(core.CSRFHeader, e.csrf)
	})
}

func (e *env) token(kind auth.Kind) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		_, secret, err = e.au.Issue(tx, auth.Spec{Kind: kind})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func code(t *testing.T, r *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(r.Body.Bytes(), &de); err != nil {
		t.Fatalf("not an error body: %d %s", r.Code, r.Body)
	}
	return de.Code
}

func (e *env) events() []eventlog.Entry {
	e.t.Helper()
	var out []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = eventlog.Read(tx, 0, 10000); return }); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) item() string {
	return e.cmd(owner, "item.create", map[string]any{"title": marker, "kind": "task", "reason": marker, "evidence": marker})["id"].(string)
}

func (e *env) itemExists(id string) bool {
	var err error
	_ = e.st.View(bg, func(tx *store.Tx) error { _, err = items.Get(tx, id); return nil })
	return err == nil
}

// ACT-PRG-04：Web 会话经 POST /settings/purge 物理删除事项；投影消失，Event 内容成墓碑，另记删除事件，响应只有机制字段。
func TestWebSessionPurgesItem(t *testing.T) {
	e := setup(t)
	id := e.item()

	r := e.web("POST", "/v1/settings/purge", `{"object":"`+id+`"}`)
	if r.Code != 200 {
		t.Fatalf("purge: %d %s", r.Code, r.Body)
	}
	var res kpurge.Result
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.Object != id || res.Kind != kpurge.KindItem || res.Events < 1 {
		t.Fatalf("result %s (%v)", r.Body, err)
	}
	if strings.Contains(r.Body.String(), marker) {
		t.Fatalf("response leaks ledger content: %s", r.Body)
	}
	if e.itemExists(id) {
		t.Fatal("item projection survived")
	}
	var purged int
	for _, en := range e.events() {
		b, _ := json.Marshal(en.Event)
		if en.Event.Object != id {
			continue
		}
		if en.Event.Type == kpurge.EvPurged {
			purged++
			continue
		}
		if !en.Tombstone || strings.Contains(string(b), marker) {
			t.Fatalf("event not tombstoned: %s", b)
		}
	}
	if purged != 1 {
		t.Fatalf("want one purge event, got %d", purged)
	}
}

// 材料经同一路由删除；独占 blob 文件随之删除。
func TestWebSessionPurgesMaterialAndBlob(t *testing.T) {
	e := setup(t)
	h := e.cmd(owner, "blob.put", map[string]any{"content": []byte(marker)})["hash"].(string)
	m := e.cmd(owner, "material.commit", map[string]any{"project": e.prj, "kind": "minutes", "title": "n", "content": marker + " " + blobs.Ref(h), "reason": "r"})["material"].(string)

	r := e.web("POST", "/v1/settings/purge", `{"object":"`+m+`"}`)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"kind":"material"`) {
		t.Fatalf("purge: %d %s", r.Code, r.Body)
	}
	if _, err := os.Stat(filepath.Join(e.blob, h)); err == nil {
		t.Fatal("unshared blob file survived")
	}
}

// 非 Web 入口：delegate 得 web_only，worker/observer 得 permission_denied，对象与事件原样；未登录 401。
func TestNonWebCredentialsCannotPurge(t *testing.T) {
	e := setup(t)
	id := e.item()
	before := len(e.events())
	body := `{"object":"` + id + `"}`

	for kind, want := range map[auth.Kind]domain.Code{auth.Delegate: domain.CodeWebOnly, auth.Worker: domain.CodePermissionDenied, auth.Observer: domain.CodePermissionDenied} {
		r := e.do("POST", "/v1/settings/purge", body, e.token(kind))
		if r.Code != 403 || code(t, r) != want {
			t.Fatalf("%s: %d %s, want 403 %s", kind, r.Code, r.Body, want)
		}
	}
	if r := e.do("POST", "/v1/settings/purge", body, nil); r.Code != 401 {
		t.Fatalf("anonymous: %d %s", r.Code, r.Body)
	}
	// Web 会话缺 CSRF 令牌同样被骨干拒绝。
	if r := e.do("POST", "/v1/settings/purge", body, func(r *http.Request) { r.AddCookie(e.cookie) }); r.Code != 403 {
		t.Fatalf("no CSRF: %d %s", r.Code, r.Body)
	}
	if !e.itemExists(id) {
		t.Fatal("rejected request deleted the item")
	}
	for _, en := range e.events()[before-1:] {
		if en.Event.Type == kpurge.EvPurged || en.Tombstone {
			t.Fatalf("rejected request left a purge trace in the ledger: %+v", en)
		}
	}
}

// 失败语义：未知对象 404、空/多余字段/坏 JSON 400；都不改账本。
func TestPurgeBadRequests(t *testing.T) {
	e := setup(t)
	id := e.item()
	for body, want := range map[string]int{
		`{"object":"nope"}`:                 404,
		`{"object":""}`:                     400,
		`{}`:                                400,
		`{"object":"` + id + `","force":1}`: 400,
		`not json`:                          400,
	} {
		if r := e.web("POST", "/v1/settings/purge", body); r.Code != want {
			t.Fatalf("%s: %d %s, want %d", body, r.Code, r.Body, want)
		}
	}
	if !e.itemExists(id) {
		t.Fatal("bad request deleted the item")
	}
	// 只开放 POST。
	if r := e.web("GET", "/v1/settings/purge", ""); r.Code == 200 {
		t.Fatalf("GET must not be served: %d", r.Code)
	}
}
