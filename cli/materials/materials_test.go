package materials_test

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

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apimat "github.com/xiaohei-info/awesome-personal-agent/api/materials"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	km "github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

// 真实账本 + 命令管线 + 材料 HTTP 路由；CLI 经真实 HTTP 调用。
type kernel struct {
	t      *testing.T
	url    string
	tokens map[auth.Kind]string
	prj    string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, km.Module, search.Module, hosted.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, nil)
	for _, err := range []error{projects.Register(x), items.Register(x, items.NewKinds()), km.Register(x), hosted.Register(x), search.Register(x), blobs.Register(x, t.TempDir())} {
		if err != nil {
			t.Fatal(err)
		}
	}
	au := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{apimat.Routes(st, x, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	k := &kernel{t: t, url: ts.URL, tokens: map[auth.Kind]string{}}
	for _, kind := range []auth.Kind{auth.Observer, auth.Worker} {
		if err := st.Update(context.Background(), func(tx *store.Tx) error {
			_, s, err := au.Issue(tx, auth.Spec{Kind: kind})
			k.tokens[kind] = s
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	owner := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	res, err := x.Do(context.Background(), command.Request{Identity: owner, Command: "project.create", Input: []byte(`{"name":"Work","intro":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var pr struct{ ID string }
	_ = json.Unmarshal(res.Value, &pr)
	k.prj = pr.ID
	return k
}

type run struct {
	code        int
	out, errOut string
}

func (r run) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.out), &m); err != nil {
		t.Fatalf("stdout is not JSON: %q stderr=%q", r.out, r.errOut)
	}
	return m
}

func (k *kernel) apa(kind auth.Kind, args ...string) run {
	k.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": k.tokens[kind], "APA_HOME": k.t.TempDir()}
	code := client.Main(args, client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: k.t.TempDir(),
		Stdin: strings.NewReader("from stdin body"), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return run{code, out.String(), errb.String()}
}

func (k *kernel) ok(kind auth.Kind, args ...string) run {
	k.t.Helper()
	r := k.apa(kind, args...)
	if r.code != 0 {
		k.t.Fatalf("apa %v: exit %d stderr=%s", args, r.code, r.errOut)
	}
	return r
}

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.md")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// flow-material-owned：新建自有型、追加修订、读回当前与指定修订、列表与检索。
func TestOwnedMaterialLifecycle(t *testing.T) {
	k := newKernel(t)
	r1 := k.ok(auth.Worker, "material", "commit", "--from", write(t, "alpha policy v1"), "--reason", "first", "--kind", "reference", "--title", "Policy", "--project", k.prj, "--tag", "ops").json(t)
	id, rev1 := r1["material"].(string), r1["revision"].(string)
	if id == "" || rev1 == "" {
		t.Fatalf("commit result: %v", r1)
	}
	k.ok(auth.Worker, "material", "commit", id, "--from", "-", "--reason", "second", "--based-on", rev1)
	if r := k.apa(auth.Worker, "material", "commit", id, "--from", write(t, "late"), "--reason", "stale", "--based-on", rev1); r.code != domain.ExitConflict {
		t.Fatalf("stale base must exit 6: %d %s", r.code, r.errOut)
	}

	cur := k.ok(auth.Observer, "material", "show", id).json(t)
	if cur["revision"].(map[string]any)["content"] != "from stdin body" {
		t.Fatalf("current revision: %v", cur)
	}
	old := k.ok(auth.Observer, "material", "show", id, "--rev", rev1).json(t)
	if old["revision"].(map[string]any)["content"] != "alpha policy v1" {
		t.Fatalf("rev %s: %v", rev1, old)
	}
	if l := k.ok(auth.Observer, "material", "list", "--project", k.prj).json(t)["materials"].([]any); len(l) != 1 {
		t.Fatalf("list: %v", l)
	}
	if l := k.ok(auth.Observer, "material", "list", "--project", "prj_none").json(t)["materials"].([]any); len(l) != 0 {
		t.Fatalf("list other project: %v", l)
	}
	if h := k.ok(auth.Observer, "material", "search", "stdin", "--project", k.prj, "--limit", "5").json(t)["hits"].([]any); len(h) == 0 {
		t.Fatal("search found nothing")
	}
	if k.apa(auth.Observer, "material", "show", "mat_missing").code != domain.ExitNotFound {
		t.Fatal("missing material must exit 5")
	}
}

// 授权由内核判定：observer 提交返回 4。
func TestObserverCannotCommit(t *testing.T) {
	k := newKernel(t)
	if r := k.apa(auth.Observer, "material", "commit", "--from", write(t, "x"), "--reason", "r", "--kind", "reference", "--title", "T"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer commit: %d %s", r.code, r.errOut)
	}
}

// flow-material-hosted：登记托管型只回位置，不带正文；对托管型 commit 返回 6。
func TestHostedMaterialAddShowAndCommitConflict(t *testing.T) {
	k := newKernel(t)
	r := k.ok(auth.Worker, "material", "add", "--repo", "git@example.com:me/doc.git", "--path", "docs/design.md", "--project", k.prj, "--kind", "design", "--title", "Design").json(t)
	id := r["material"].(string)
	show := k.ok(auth.Observer, "material", "show", id)
	if strings.Contains(show.out, "content") && show.json(t)["revision"] != nil {
		t.Fatalf("hosted show must not carry a body: %s", show.out)
	}
	if h := show.json(t)["hosted"].(map[string]any); h["path"] != "docs/design.md" {
		t.Fatalf("hosted location: %v", h)
	}
	if c := k.apa(auth.Worker, "material", "commit", id, "--from", write(t, "x"), "--reason", "r"); c.code != domain.ExitConflict {
		t.Fatalf("commit on hosted: %d %s", c.code, c.errOut)
	}
	if a := k.apa(auth.Worker, "material", "add", "--repo", "r"); a.code != domain.ExitUsage {
		t.Fatalf("add without --path: %d", a.code)
	}
}

// 附件按内容哈希存取，字节原样往返。
func TestBlobPutGetRoundTrip(t *testing.T) {
	k := newKernel(t)
	data := []byte{0, 1, 2, 0xff, '\n', 'x'}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	p := filepath.Join(t.TempDir(), "a.bin")
	_ = os.WriteFile(p, data, 0o600)
	if put := k.ok(auth.Worker, "blob", "put", p).json(t); put["hash"] != hash {
		t.Fatalf("put: %v", put)
	}
	if got := k.ok(auth.Observer, "blob", "get", hash); got.out != string(data) {
		t.Fatalf("get stdout: %q", got.out)
	}
	out := filepath.Join(t.TempDir(), "o.bin")
	k.ok(auth.Observer, "blob", "get", hash, "--out", out)
	if b, _ := os.ReadFile(out); !bytes.Equal(b, data) {
		t.Fatalf("get --out: %q", b)
	}
	if k.apa(auth.Observer, "blob", "put", p).code != domain.ExitPermissionDenied {
		t.Fatal("observer blob put must exit 4")
	}
	if k.apa(auth.Observer, "blob", "get", strings.Repeat("0", 64)).code != domain.ExitNotFound {
		t.Fatal("unknown blob must exit 5")
	}
}

func TestUsageErrors(t *testing.T) {
	k := newKernel(t)
	for _, a := range [][]string{{"material"}, {"material", "bogus"}, {"material", "list", "x"}, {"material", "show"}, {"material", "search"},
		{"material", "commit", "--reason", "r"}, {"material", "commit", "m1", "--from", "/nonexistent/f", "--reason", "r"},
		{"blob"}, {"blob", "put"}, {"blob", "get"}, {"blob", "zap"}} {
		if r := k.apa(auth.Observer, a...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %d", a, r.code)
		}
	}
}
