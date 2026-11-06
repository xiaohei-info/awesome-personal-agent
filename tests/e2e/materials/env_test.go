//go:build e2e

package materials

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// newEnv 建一套验收环境。hosted-reader 是内核按名字（PATH）拉起的真实读取器，这里构建真实二进制并放进 PATH；
// repo 非空时用作 <TEST_REPO>。
func newEnv(t *testing.T, repo string) *baseline.Env {
	t.Helper()
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "hosted-reader"), "./sources/hostedreader/cmd/hosted-reader")
	build.Dir = filepath.Join(wd(t), "..", "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 hosted-reader: %v\n%s", err, out)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// git 身份与配置只来自测试，不读运行者的全局配置。
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "acc")
	t.Setenv("GIT_AUTHOR_EMAIL", "acc@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "acc")
	t.Setenv("GIT_COMMITTER_EMAIL", "acc@example.invalid")
	if repo != "" {
		t.Setenv("APA_E2E_TEST_REPO", repo)
	}
	return baseline.Setup(t)
}

func wd(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// git 在 dir 运行 git 并返回去掉首尾空白的输出。
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
