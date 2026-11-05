package sources_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/sources"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 假内核只回来源登记：路由本身在 api/sources 里测，这里测 CLI 拉起程序的契约。
func apa(t *testing.T, home string, sources map[string]string, args ...string) (int, string, string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := sources[strings.TrimPrefix(r.URL.Path, "/v1/sources/")]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":"not_found","why":"no such source","next":"list sources"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	vars := map[string]string{"APA_URL": ts.URL, "APA_TOKEN": "t", "APA_HOME": home}
	var out, errb bytes.Buffer
	code := client.Main(append([]string{"source"}, args...), client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: home,
		Stdin: strings.NewReader("code-1\n"), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return code, out.String(), errb.String()
}

// `apa source auth <id>`：附带拉取程序在登记的 credentials_path 里以 `auth <credentials_path>` 运行，继承标准输入输出；不是 pull 类型的不启动（附录 B.1）。
func TestSourceAuthRunsBundledProgram(t *testing.T) {
	home := t.TempDir()
	prog := filepath.Join(t.TempDir(), "fake-source")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nread line\necho \"$1 $line\" > /dev/stdout; echo \"$2\" > arg\npwd > cwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	failing := filepath.Join(t.TempDir(), "failing-source")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho denied >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(home, "sources", "src_ok")
	reg := map[string]string{
		"src_ok":     `{"id":"src_ok","type":"pull","command":"` + prog + `","credentials_path":"` + creds + `","enabled":true}`,
		"src_nocred": `{"id":"src_nocred","type":"pull","command":"` + prog + `","enabled":true}`,
		"src_bad":    `{"id":"src_bad","type":"pull","command":"` + failing + `","credentials_path":"` + filepath.Join(home, "sources", "src_bad") + `","enabled":true}`,
		"src_tool":   `{"id":"src_tool","type":"script","command":"` + prog + `","enabled":true}`,
		"src_hook":   `{"id":"src_hook","type":"webhook","enabled":true}`,
	}

	code, out, errOut := apa(t, home, reg, "auth", "src_ok")
	if code != 0 || strings.TrimSpace(out) != "auth code-1" {
		t.Fatalf("auth: %d %q %q", code, out, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(creds, "arg")); err != nil || strings.TrimSpace(string(b)) != creds {
		t.Fatalf("credentials_path arg: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(creds, "cwd")); err != nil || !strings.HasSuffix(strings.TrimSpace(string(b)), filepath.Join("sources", "src_ok")) {
		t.Fatalf("cwd: %q %v", b, err)
	}
	if code, _, errOut := apa(t, home, reg, "auth", "src_bad"); code != domain.ExitUnclassified || !strings.Contains(errOut, "denied") {
		t.Fatalf("failing program: %d %q", code, errOut)
	}
	for _, id := range []string{"src_tool", "src_hook", "src_nocred"} {
		if code, _, _ := apa(t, home, reg, "auth", id); code != domain.ExitUsage {
			t.Fatalf("%s: %d", id, code)
		}
	}
	if code, _, _ := apa(t, home, reg, "auth", "src_none"); code != domain.ExitNotFound {
		t.Fatalf("unknown: %d", code)
	}
	for _, args := range [][]string{{}, {"auth"}, {"auth", "a", "b"}, {"auth", "--x"}, {"bogus"}} {
		if code, _, _ := apa(t, home, reg, args...); code != domain.ExitUsage {
			t.Fatalf("%v: %d", args, code)
		}
	}
}
