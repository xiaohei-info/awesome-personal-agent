package materials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	km "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

type env struct {
	t    *testing.T
	st   *store.Store
	au   *auth.Auth
	srv  *core.Server
	sec  map[string]string
	web  *http.Cookie
	csrf string
	prj  string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, executors.Module, km.Module, search.Module, proc.Module, delegation.Module, hosted.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	blobDir := t.TempDir()
	x := command.New(st, nil)
	for _, err := range []error{
		projects.Register(x), items.Register(x, items.NewKinds()), executors.Register(x, executors.Options{Owner: "owner"}),
		km.Register(x), hosted.Register(x), blobs.Register(x, blobDir),
		delegation.Register(x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Error(err) }}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	// 读取器是外部子进程边界：用一个固定输出的脚本替身，其余全部是真实内核。
	reader := filepath.Join(t.TempDir(), "reader.sh")
	if err := os.WriteFile(reader, []byte("#!/bin/sh\necho '{\"commit\":\"c0ffee\",\"branch\":\"main\",\"content\":\"# hosted doc\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var d *proc.Dispatcher
	hs, err := hosted.New(x, hosted.Options{Reader: []string{reader}, Host: "apa-host", Owner: "owner",
		Kick: func() { d.Dispatch(context.Background()) }, OnError: func(_ string, err error) { t.Error(err) }})
	if err != nil {
		t.Fatal(err)
	}
	d = proc.New(st, config.Default(), proc.Options{OnResult: hs.OnResult})
	t.Cleanup(d.Close)

	au := auth.New(nil)
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{Routes(st, x, hs)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, au: au, srv: srv, sec: map[string]string{}}
	for name, s := range map[string]auth.Spec{"obs": {Kind: auth.Observer}, "wrk": {Kind: auth.Worker}, "dlg": {Kind: auth.Delegate},
		"ext": {Kind: auth.Ext, Scope: "src_1"}, "export": {Kind: auth.Ext, Scope: "export"}} {
		if err := st.Update(context.Background(), func(tx *store.Tx) error {
			_, secret, err := au.Issue(tx, s)
			e.sec[name] = secret
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec := e.do("", "POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.web = c
		}
	}
	if e.web == nil || out.CSRF == "" {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	e.csrf = out.CSRF
	owner := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	res, err := x.Do(context.Background(), command.Request{Identity: owner, Command: "project.create", Input: []byte(`{"name":"Work","intro":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var pr struct{ ID string }
	json.Unmarshal(res.Value, &pr)
	e.prj = pr.ID
	for _, in := range []string{`{"id":"other.cli","kind":"external-cli"}`, `{"id":"third.cli","kind":"external-cli"}`} {
		if _, err := x.Do(context.Background(), command.Request{Identity: owner, Command: "executor.put", Input: []byte(in)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.Do(context.Background(), command.Request{Identity: owner, Command: "project.allowed_executors.set",
		Input: []byte(`{"id":"` + e.prj + `","executors":["other.cli"]}`)}); err != nil {
		t.Fatal(err)
	}
	return e
}

// do 以 who 的身份发请求：空为匿名，"web" 为本人的 Web 会话，其余是 e.sec 里的 Bearer。
func (e *env) do(who, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.doBytes(who, method, path, []byte(body), hdr...)
}

func (e *env) doBytes(who, method, path string, body []byte, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	switch who {
	case "":
	case "web":
		req.AddCookie(e.web)
		req.Header.Set(core.CSRFHeader, e.csrf)
	default:
		req.Header.Set("Authorization", "Bearer "+e.sec[who])
		req.Header.Set("X-APA-Session", "s1")
		req.Header.Set("X-APA-Session-Kind", "direct")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func into(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("not json (%d): %s", rec.Code, rec.Body)
	}
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var m struct{ Code string }
	into(t, rec, &m)
	return m.Code
}

// want 断言状态码；失败时带上响应体。
func want(t *testing.T, rec *httptest.ResponseRecorder, status int, errCode string) {
	t.Helper()
	if rec.Code != status || (errCode != "" && code(t, rec) != errCode) {
		t.Fatalf("want %d %s, got %d %s", status, errCode, rec.Code, rec.Body)
	}
}

func (e *env) project(t *testing.T) string { return e.prj }

type result struct{ Material, Revision, State string }

func (e *env) commit(who, body string) (result, *httptest.ResponseRecorder) {
	e.t.Helper()
	rec := e.do(who, "POST", "/v1/materials", body)
	var r result
	if rec.Code < 300 {
		into(e.t, rec, &r)
	}
	return r, rec
}

// flow-material-owned：自有型材料的创建、修订、基线冲突、回退（只认 Web）、确认与拒绝、规范生效（只认 Web）。
func TestOwnedMaterialLifecycle(t *testing.T) {
	e := setup(t)
	prj := e.project(t)
	body := func(kind, content string) string {
		return `{"project":"` + prj + `","kind":"` + kind + `","title":"Notes","tags":["a"],"content":"` + content + `","reason":"first"}`
	}

	// 写入权限：observer 不能写，也不能读不存在的材料。
	_, rec := e.commit("obs", body("minutes", "v1"))
	want(t, rec, 403, "permission_denied")
	want(t, e.do("obs", "GET", "/v1/materials/mat_nope", ""), 404, "not_found")
	want(t, e.do("", "GET", "/v1/materials", ""), 401, "")

	// minutes 直写：创建即生效；读回当前修订与列表。
	r1, rec := e.commit("wrk", body("minutes", "v1 alpha"))
	want(t, rec, 201, "")
	if r1.State != "current" || r1.Material == "" {
		t.Fatalf("create: %+v", r1)
	}
	type showT struct {
		Material km.Material
		Revision *km.Revision
	}
	var shown showT
	into(t, e.do("obs", "GET", "/v1/materials/"+r1.Material, ""), &shown)
	if shown.Material.CurrentRev != r1.Revision || shown.Revision == nil || shown.Revision.Content != "v1 alpha" {
		t.Fatalf("show: %+v", shown)
	}
	var list struct{ Materials []km.Material }
	into(t, e.do("obs", "GET", "/v1/materials?project="+prj, ""), &list)
	if len(list.Materials) != 1 || list.Materials[0].ID != r1.Material {
		t.Fatalf("list: %+v", list)
	}

	// 追加修订：基线对才生效，旧基线是冲突；id 不能出现在体里。
	rev := "/v1/materials/" + r1.Material + "/revisions"
	r2res := e.do("wrk", "POST", rev, `{"content":"v2 beta","reason":"edit","based_on":"`+r1.Revision+`"}`)
	want(t, r2res, 201, "")
	var r2 result
	into(t, r2res, &r2)
	want(t, e.do("wrk", "POST", rev, `{"content":"v3","reason":"edit","based_on":"`+r1.Revision+`"}`), 409, "conflict")
	want(t, e.do("wrk", "POST", rev, `{"id":"x","content":"v3","reason":"edit"}`), 400, "invalid_request")
	var hist struct{ Revisions []km.Revision }
	into(t, e.do("obs", "GET", rev, ""), &hist)
	if len(hist.Revisions) != 2 || hist.Revisions[0].ID != r1.Revision || hist.Revisions[1].State != "current" || hist.Revisions[0].State != "superseded" {
		t.Fatalf("history: %+v", hist)
	}

	// 回退：只有 Web。worker 得到 permission_denied，delegate 得到 web_only，Web 本人成功并新增一条修订。
	revert := "/v1/materials/" + r1.Material + "/revert"
	revBody := `{"to":"` + r1.Revision + `","reason":"undo"}`
	want(t, e.do("wrk", "POST", revert, revBody), 403, "permission_denied")
	want(t, e.do("dlg", "POST", revert, revBody, core.EvidenceHeader, "owner: undo it"), 403, "web_only")
	rec = e.do("web", "POST", revert, revBody)
	want(t, rec, 200, "")
	into(t, e.do("obs", "GET", rev, ""), &hist)
	if len(hist.Revisions) != 3 || hist.Revisions[2].Content != "v1 alpha" || hist.Revisions[2].State != "current" {
		t.Fatalf("revert must append a revision with the old content: %+v", hist)
	}
	want(t, e.do("web", "POST", revert, `{"to":"`+r2.Revision+`","reason":"x"}`, "Origin", "http://evil.example"), 403, "permission_denied")

	// design 是 proposed：创建后没有当前修订，Web 确认才生效；确认第二次是冲突。
	p1, rec := e.commit("wrk", body("design", "design v1"))
	want(t, rec, 201, "")
	if p1.State != "proposed" {
		t.Fatalf("design must be proposed: %+v", p1)
	}
	shown = showT{}
	into(t, e.do("obs", "GET", "/v1/materials/"+p1.Material, ""), &shown)
	if shown.Revision != nil || shown.Material.CurrentRev != "" {
		t.Fatalf("a proposed-only material has no current revision: %+v", shown)
	}
	confirm := "/v1/revisions/" + p1.Revision + "/confirm"
	want(t, e.do("wrk", "POST", confirm, `{}`), 403, "permission_denied")
	want(t, e.do("dlg", "POST", confirm, `{}`), 403, "basis_required") // 第二档：delegate 必须附本人原话
	want(t, e.do("dlg", "POST", confirm, `{}`, core.EvidenceHeader, "owner: yes, adopt the design"), 200, "")
	want(t, e.do("web", "POST", confirm, `{}`), 409, "conflict")

	// 拒绝：被拒绝的修订不生效，再拒绝是冲突。
	p2res := e.do("wrk", "POST", "/v1/materials/"+p1.Material+"/revisions", `{"content":"design v2","reason":"tweak","based_on":"`+p1.Revision+`"}`)
	want(t, p2res, 201, "")
	var p2 result
	into(t, p2res, &p2)
	want(t, e.do("web", "POST", "/v1/revisions/"+p2.Revision+"/reject", `{"reason":"no"}`), 200, "")
	want(t, e.do("web", "POST", "/v1/revisions/"+p2.Revision+"/reject", `{}`), 409, "conflict")
	into(t, e.do("obs", "GET", "/v1/materials/"+p1.Material, ""), &shown)
	if shown.Revision == nil || shown.Revision.Content != "design v1" {
		t.Fatalf("rejected revision must not become current: %+v", shown)
	}
	want(t, e.do("web", "POST", "/v1/revisions/rev_nope/confirm", `{}`), 404, "not_found")

	// 规范：生效只有 Web，delegate 带着本人原话也得到 web_only。
	g, rec := e.commit("wrk", `{"project":"`+prj+`","kind":"guideline","title":"Rules","content":"be kind","reason":"new"}`)
	want(t, rec, 201, "")
	gc := "/v1/revisions/" + g.Revision + "/confirm"
	want(t, e.do("dlg", "POST", gc, `{}`, core.EvidenceHeader, "owner: activate it"), 403, "web_only")
	want(t, e.do("web", "POST", gc, `{}`), 200, "")
	into(t, e.do("obs", "GET", "/v1/materials/"+g.Material, ""), &shown)
	if shown.Revision == nil || shown.Revision.Content != "be kind" {
		t.Fatalf("guideline not active: %+v", shown)
	}
}

func TestSearchFiltersAndValidation(t *testing.T) {
	e := setup(t)
	prj := e.project(t)
	for title, content := range map[string]string{"Alpha plan": "我们讨论 roadmap 与预算", "Beta log": "nothing relevant"} {
		_, rec := e.commit("wrk", `{"project":"`+prj+`","kind":"minutes","title":"`+title+`","tags":["team"],"content":"`+content+`","reason":"r"}`)
		want(t, rec, 201, "")
	}
	var out struct{ Hits []search.Hit }
	into(t, e.do("obs", "GET", "/v1/materials/search?q=roadmap&kind=minutes&tag=team&project="+prj, ""), &out)
	if len(out.Hits) != 1 || out.Hits[0].Title != "Alpha plan" || !strings.Contains(out.Hits[0].Snippet, "roadmap") {
		t.Fatalf("hits: %+v", out)
	}
	into(t, e.do("obs", "GET", "/v1/materials/search?q=预算", ""), &out)
	if len(out.Hits) != 1 {
		t.Fatalf("CJK query: %+v", out)
	}
	into(t, e.do("obs", "GET", "/v1/materials/search?q=roadmap&tag=missing", ""), &out)
	if out.Hits == nil || len(out.Hits) != 0 {
		t.Fatalf("no match must be an empty list: %+v", out)
	}
	into(t, e.do("obs", "GET", "/v1/materials/search?limit=1", ""), &out)
	if len(out.Hits) != 1 {
		t.Fatalf("limit: %+v", out)
	}
	for _, q := range []string{"limit=0", "limit=x", "after=yesterday", "before=1", "q=%21%21"} {
		want(t, e.do("obs", "GET", "/v1/materials/search?"+q, ""), 400, "invalid_request")
	}
	future := url(time.Now().Add(time.Hour))
	into(t, e.do("obs", "GET", "/v1/materials/search?after="+future, ""), &out)
	if len(out.Hits) != 0 {
		t.Fatalf("after filter: %+v", out)
	}
	// search 是静态路径，不能被 {id} 吞掉。
	want(t, e.do("obs", "GET", "/v1/materials/search?q=roadmap", ""), 200, "")
	want(t, e.do("ext", "GET", "/v1/materials/search?q=roadmap", ""), 403, "permission_denied")
	want(t, e.do("export", "GET", "/v1/materials/search?q=roadmap", ""), 200, "")
	want(t, e.do("export", "GET", "/v1/materials", ""), 200, "")
}

func url(t time.Time) string { return strings.ReplaceAll(t.UTC().Format(time.RFC3339), "+", "%2B") }

func TestBlobs(t *testing.T) {
	e := setup(t)
	data := []byte("\x00binary\xffcontent")
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	path := "/v1/blobs/" + h

	want(t, e.doBytes("obs", "PUT", path, data), 403, "permission_denied")
	want(t, e.doBytes("wrk", "PUT", "/v1/blobs/"+strings.Repeat("0", 64), data), 400, "invalid_request")
	want(t, e.doBytes("wrk", "PUT", "/v1/blobs/short", data), 400, "invalid_request")

	rec := e.doBytes("wrk", "PUT", path, data)
	want(t, rec, 201, "")
	var put blobs.PutResult
	into(t, rec, &put)
	if put.Hash != h || put.Ref != blobs.Ref(h) || put.Size != int64(len(data)) || !put.Created {
		t.Fatalf("put: %+v", put)
	}
	rec = e.doBytes("wrk", "PUT", path, data)
	want(t, rec, 200, "")
	into(t, rec, &put)
	if put.Created {
		t.Fatalf("same content must not be stored twice: %+v", put)
	}

	rec = e.do("obs", "GET", path, "")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), data) || rec.Header().Get("Content-Type") != "application/octet-stream" ||
		rec.Header().Get("Content-Disposition") != "attachment" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("get: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	want(t, e.do("obs", "GET", "/v1/blobs/"+strings.Repeat("a", 64), ""), 404, "not_found")
	want(t, e.do("obs", "GET", "/v1/blobs/NOT-A-HASH", ""), 400, "invalid_request")
	want(t, e.do("ext", "GET", path, ""), 403, "permission_denied")
	want(t, e.do("", "GET", path, ""), 401, "")
}

// flow-material-hosted：登记托管型（只记位置）、不能写修订、正文只给 Web 且异步读取、修改走委派。
func TestHostedMaterial(t *testing.T) {
	e := setup(t)
	prj := e.project(t)
	repo := "git@example.com:o/r.git"
	rec := e.do("wrk", "POST", "/v1/materials", `{"project":"`+prj+`","kind":"design","title":"Design","repo":"`+repo+`","path":"docs/./design.md"}`)
	want(t, rec, 201, "")
	var added struct{ Material string }
	into(t, rec, &added)
	id := added.Material

	var shown struct {
		Material km.Material
		Hosted   hosted.Entry
		Revision *km.Revision
	}
	into(t, e.do("obs", "GET", "/v1/materials/"+id, ""), &shown)
	if shown.Hosted.Repo != repo || shown.Hosted.Path != "docs/design.md" || shown.Material.Location != repo+":docs/design.md" || shown.Revision != nil {
		t.Fatalf("show: %+v", shown)
	}
	var hist struct{ Revisions []km.Revision }
	into(t, e.do("obs", "GET", "/v1/materials/"+id+"/revisions", ""), &hist)
	if hist.Revisions == nil || len(hist.Revisions) != 0 {
		t.Fatalf("hosted material has no revisions: %+v", hist)
	}
	want(t, e.do("wrk", "POST", "/v1/materials/"+id+"/revisions", `{"content":"x","reason":"r"}`), 409, "conflict")
	want(t, e.do("web", "POST", "/v1/materials/"+id+"/revert", `{"to":"rev_x","reason":"r"}`), 409, "conflict")
	want(t, e.do("obs", "POST", "/v1/materials", `{"project":"`+prj+`","kind":"design","repo":"`+repo+`","path":"a.md"}`), 403, "permission_denied")
	want(t, e.do("wrk", "POST", "/v1/materials", `{"project":"`+prj+`","kind":"design","repo":"`+repo+`","path":"/abs.md"}`), 400, "invalid_request")

	// 搜索命中托管型：回路径，不回片段。
	var hits struct{ Hits []search.Hit }
	into(t, e.do("obs", "GET", "/v1/materials/search?kind=design", ""), &hits)
	if len(hits.Hits) != 1 || !hits.Hits[0].Hosted || hits.Hits[0].Path != repo+":docs/design.md" || hits.Hits[0].Snippet != "" {
		t.Fatalf("hits: %+v", hits)
	}

	// 正文只给 Web 登录；读取是异步的：先 reading，轮询到 ready。
	content := "/v1/materials/" + id + "/content"
	want(t, e.do("wrk", "GET", content, ""), 403, "web_only")
	want(t, e.do("dlg", "GET", content, ""), 403, "web_only")
	var got map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec := e.do("web", "GET", content, "")
		want(t, rec, 200, "")
		got = nil
		into(t, rec, &got)
		if got["state"] != "reading" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got["state"] != "ready" || got["content"] != "# hosted doc" || got["commit"] != "c0ffee" || got["repo"] != repo || got["path"] != "docs/design.md" {
		t.Fatalf("content: %+v", got)
	}
	into(t, e.do("obs", "GET", "/v1/materials/"+id, ""), &shown)
	if shown.Hosted.Commit != "c0ffee" {
		t.Fatalf("a successful read must record the latest commit: %+v", shown.Hosted)
	}

	// 修改：生成委派子事项，不改仓库。缺 request、带 id 都是 400；执行者不在项目允许范围内是 403（退出码 21）。
	changes := "/v1/materials/" + id + "/changes"
	want(t, e.do("web", "POST", changes, `{"assignee":"other.cli"}`), 400, "invalid_request")
	want(t, e.do("web", "POST", changes, `{"id":"x","assignee":"other.cli","request":"fix"}`), 400, "invalid_request")
	want(t, e.do("web", "POST", changes, `{"assignee":"third.cli","request":"fix typo"}`), 403, "executor_not_allowed")
	rec = e.do("web", "POST", changes, `{"assignee":"other.cli","request":"fix typo"}`)
	want(t, rec, 201, "")
	var ch struct{ Item, Parent string }
	into(t, rec, &ch)
	if ch.Item == "" || ch.Parent == "" || ch.Item == ch.Parent {
		t.Fatalf("changes: %+v", ch)
	}
	if err := e.st.View(context.Background(), func(tx *store.Tx) error {
		child, err := items.Get(tx, ch.Item)
		if err != nil {
			return err
		}
		if child.Project != prj || child.Assignee != "other.cli" || !strings.Contains(child.Body, "fix typo") || !strings.Contains(child.Body, repo) {
			t.Fatalf("child item: %+v", child)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 材料一侧看得到带类型的关联：修改的父事项以 output 关联材料。
	var linked struct{ Relations []domain.Relation }
	into(t, e.do("obs", "GET", "/v1/materials/"+id, ""), &linked)
	if len(linked.Relations) != 1 || linked.Relations[0] != (domain.Relation{Type: domain.RelOutput, From: ch.Parent, To: id}) {
		t.Fatalf("material relations: %+v", linked.Relations)
	}

	// 请求元数据：同键重试重放首次的父事项与子事项，不再建第二套；delegate 附本人原话、不带 parent 也能发起。
	k1 := e.do("web", "POST", changes, `{"assignee":"other.cli","request":"again"}`, IdempotencyHeader, "k-change")
	k2 := e.do("web", "POST", changes, `{"assignee":"other.cli","request":"again"}`, IdempotencyHeader, "k-change")
	want(t, k1, 201, "")
	want(t, k2, 201, "")
	if k1.Body.String() != k2.Body.String() {
		t.Fatalf("replay must return the first parent and child: %s vs %s", k1.Body, k2.Body)
	}
	want(t, e.do("dlg", "POST", changes, `{"assignee":"other.cli","request":"no words"}`), 403, "basis_required")
	rec = e.do("dlg", "POST", changes, `{"assignee":"other.cli","request":"by delegate"}`, core.EvidenceHeader, "owner: please update the design doc")
	want(t, rec, 201, "")

	// 自有型材料：content 与 changes 都是冲突，让调用者去读修订。
	own, rec := e.commit("wrk", `{"project":"`+prj+`","kind":"minutes","title":"m","content":"x","reason":"r"}`)
	want(t, rec, 201, "")
	want(t, e.do("web", "GET", "/v1/materials/"+own.Material+"/content", ""), 409, "conflict")
	want(t, e.do("web", "POST", "/v1/materials/"+own.Material+"/changes", `{"assignee":"a","request":"r"}`), 409, "conflict")
}

func TestWriteRequestShape(t *testing.T) {
	e := setup(t)
	prj := e.project(t)
	want(t, e.do("wrk", "POST", "/v1/materials", `null`), 400, "invalid_request")
	want(t, e.do("wrk", "POST", "/v1/materials", `{"id":"mat_x","kind":"minutes"}`), 400, "invalid_request")
	want(t, e.do("wrk", "POST", "/v1/materials", `{"project":"`+prj+`","kind":"nope","title":"t","content":"x","reason":"r"}`), 400, "invalid_request")
	want(t, e.do("wrk", "POST", "/v1/materials", `{"project":"`+prj+`","kind":"minutes","title":"t","content":"x","reason":"r","evidence":7}`), 400, "invalid_request")
	want(t, e.do("wrk", "POST", "/v1/materials", `{"project":"`+prj+`","kind":"minutes","title":"t","content":"x"}`), 400, "invalid_request")

	// 幂等：同键同输入重放首次结果（200 而非 201），不重复创建。
	body := `{"project":"` + prj + `","kind":"minutes","title":"t","content":"x","reason":"r"}`
	first := e.do("wrk", "POST", "/v1/materials", body, IdempotencyHeader, "k1")
	want(t, first, 201, "")
	again := e.do("wrk", "POST", "/v1/materials", body, IdempotencyHeader, "k1")
	want(t, again, 200, "")
	if first.Body.String() != again.Body.String() {
		t.Fatalf("replay differs: %s vs %s", first.Body, again.Body)
	}
	var list struct{ Materials []km.Material }
	into(t, e.do("obs", "GET", "/v1/materials", ""), &list)
	if len(list.Materials) != 1 {
		t.Fatalf("replay must not create a second material: %+v", list)
	}
}
