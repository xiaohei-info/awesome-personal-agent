package hostedreader

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture 建一个带 docs/design.md 的远程（裸库）和一份检出。
type fixture struct{ remote, checkout, commit string }

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	remote := filepath.Join(root, "remote.git")
	checkout := filepath.Join(root, "checkout")
	sh(t, root, "init", "-q", "-b", "main", seed)
	os.MkdirAll(filepath.Join(seed, "docs"), 0o755)
	os.WriteFile(filepath.Join(seed, "docs/design.md"), []byte("# committed\n"), 0o644)
	sh(t, seed, "add", ".")
	sh(t, seed, "commit", "-q", "-m", "init")
	sh(t, root, "clone", "-q", "--bare", seed, remote)
	sh(t, root, "clone", "-q", remote, checkout)
	return fixture{remote, checkout, sh(t, seed, "rev-parse", "HEAD")}
}

func call(t *testing.T, in any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(in)
	var out, errb bytes.Buffer
	code := Run(context.Background(), bytes.NewReader(b), &out, &errb)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("stdout 不是单个 JSON: %q", out.String())
	}
	return code, m
}

func req(c Config) map[string]any { return map[string]any{"source_id": "s", "run_id": "r", "config": c} }

func TestReadsDefaultBranchNotWorktree(t *testing.T) {
	f := newFixture(t)
	os.WriteFile(filepath.Join(f.checkout, "docs/design.md"), []byte("# uncommitted\n"), 0o644)
	code, m := call(t, req(Config{Repo: f.remote, Path: "docs/design.md", Checkout: f.checkout}))
	if code != ExitOK || m["content"] != "# committed\n" || m["commit"] != f.commit || m["origin"] != "checkout" || m["branch"] != "main" {
		t.Fatalf("code=%d out=%v", code, m)
	}
}

func TestIgnoresUnpushedLocalCommitOnFeatureBranch(t *testing.T) {
	f := newFixture(t)
	sh(t, f.checkout, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(f.checkout, "docs/design.md"), []byte("# feature\n"), 0o644)
	sh(t, f.checkout, "commit", "-qam", "wip")
	code, m := call(t, req(Config{Repo: f.remote, Path: "docs/design.md", Checkout: f.checkout}))
	if code != ExitOK || m["content"] != "# committed\n" {
		t.Fatalf("code=%d out=%v", code, m)
	}
}

func TestFallsBackToRemoteWithoutCheckout(t *testing.T) {
	f := newFixture(t)
	for _, co := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		code, m := call(t, req(Config{Repo: f.remote, Path: "docs/design.md", Checkout: co}))
		if code != ExitOK || m["content"] != "# committed\n" || m["origin"] != "remote" || m["commit"] != f.commit {
			t.Fatalf("checkout=%q code=%d out=%v", co, code, m)
		}
	}
}

func TestCheckoutOfOtherRepoIsNotUsed(t *testing.T) {
	f := newFixture(t)
	other := newFixture(t)
	os.WriteFile(filepath.Join(other.checkout, "docs/design.md"), []byte("x"), 0o644)
	code, m := call(t, req(Config{Repo: f.remote, Path: "docs/design.md", Checkout: other.checkout}))
	if code != ExitOK || m["origin"] != "remote" {
		t.Fatalf("code=%d out=%v", code, m)
	}
}

func TestUnreadableWhenCheckoutAndRemoteBothGone(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "nope")
	code, m := call(t, req(Config{Repo: gone + ".git", Path: "docs/design.md", Checkout: gone}))
	e, _ := m["error"].(map[string]any)
	if code != ExitUnreadable || e["code"] != CodeUnreadable || e["message"] == "" {
		t.Fatalf("code=%d out=%v", code, m)
	}
	if _, has := m["content"]; has {
		t.Fatal("失败时不得带内容")
	}
}

func TestPathDeletedOnDefaultBranch(t *testing.T) {
	f := newFixture(t)
	for _, co := range []string{f.checkout, ""} {
		code, m := call(t, req(Config{Repo: f.remote, Path: "docs/gone.md", Checkout: co}))
		e, _ := m["error"].(map[string]any)
		if code != ExitPathMissing || e["code"] != CodePathMissing {
			t.Fatalf("checkout=%q code=%d out=%v", co, code, m)
		}
	}
}

func TestBadConfig(t *testing.T) {
	for name, c := range map[string]Config{
		"empty repo":    {Path: "a.md"},
		"option repo":   {Repo: "--upload-pack=x", Path: "a.md"},
		"empty path":    {Repo: "r"},
		"absolute path": {Repo: "r", Path: "/etc/passwd"},
		"escaping path": {Repo: "r", Path: "../x"},
	} {
		if code, m := call(t, req(c)); code != ExitBadConfig || m["error"].(map[string]any)["code"] != CodeBadConfig {
			t.Fatalf("%s: code=%d out=%v", name, code, m)
		}
	}
	var out, errb bytes.Buffer
	if code := Run(context.Background(), strings.NewReader("not json"), &out, &errb); code != ExitBadConfig {
		t.Fatalf("坏 JSON code=%d", code)
	}
}

func TestDirectoryPathIsNotAFile(t *testing.T) {
	f := newFixture(t)
	code, _ := call(t, req(Config{Repo: f.remote, Path: "docs", Checkout: f.checkout}))
	if code != ExitPathMissing {
		t.Fatalf("code=%d", code)
	}
}

// owner/name 简写要读得到：git 不认它，读取器展开成 URL。insteadOf 把默认站点指回本地裸库，不走网络。
func TestOwnerRepoShorthandReadsRemote(t *testing.T) {
	f := newFixture(t)
	site := filepath.Dir(f.remote)
	if err := os.MkdirAll(filepath.Join(site, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh(t, site, "clone", "-q", "--bare", f.remote, filepath.Join(site, "acme", "widgets"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.file://"+site+"/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/")

	code, m := call(t, req(Config{Repo: "acme/widgets", Path: "docs/design.md"}))
	if code != ExitOK || m["origin"] != "remote" || m["content"] != "# committed\n" {
		t.Fatalf("code=%d out=%v", code, m)
	}
}

func TestCloneURL(t *testing.T) {
	for in, want := range map[string]string{
		"acme/widgets":                     "https://github.com/acme/widgets",
		"acme/widgets.git":                 "https://github.com/acme/widgets.git",
		"https://example.com/acme/app.git": "https://example.com/acme/app.git",
		"git@example.com:acme/app.git":     "git@example.com:acme/app.git",
		"/tmp/acme/app.git":                "/tmp/acme/app.git",
		"./acme/app":                       "./acme/app",
		"../app":                           "../app",
		"a/../b":                           "a/../b",
		"acme/../../etc":                   "acme/../../etc",
	} {
		if got := cloneURL(in); got != want {
			t.Errorf("cloneURL(%q) = %q, want %q", in, got, want)
		}
	}
}
