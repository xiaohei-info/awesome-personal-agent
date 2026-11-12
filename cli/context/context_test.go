package context_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apicontext "github.com/xiaohei-info/awesome-personal-agent/api/context"
	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apiprojects "github.com/xiaohei-info/awesome-personal-agent/api/projects"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/context"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/items"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/background"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	kdelivery "github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var bg = context.Background()

// kernel 是真实内核（真实存储、命令管线、授权与路由）经 httptest 暴露；CLI 经真实 HTTP 调它。
type kernel struct {
	t   *testing.T
	st  *store.Store
	au  *auth.Auth
	url string
	tok string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, kp.Module, clock.Module, kitems.Module, kdelivery.Module, kcontext.Module, proc.Module, background.Module, core.Module} {
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
	if err := kp.Register(x); err != nil {
		t.Fatal(err)
	}
	au := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{apiitems.Routes(x, st), apiprojects.Routes(st, x), apicontext.Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	k := &kernel{t: t, st: st, au: au, url: ts.URL}
	k.tok = k.token(auth.Delegate)
	return k
}

func (k *kernel) token(kind auth.Kind) string {
	k.t.Helper()
	var secret string
	if err := k.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := k.au.Issue(tx, auth.Spec{Kind: kind})
		secret = s
		return err
	}); err != nil {
		k.t.Fatal(err)
	}
	return secret
}

type result struct {
	code        int
	out, errOut string
}

func (k *kernel) apaTok(token string, vars map[string]string, args ...string) result {
	k.t.Helper()
	v := map[string]string{"APA_URL": k.url, "APA_TOKEN": token}
	for a, b := range vars {
		v[a] = b
	}
	var out, errb bytes.Buffer
	env := client.Env{Getenv: func(s string) string { return v[s] }, UserHome: k.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient}
	return result{client.Main(append([]string{"--evidence", "owner: go ahead"}, args...), env), out.String(), errb.String()}
}

func (k *kernel) apa(args ...string) result { return k.apaTok(k.tok, nil, args...) }

func (k *kernel) must(args ...string) result {
	k.t.Helper()
	r := k.apa(args...)
	if r.code != 0 {
		k.t.Fatalf("%v: %+v", args, r)
	}
	return r
}

// mustAsOwner 建项目、加 repos 是替本人做决定的第二档：除了本人原话（apaTok 已带）还要会话引用。
// 不放进 apa：APA_SESSION 会让 context 记游标，改变那些测试要看的完整投影。
func (k *kernel) mustAsOwner(args ...string) result {
	k.t.Helper()
	r := k.apaTok(k.tok, map[string]string{"APA_SESSION": "setup", "APA_SESSION_KIND": "direct"}, args...)
	if r.code != 0 {
		k.t.Fatalf("%v: %+v", args, r)
	}
	return r
}

func (k *kernel) project(name, host, dir string) string {
	k.t.Helper()
	var p domain.Project
	if err := json.Unmarshal([]byte(k.mustAsOwner("project", "create", "--name", name).out), &p); err != nil {
		k.t.Fatal(err)
	}
	if dir != "" {
		remote := "https://example.com/" + name + ".git"
		k.mustAsOwner("project", "repos", "add", p.ID, "--remote", remote)
		k.mustAsOwner("project", "repos", "add", p.ID, "--remote", remote, "--host", host, "--path", dir)
	}
	return p.ID
}

func (k *kernel) item(title, projectID string) string {
	k.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", title, "--reason", "r", "--evidence", "owner: add it"}
	if projectID != "" {
		args = append(args, "--project", projectID)
	}
	var it struct{ ID string }
	if err := json.Unmarshal([]byte(k.must(args...).out), &it); err != nil || it.ID == "" {
		k.t.Fatalf("item add: %v", err)
	}
	return it.ID
}

type ctxOut struct {
	Cursor  int64 `json:"cursor"`
	Project *struct {
		ID string `json:"id"`
	} `json:"project"`
	Items []struct{ ID, Title string } `json:"items"`
}

func (k *kernel) ctx(vars map[string]string, args ...string) (ctxOut, result) {
	k.t.Helper()
	r := k.apaTok(k.tok, vars, append([]string{"context"}, args...)...)
	var o ctxOut
	if r.code == 0 {
		if err := json.Unmarshal([]byte(r.out), &o); err != nil {
			k.t.Fatalf("not context JSON: %q", r.out)
		}
	}
	return o, r
}

func ids(o ctxOut) map[string]bool {
	m := map[string]bool{}
	for _, i := range o.Items {
		m[i.ID] = true
	}
	return m
}

// ACT-HCTX-05：同一会话第二次读取只返回增量；--session 覆盖 APA_SESSION；不带会话每次都是完整投影。
func TestContextSessionIncrementalIsKernelOwned(t *testing.T) {
	k := newKernel(t)
	a := k.item("first", "")
	o, r := k.ctx(nil, "--session", "s1")
	if r.code != 0 || !ids(o)[a] {
		t.Fatalf("first read must include item: %+v", r)
	}
	if o, _ = k.ctx(nil, "--session", "s1"); len(o.Items) != 0 {
		t.Fatalf("second read of same session must be empty: %+v", o)
	}
	b := k.item("second", "")
	if o, _ = k.ctx(nil, "--session", "s1"); len(o.Items) != 1 || !ids(o)[b] {
		t.Fatalf("only the new item expected: %+v", o)
	}
	// APA_SESSION 与 --session 是同一会话。
	k.item("third", "")
	if o, _ = k.ctx(map[string]string{"APA_SESSION": "s1"}); len(o.Items) != 1 {
		t.Fatalf("env session must continue s1: %+v", o)
	}
	// 另一个会话从头读；无会话总是完整投影。
	if o, _ = k.ctx(nil, "--session", "s2"); len(o.Items) != 3 {
		t.Fatalf("new session must see everything: %+v", o)
	}
	n1, _ := k.ctx(nil)
	n2, _ := k.ctx(nil)
	if len(n1.Items) < 3 || len(n2.Items) != len(n1.Items) {
		t.Fatalf("sessionless reads must be full and repeatable: %d %d", len(n1.Items), len(n2.Items))
	}
}

// format=inject：stdout 是内核渲染的纯文本，不是 JSON。
func TestContextFormatInjectIsPlainText(t *testing.T) {
	k := newKernel(t)
	k.item("write the report", "")
	r := k.apa("context", "--format", "inject")
	if r.code != 0 || !strings.Contains(r.out, "write the report") || json.Valid([]byte(r.out)) {
		t.Fatalf("inject must be rendered text: %+v", r)
	}
	if r := k.apa("context", "--format", "xml"); r.code != 2 {
		t.Fatalf("bad format must exit 2: %+v", r)
	}
}

// ACT-MAP-01：显式 --project 优先于 APA_PROJECT 与当前目录；不存在的项目报 5，不回退。
func TestContextExplicitProjectWinsAndUnknownFails(t *testing.T) {
	k := newKernel(t)
	pa, pb := k.project("alpha", "box", "/w/alpha"), k.project("beta", "box", "")
	ia, ib := k.item("in alpha", pa), k.item("in beta", pb)
	o, r := k.ctx(map[string]string{"APA_PROJECT": pb}, "--project", pa, "--path", "/w/beta", "--host", "box")
	if r.code != 0 || o.Project == nil || o.Project.ID != pa || !ids(o)[ia] || ids(o)[ib] {
		t.Fatalf("--project must win: %+v", r)
	}
	// ACT-MAP-02：APA_PROJECT 次于 --project，优于目录。
	o, r = k.ctx(map[string]string{"APA_PROJECT": pb}, "--path", "/w/alpha", "--host", "box")
	if r.code != 0 || o.Project == nil || o.Project.ID != pb || !ids(o)[ib] || ids(o)[ia] {
		t.Fatalf("APA_PROJECT must win over cwd: %+v", r)
	}
	if _, r = k.ctx(nil, "--project", "nope"); r.code != 5 {
		t.Fatalf("unknown project must exit 5: %+v", r)
	}
}

// ACT-MAP-03：目录落在本机检出内才映射到项目（最长前缀）；别的主机、兄弟目录、无匹配都不猜。
func TestContextDirectoryMappingFollowsCheckoutsAndNeverGuesses(t *testing.T) {
	k := newKernel(t)
	pa := k.project("alpha", "box", "/w/alpha")
	pn := k.project("nested", "box", "/w/alpha/svc")
	ia, in := k.item("in alpha", pa), k.item("in nested", pn)
	free := k.item("no project", "")

	o, r := k.ctx(nil, "--path", "/w/alpha/docs", "--host", "box")
	if r.code != 0 || o.Project == nil || o.Project.ID != pa || !ids(o)[ia] || ids(o)[free] {
		t.Fatalf("inside checkout: %+v", r)
	}
	if o, _ = k.ctx(nil, "--path", "/w/alpha/svc/pkg", "--host", "box"); o.Project == nil || o.Project.ID != pn || !ids(o)[in] {
		t.Fatalf("longest prefix must win: %+v", o)
	}
	for _, c := range [][]string{{"/w/alpha2", "box"}, {"/w/alpha", "other-host"}, {"/elsewhere", "box"}} {
		o, r = k.ctx(nil, "--path", c[0], "--host", c[1])
		if r.code != 0 || o.Project != nil || ids(o)[ia] || ids(o)[in] || !ids(o)[free] {
			t.Fatalf("%v must not map to any project: %+v", c, r)
		}
	}
	if _, r = k.ctx(nil, "--path", "relative/dir", "--host", "box"); r.code != 2 {
		t.Fatalf("relative path must exit 2: %+v", r)
	}
}

// ACT-MAP-04：同一最长前缀属于两个项目是冲突（6），要求显式 --project；--scope all 才是跨项目视图。
func TestContextAmbiguousDirectoryConflictsAndScopeAll(t *testing.T) {
	k := newKernel(t)
	p1, p2 := k.project("one", "box", "/w/dup"), k.project("two", "box", "/w/dup")
	i1, i2 := k.item("one item", p1), k.item("two item", p2)
	if _, r := k.ctx(nil, "--path", "/w/dup/x", "--host", "box"); r.code != 6 {
		t.Fatalf("ambiguous mapping must exit 6: %+v", r)
	}
	if o, r := k.ctx(nil, "--path", "/w/dup/x", "--host", "box", "--project", p1); r.code != 0 || !ids(o)[i1] || ids(o)[i2] {
		t.Fatalf("--project resolves the ambiguity: %+v", r)
	}
	if o, r := k.ctx(nil, "--scope", "all", "--project", p1); r.code != 0 || !ids(o)[i1] {
		t.Fatalf("scope all: %+v", r)
	}
	if _, r := k.ctx(nil, "--scope", "bogus"); r.code != 2 {
		t.Fatalf("unsupported scope must exit 2: %+v", r)
	}
}

// 默认取进程当前目录与主机名：测试进程的目录不在任何检出里，所以无项目作用域，且不出错。
func TestContextDefaultsToProcessCwd(t *testing.T) {
	k := newKernel(t)
	free := k.item("free", "")
	k.item("owned", k.project("alpha", "box", "/w/alpha"))
	o, r := k.ctx(nil)
	if r.code != 0 || o.Project != nil || len(o.Items) != 1 || !ids(o)[free] {
		t.Fatalf("unmapped cwd means project-less items only: %+v", r)
	}
}

func TestPersona(t *testing.T) {
	k := newKernel(t)
	if r := k.apa("persona"); r.code != 0 || r.out != "" {
		t.Fatalf("unset persona must print nothing: %+v", r)
	}
	actor := domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	if err := k.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, actor, "test", func(p *policy.Policy) error { p.Persona = "warm and brief"; return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := k.apa("persona")
	if r.code != 0 || !strings.Contains(r.out, "warm and brief") {
		t.Fatalf("persona text: %+v", r)
	}
	if r := k.apa("persona", "extra"); r.code != 2 {
		t.Fatalf("args must exit 2: %+v", r)
	}
	// 凭据种类不够时由内核拒绝：ext 不能读账本。
	var ext string
	if err := k.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := k.au.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "x"})
		ext = s
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := k.apaTok(ext, nil, "persona"); r.code != 4 {
		t.Fatalf("ext credential must exit 4: %+v", r)
	}
}

func TestContextUnreachableKernelExits7(t *testing.T) {
	k := newKernel(t)
	r := k.apaTok(k.tok, map[string]string{"APA_URL": "http://127.0.0.1:1"}, "context")
	if r.code != 7 {
		t.Fatalf("unreachable must exit 7: %+v", r)
	}
}
