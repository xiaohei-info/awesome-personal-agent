// Package nohost 在剔除 harness 的 PATH 下跑真实 apa 二进制的 e2e 套件（flow-release 的 ACT-REL-04）。
// 套件按包拆开跑（tests/release/generic*）：合在一个包里串行要越过 Go 默认的 10 分钟包超时。
package nohost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// harnessNames 与 tools/redline 的词表一致，只用来构造"未安装 harness"的主机。
var harnessNames = []string{"hermes", "claude", "codex", "dsh", "gemini", "opencode", "openclaw"}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("找不到仓库根（go.mod）")
	}
}

func run(t *testing.T, root string, env []string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out), 0
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// noHarnessEnv 返回把含 harness 的 PATH 目录剔除后的环境，并确认确实找不到任何 harness。
func noHarnessEnv(t *testing.T) []string {
	t.Helper()
	var keep []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		has := false
		for _, n := range harnessNames {
			if st, err := os.Stat(filepath.Join(dir, n)); err == nil && !st.IsDir() {
				has = true
			}
		}
		if !has {
			keep = append(keep, dir)
		}
	}
	path := strings.Join(keep, string(os.PathListSeparator))
	env := append(os.Environ(), "PATH="+path)
	t.Setenv("PATH", path) // 让下面的 LookPath 与子进程看到同一个 PATH
	for _, n := range harnessNames {
		if p, err := exec.LookPath(n); err == nil {
			t.Fatalf("未安装 harness 的主机上不应找到 %s（%s）", n, p)
		}
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("剔除 harness 目录后找不到 go：%v", err)
	}
	return env
}

// Run 在未安装任何 harness 的主机上跑 suites（可带 -run 过滤），要求每个套件都有通过记录。
func Run(t *testing.T, suites []string, runFilter string) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short 跳过：需要先构建 Web（npm --prefix web run build）")
	}
	root := repoRoot(t)
	env := noHarnessEnv(t)
	args := []string{"test", "-tags", "e2e", "-count=1", "-p", "4"}
	if runFilter != "" {
		args = append(args, "-run", runFilter)
	}
	out, code := run(t, root, env, "go", append(args, suites...)...)
	if code != 0 {
		t.Fatalf("未装 harness 时通用能力失败（退出码 %d）:\n%s", code, tail(out, 4000))
	}
	for _, s := range suites {
		if !strings.Contains(out, "ok  \tgithub.com/xiaohei-info/awesome-personal-agent/"+strings.TrimPrefix(s, "./")) {
			t.Errorf("套件 %s 没有通过的记录:\n%s", s, tail(out, 2000))
		}
	}
}
