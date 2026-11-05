package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleRoot 是仓库根（本包在 tools/release 下）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// webOverlay 用 go build -overlay 虚拟出 web/dist/index.html，让测试不依赖 npm、也不改工作区。
func webOverlay(t *testing.T, root string) []string {
	t.Helper()
	dir := t.TempDir()
	idx := filepath.Join(dir, "index.html")
	if err := os.WriteFile(idx, []byte(`<!doctype html><html><body><div id="root"></div></body></html>`), 0o644); err != nil {
		t.Fatal(err)
	}
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {filepath.Join(root, "web", "dist", "index.html"): idx}})
	p := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(p, ov, 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"-overlay=" + p}
}

func build(t *testing.T, extra []string) (string, error) {
	t.Helper()
	root := moduleRoot(t)
	out := filepath.Join(t.TempDir(), "apa")
	var log bytes.Buffer
	err := Build(Config{Root: root, Out: out, SkipWeb: true, GoFlags: extra, Log: &log})
	if err != nil {
		t.Logf("build log: %s", log.String())
	}
	return out, err
}

// ACT-REL-01：发布构建产出嵌入 Web 与全部内置包的单文件二进制，
// 在没有 go、node、任何 PATH 工具的干净环境里 init、serve、打开 Web 并看到内置包。
func TestReleaseBinaryRunsInitServeAndOpensWebInCleanEnvironment(t *testing.T) {
	root := moduleRoot(t)
	bin, err := build(t, webOverlay(t, root))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sums, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "SHA256SUMS"))
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(sums)), "  apa") {
		t.Fatalf("SHA256SUMS missing or malformed: %q %v", sums, err)
	}
	if err := Verify(bin); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// 失败语义：web/dist 没有构建产物时二进制只会对 / 回 503，Verify 必须报错而不是放行。
func TestVerifyRejectsBinaryWithoutBuiltWeb(t *testing.T) {
	// overlay 里映射到空串 = 在这次构建里删除该文件，无论工作区是否已构建过 Web。
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {filepath.Join(moduleRoot(t), "web", "dist", "index.html"): ""}})
	p := filepath.Join(t.TempDir(), "overlay.json")
	if err := os.WriteFile(p, ov, 0o644); err != nil {
		t.Fatal(err)
	}
	bin, err := build(t, []string{"-overlay=" + p})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	err = Verify(bin)
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("expected a web-not-embedded error, got %v", err)
	}
}

func TestVerifyFailsOnMissingBinary(t *testing.T) {
	if err := Verify(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestBuildReportsGoBuildFailure(t *testing.T) {
	root := moduleRoot(t)
	var log bytes.Buffer
	err := Build(Config{Root: root, Out: filepath.Join(t.TempDir(), "apa"), SkipWeb: true, GOOS: "no-such-os", Log: &log})
	if err == nil {
		t.Fatal("expected go build failure to surface")
	}
}

// 命令入口：交叉编译时跳过本机运行并成功；未知参数与构建失败给出非零退出码。
func TestRunExitCodes(t *testing.T) {
	root := moduleRoot(t)
	var out, errb bytes.Buffer
	dst := filepath.Join(t.TempDir(), "apa-arm64")
	if code := run(root, []string{"-skip-web", "-goos", "linux", "-goarch", "arm64", "-no-verify", "-out", dst}, &out, &errb); code != 0 {
		t.Fatalf("cross build exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(dst); err != nil || !strings.Contains(out.String(), "release: ok") {
		t.Fatalf("cross build produced no binary: %v %q", err, out.String())
	}
	if code := run(root, []string{"-bogus"}, &out, &errb); code != 2 {
		t.Fatalf("bad flag exit %d", code)
	}
	if code := run(root, []string{"-skip-web", "-goos", "no-such-os", "-out", filepath.Join(t.TempDir(), "x")}, &out, &errb); code != 1 {
		t.Fatalf("build failure exit %d", code)
	}
}
