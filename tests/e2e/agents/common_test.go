//go:build e2e

package agents

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// apaBaseEnv 是 apa 子命令的基础环境（与 baseline.Env 的做法一致，但可由调用方追加会话变量）。
func (f *fx) apaBaseEnv(token string, _ []string) []string {
	env := append(os.Environ(), "APA_HOME="+f.Home, "APA_URL="+f.URL)
	if token != "" {
		env = append(env, "APA_TOKEN="+token)
	}
	return env
}

func (f *fx) tmpFile(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%.600s", err, s)
	}
	return v
}

// git 在 dir 里运行 git（固定作者），返回去掉首尾空白的输出；失败即终止。
func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=acc", "GIT_AUTHOR_EMAIL=acc@example.invalid",
		"GIT_COMMITTER_NAME=acc", "GIT_COMMITTER_EMAIL=acc@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t testing.TB, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t testing.TB, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// repoDir 是 LOCAL_REPO 的本机目录（file:// 去掉前缀）。
func (f *fx) repoDir() string { return strings.TrimPrefix(f.LocalRepo, "file://") }

// registerCheckout 把 LOCAL_REPO 的本机目录登记为 acc-work 的检出（主机名取本机），
// 之后在该目录运行 apa 命令即按"当前目录到项目"的映射落到 acc-work。
func (f *fx) registerCheckout() {
	f.t.Helper()
	host, _ := os.Hostname()
	f.webOK("POST", "/projects/"+f.Projects["acc-work"]+"/repos",
		`{"op":"set_checkout","remote":"`+f.LocalRepo+`","host":"`+host+`","path":"`+f.repoDir()+`"}`, 200)
}

// useTools 构建真实的随发行程序（hosted-reader、github-source）放进 PATH（内核与来源按名字拉起它们），并隔离 git 全局配置。
func useTools(t *testing.T, names ...string) {
	t.Helper()
	pkgs := map[string]string{"hosted-reader": "./sources/hostedreader/cmd/hosted-reader", "github-source": "./sources/github/cmd/github-source"}
	dir := t.TempDir()
	for _, n := range names {
		build := exec.Command("go", "build", "-o", filepath.Join(dir, n), pkgs[n])
		build.Dir = filepath.Join("..", "..", "..")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("构建 %s: %v\n%s", n, err, out)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func useHostedReader(t *testing.T) { t.Helper(); useTools(t, "hosted-reader") }
