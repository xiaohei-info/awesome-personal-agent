package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildPackage 用 overlay 虚拟的 Web 构建一个完整发布包（不依赖 npm）。
func buildPackage(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	var log bytes.Buffer
	dir, err := Package(Config{Root: root, Out: t.TempDir(), SkipWeb: true, GoFlags: webOverlay(t, root), Log: &log})
	if err != nil {
		t.Fatalf("package: %v\n%s", err, log.String())
	}
	return dir
}

// ACT-REL-06：包内有 §16.6 表里的每一类内容；哈希清单未改动时全部通过，改动任一文件一个字节后该文件校验失败。
func TestPackageContentsAndHashManifest(t *testing.T) {
	dir := buildPackage(t)
	if !strings.Contains(filepath.Base(dir), "0.2.0") {
		t.Errorf("package dir %q carries no version", dir)
	}
	for _, p := range []string{"bin/apa", "sources/rss-source", "sources/mail-himalaya", "ext/export/export", "ext/export/ext.yaml",
		"ext/telegram/telegram-ext", "ext/webhookforward/webhookforward", "plugins/hermes/core/plugin.yaml",
		"deploy/systemd/apa.service", "deploy/nginx/apa.conf.example", "deploy/caddy/Caddyfile.example",
		"docs/user/quickstart.md", "docs/user/changelog.md", "SHA256SUMS"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("package misses %s", p)
		}
	}
	if err := CheckSums(dir); err != nil {
		t.Fatalf("untouched package fails its own manifest: %v", err)
	}
	// 与 `sha256sum -c` 同一格式、同一结论。
	if _, err := exec.LookPath("sha256sum"); err == nil {
		cmd := exec.Command("sha256sum", "-c", "--quiet", "SHA256SUMS")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sha256sum -c: %v %s", err, out)
		}
	}
	victim := filepath.Join(dir, "docs", "user", "quickstart.md")
	b, _ := os.ReadFile(victim)
	b[0] ^= 1
	if err := os.WriteFile(victim, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckSums(dir); err == nil || !strings.Contains(err.Error(), "docs/user/quickstart.md") {
		t.Fatalf("one flipped byte must fail the manifest naming the file, got %v", err)
	}
}

// ACT-REL-01/07/12：只用包内文件，在空 PATH 的干净环境里装全并启动；包内来源程序拉取成功、包内 ext 经 `apa ext run` 写出文件；
// 测试入口（时钟、迁移注入）不生效，构建信息不含 apa_testclock。
func TestPackageRunsOnCleanHost(t *testing.T) {
	if err := VerifyPackage(buildPackage(t)); err != nil {
		t.Fatal(err)
	}
}

// 失败语义：发布构建拒绝 apa_testclock 标签；缺内容的包与被改动的包 VerifyPackage 报错。
func TestPackageRejectsTestBuildAndBrokenPackages(t *testing.T) {
	root := moduleRoot(t)
	if _, err := Package(Config{Root: root, Out: t.TempDir(), SkipWeb: true, GoFlags: []string{"-tags", "apa_testclock"}}); err == nil ||
		!strings.Contains(err.Error(), "apa_testclock") {
		t.Fatalf("test-clock build must be refused, got %v", err)
	}
	dir := buildPackage(t)
	if err := os.Remove(filepath.Join(dir, "deploy", "systemd", "apa.service")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPackage(dir); err == nil {
		t.Fatal("package without the service unit must not verify")
	}
}

// 带 apa_testclock 构建的二进制被 checkNoTestBuild 识别。
func TestCheckNoTestBuildDetectsTestClock(t *testing.T) {
	root := moduleRoot(t)
	bin := filepath.Join(t.TempDir(), "apa")
	cmd := exec.Command("go", "build", "-tags", "apa_testclock", "-o", bin, "./cli/apa")
	cmd.Dir = root
	cmd.Args = append(cmd.Args[:2], append(webOverlay(t, root), cmd.Args[2:]...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	if err := checkNoTestBuild(bin); err == nil {
		t.Fatal("apa_testclock binary must be flagged")
	}
}

// 命令入口 -pack：产出包目录与 tar.gz；构建失败给非零退出码。
func TestRunPackFlag(t *testing.T) {
	root := moduleRoot(t)
	var out, errb bytes.Buffer
	dst := t.TempDir()
	// 用 overlay 的 Web，避免依赖工作区是否已构建。
	if code := run(root, []string{"-skip-web", "-pack", dst, "-no-verify"}, &out, &errb); code != 0 {
		t.Fatalf("pack exit %d: %s", code, errb.String())
	}
	m, _ := filepath.Glob(filepath.Join(dst, "apa-0.2.0-*.tar.gz"))
	if len(m) != 1 || !strings.Contains(out.String(), "release: ok") {
		t.Fatalf("no tarball: %v %q", m, out.String())
	}
	if code := run(root, []string{"-skip-web", "-pack", t.TempDir(), "-goos", "no-such-os"}, &out, &errb); code != 1 {
		t.Fatalf("bad goos exit %d", code)
	}
}

// 失败语义：清单没有覆盖的新增文件、被删除的文件、被篡改的主程序，都不能通过。
func TestPackageManifestCatchesExtraMissingAndTamperedFiles(t *testing.T) {
	dir := buildPackage(t)
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckSums(dir); err == nil || !strings.Contains(err.Error(), "extra.txt") {
		t.Fatalf("file outside manifest must fail, got %v", err)
	}
	os.Remove(filepath.Join(dir, "extra.txt"))
	os.Remove(filepath.Join(dir, "docs", "user", "uninstall.md"))
	if err := CheckSums(dir); err == nil || !strings.Contains(err.Error(), "uninstall.md") {
		t.Fatalf("missing file must fail, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckSums(dir); err == nil {
		t.Fatal("malformed manifest must fail")
	}
}

// 失败语义：Verify 拒绝带 apa_testclock 的二进制；缺失的二进制报错。
func TestVerifyRejectsTestClockBinary(t *testing.T) {
	root := moduleRoot(t)
	bin := filepath.Join(t.TempDir(), "apa")
	args := append([]string{"build", "-tags", "apa_testclock"}, webOverlay(t, root)...)
	cmd := exec.Command("go", append(args, "-o", bin, "./cli/apa")...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	if err := Verify(bin); err == nil || !strings.Contains(err.Error(), "apa_testclock") {
		t.Fatalf("got %v", err)
	}
	if err := checkNoTestBuild(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("missing binary must fail")
	}
}

// 失败语义：缺来源程序、缺 ext 清单、ext 命令不是 `go run <包>`、npm 构建失败，都要暴露真实错误而不是产出残缺的包。
func TestPackageStepsFailLoudly(t *testing.T) {
	empty := Config{Root: t.TempDir()}
	if err := empty.buildSources(t.TempDir()); err == nil {
		t.Error("no source programs must fail")
	}
	if err := empty.buildExts(t.TempDir()); err == nil {
		t.Error("no ext manifests must fail")
	}
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "ext", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "ext", "x", "ext.yaml"), []byte("kind: export\ncommand: [python3, x.py]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Config{Root: bad}).buildExts(t.TempDir()); err == nil || !strings.Contains(err.Error(), "ext x") {
		t.Errorf("non go-run ext command must fail, got %v", err)
	}
	if err := (Config{Root: t.TempDir()}).buildWeb(); err == nil {
		t.Error("npm failure must surface")
	}
	if _, err := Package(Config{Root: t.TempDir(), Out: t.TempDir(), SkipWeb: true}); err == nil {
		t.Error("package from a directory that is not the repo must fail")
	}
	if err := VerifyPackage(t.TempDir()); err == nil {
		t.Error("empty directory is not a release package")
	}
}

func TestHelpersReportFailures(t *testing.T) {
	if err := until(300*time.Millisecond, "never", func() (bool, error) { return false, nil }); err == nil || !strings.Contains(err.Error(), "never") {
		t.Errorf("timeout must name what it waited for, got %v", err)
	}
	if err := until(time.Second, "x", func() (bool, error) { return false, os.ErrClosed }); err != os.ErrClosed {
		t.Errorf("condition error must pass through, got %v", err)
	}
	if _, err := fileSum(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("missing file must fail")
	}
	if _, _, err := get(http.DefaultClient, "http://127.0.0.1:1/"); err == nil {
		t.Error("unreachable url must fail")
	}
	if _, err := startInstance(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("missing binary must fail")
	}
	if err := checkNoTestBuild(os.Args[0]); err != nil { // go test 二进制是普通构建
		t.Errorf("plain build flagged: %v", err)
	}
}
