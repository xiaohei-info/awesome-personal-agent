package blobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

var owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

type env struct {
	x   *command.Executor
	st  *store.Store
	au  *auth.Auth
	dir string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module} {
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
	x := command.New(st, nil)
	if err := Register(x, l.Blobs); err != nil {
		t.Fatal(err)
	}
	return &env{x: x, st: st, au: auth.New(nil), dir: l.Blobs}
}

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func (e *env) put(t *testing.T, id command.Identity, content string) (PutResult, error) {
	t.Helper()
	res, err := e.x.Do(bg, command.Request{Identity: id, Command: "blob.put", Input: js(map[string]any{"content": []byte(content)})})
	if err != nil {
		return PutResult{}, err
	}
	var r PutResult
	if err := json.Unmarshal(res.Value, &r); err != nil {
		t.Fatal(err)
	}
	return r, nil
}

func (e *env) get(t *testing.T, id command.Identity, hash string) (GetResult, error) {
	t.Helper()
	res, err := e.x.Do(bg, command.Request{Identity: id, Command: "blob.get", Input: js(map[string]any{"hash": hash})})
	if err != nil {
		return GetResult{}, err
	}
	var r GetResult
	if err := json.Unmarshal(res.Value, &r); err != nil {
		t.Fatal(err)
	}
	return r, nil
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, f := range es {
		n = append(n, f.Name())
	}
	return n
}

func TestPutThenGetRoundTripsByContentHash(t *testing.T) {
	e := setup(t)
	content := "附件\x00正文\xff"
	sum := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(sum[:])

	p, err := e.put(t, owner, content)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hash != want || p.Ref != "apa://blob/"+want || p.Size != int64(len(content)) || !p.Created {
		t.Fatalf("put result %+v", p)
	}
	g, err := e.get(t, owner, p.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if string(g.Content) != content || g.Hash != want || g.Size != int64(len(content)) {
		t.Fatalf("get result %+v", g)
	}
}

func TestSameContentStoredOnce(t *testing.T) {
	e := setup(t)
	a, _ := e.put(t, owner, "same")
	b, err := e.put(t, owner, "same")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash != b.Hash || !a.Created || b.Created {
		t.Fatalf("first %+v second %+v", a, b)
	}
	if fs := files(t, e.dir); len(fs) != 1 || fs[0] != a.Hash {
		t.Fatalf("want exactly one blob file, got %v", fs)
	}
	if _, err := e.put(t, owner, "other"); err != nil {
		t.Fatal(err)
	}
	if fs := files(t, e.dir); len(fs) != 2 {
		t.Fatalf("different content is a different blob: %v", fs)
	}
}

func TestEmptyContentIsABlob(t *testing.T) {
	e := setup(t)
	p, err := e.put(t, owner, "")
	if err != nil || p.Size != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	if g, err := e.get(t, owner, p.Hash); err != nil || len(g.Content) != 0 {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestGetFailureSemantics(t *testing.T) {
	e := setup(t)
	missing := strings.Repeat("a", 64)
	if _, err := e.get(t, owner, missing); exit(err) != domain.ExitNotFound {
		t.Fatalf("missing blob: %v", err)
	}
	for _, h := range []string{"", "abc", "../ledger.db", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 65)} {
		if _, err := e.get(t, owner, h); exit(err) != domain.ExitUsage {
			t.Fatalf("hash %q must be rejected as invalid request: %v", h, err)
		}
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	e := setup(t)
	p, _ := e.put(t, owner, "good")
	if err := os.WriteFile(filepath.Join(e.dir, p.Hash), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.get(t, owner, p.Hash); exit(err) != domain.ExitUnclassified {
		t.Fatalf("corrupt blob must surface as internal error, not data: %v", err)
	}
}

func TestPutLeavesNoTempFilesAndSurvivesReopen(t *testing.T) {
	e := setup(t)
	p, _ := e.put(t, owner, "persist")
	for _, n := range files(t, e.dir) {
		if n != p.Hash {
			t.Fatalf("stray file %q", n)
		}
	}
	// 新进程：重新登记到另一个 Executor，仍可读到。
	x2 := command.New(e.st, nil)
	if err := Register(x2, e.dir); err != nil {
		t.Fatal(err)
	}
	res, err := x2.Do(bg, command.Request{Identity: owner, Command: "blob.get", Input: js(map[string]any{"hash": p.Hash})})
	if err != nil || !strings.Contains(string(res.Value), p.Hash) {
		t.Fatalf("%s %v", res.Value, err)
	}
}

func TestObserverCannotPutButCanGet(t *testing.T) {
	e := setup(t)
	p, _ := e.put(t, owner, "x")
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		_, secret, err = e.au.Issue(tx, auth.Spec{Kind: auth.Observer, Label: "o"})
		return
	}); err != nil {
		t.Fatal(err)
	}
	obs := command.Bearer{Auth: e.au, Secret: secret}
	if _, err := e.put(t, obs, "y"); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("observer put: %v", err)
	}
	if fs := files(t, e.dir); len(fs) != 1 {
		t.Fatalf("denied put must not store: %v", fs)
	}
	if _, err := e.get(t, obs, p.Hash); err != nil {
		t.Fatalf("observer get: %v", err)
	}
	if _, err := e.put(t, command.Bearer{Auth: e.au, Secret: "bogus"}, "z"); exit(err) != domain.ExitUnauthenticated {
		t.Fatalf("bad credential: %v", err)
	}
}

func TestRefParsing(t *testing.T) {
	h := strings.Repeat("0", 64)
	if got, err := ParseRef("apa://blob/" + h); err != nil || got != h {
		t.Fatalf("%q %v", got, err)
	}
	for _, s := range []string{"", h, "apa://blob/", "apa://blob/xyz", "http://blob/" + h, "apa://blob/" + h + "/x"} {
		if _, err := ParseRef(s); err == nil {
			t.Fatalf("%q must be rejected", s)
		}
	}
}

func TestRegisterTwiceFails(t *testing.T) {
	e := setup(t)
	if err := Register(e.x, e.dir); err == nil {
		t.Fatal("duplicate registration must fail")
	}
}

func TestPutFailsWhenDirUnwritable(t *testing.T) {
	e := setup(t)
	if err := os.RemoveAll(e.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := e.put(t, owner, "x"); exit(err) != domain.ExitUnclassified {
		t.Fatalf("real I/O error must surface: %v", err)
	}
}
