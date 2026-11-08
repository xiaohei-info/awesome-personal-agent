package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/sources/github"
)

// fakeGH 在 PATH 里放一个假 gh，把参数和 GH_CONFIG_DIR 记到 rec。
func fakeGH(t *testing.T) (rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	script := "#!/bin/sh\necho \"$1 $GH_CONFIG_DIR\" >> " + rec + "\n[ \"$1\" = auth ] && exit 0\ncase \"$4\" in */contents/*) echo 'gh: Not Found (HTTP 404)' >&2; exit 1;; esac\necho '[]'\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return rec
}

func TestBadArgumentsExitWithConfigFailure(t *testing.T) {
	for _, args := range [][]string{{"auth"}, {"auth", "relative/gh"}, {"auth", "/a", "b"}, {"nope"}} {
		var errb bytes.Buffer
		if code := run(context.Background(), args, nil, &bytes.Buffer{}, &errb); code != github.ExitAuth || !strings.Contains(errb.String(), "usage") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errb.String())
		}
	}
}

func TestAuthWritesToGivenCredentialsPath(t *testing.T) {
	rec := fakeGH(t)
	dir := filepath.Join(t.TempDir(), "gh")
	if code := run(context.Background(), []string{"auth", dir}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); code != github.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(rec); strings.TrimSpace(string(b)) != "auth "+dir {
		t.Fatalf("gh called as %q", b)
	}
}

// 真实入口：凭据目录只来自 stdin 的 credentials_path，APA_HOME 指向别处也不影响。
func TestPullUsesCredentialsPathFromStdin(t *testing.T) {
	rec := fakeGH(t)
	t.Setenv("APA_HOME", "/elsewhere")
	in := `{"source_id":"src_gh","credentials_path":"/data/sources/src_gh/gh","config":{"repos":["o/r"]}}`
	var out bytes.Buffer
	if code := run(context.Background(), nil, strings.NewReader(in), &out, &bytes.Buffer{}); code != github.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(rec); strings.TrimSpace(string(b)) != "api /data/sources/src_gh/gh\napi /data/sources/src_gh/gh" || !strings.Contains(out.String(), `"cursor"`) {
		t.Fatalf("gh called as %q, out %q", b, out.String())
	}
}
