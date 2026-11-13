package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/version"
)

// manifestName 是包内哈希清单，格式与 `sha256sum -c` 相同（"<hex>  <相对路径>"）。
const manifestName = "SHA256SUMS"

// Package 按设计 §16.6 在 c.Out 下组出带版本号的发布包 apa-<版本>-<os>-<arch>/ 及同名 .tar.gz，返回包目录：
// 主程序、预编译的来源程序与 ext、宿主插件、部署样例、用户文档、哈希清单。全部来自本仓库，不联网发布。
func Package(c Config) (string, error) {
	goos, goarch := c.GOOS, c.GOARCH
	if goos == "" || goarch == "" {
		out, err := exec.Command("go", "env", "GOOS", "GOARCH").Output()
		if err != nil {
			return "", err
		}
		f := strings.Fields(string(out))
		if len(f) != 2 {
			return "", fmt.Errorf("go env: unexpected %q", out)
		}
		goos, goarch = orDefault(goos, f[0]), orDefault(goarch, f[1])
	}
	out, err := filepath.Abs(c.Out)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("apa-%s-%s-%s", version.Release, goos, goarch)
	dir := filepath.Join(out, name)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := c.buildWeb(); err != nil {
		return "", err
	}
	if err := c.buildGo("./cli/apa", filepath.Join(dir, "bin", "apa")); err != nil {
		return "", err
	}
	if err := c.buildSources(dir); err != nil {
		return "", err
	}
	if err := c.buildExts(dir); err != nil {
		return "", err
	}
	for _, d := range []string{"plugins/hermes", "deploy", "docs/user"} {
		if err := copyTree(filepath.Join(c.Root, d), filepath.Join(dir, d)); err != nil {
			return "", err
		}
	}
	if err := writeSums(dir); err != nil {
		return "", err
	}
	return dir, writeTarball(dir, dir+".tar.gz")
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// buildSources 把 sources/*/cmd/<名> 各自编成 sources/<名>。
func (c Config) buildSources(dir string) error {
	cmds, err := filepath.Glob(filepath.Join(c.Root, "sources", "*", "cmd", "*"))
	if err != nil || len(cmds) == 0 {
		return fmt.Errorf("no source programs found under sources/*/cmd: %v", err)
	}
	for _, p := range cmds {
		rel, _ := filepath.Rel(c.Root, p)
		if err := c.buildGo("./"+filepath.ToSlash(rel), filepath.Join(dir, "sources", filepath.Base(p))); err != nil {
			return err
		}
	}
	return nil
}

var cmdLine = regexp.MustCompile(`(?m)^command:.*$`)
var goRun = regexp.MustCompile(`^command:\s*\[\s*go\s*,\s*run\s*,\s*([^\]\s,]+)\s*\]\s*$`)

// buildExts 为 ext/<名>/ext.yaml 里 `go run <包>` 的每个 ext 预编译二进制，放进 ext/<名>/，并把清单的 command 改成直接运行它。
func (c Config) buildExts(dir string) error {
	mans, err := filepath.Glob(filepath.Join(c.Root, "ext", "*", "ext.yaml"))
	if err != nil || len(mans) == 0 {
		return fmt.Errorf("no ext manifests found under ext/*: %v", err)
	}
	for _, m := range mans {
		src := filepath.Dir(m)
		name := filepath.Base(src)
		data, err := os.ReadFile(m)
		if err != nil {
			return err
		}
		line := cmdLine.FindString(string(data))
		g := goRun.FindStringSubmatch(line)
		if g == nil {
			return fmt.Errorf("ext %s: command is not `[go, run, <package>]`: %q", name, line)
		}
		pkg := filepath.Join(src, g[1])
		bin := name
		if g[1] != "." {
			bin = filepath.Base(g[1])
		}
		rel, _ := filepath.Rel(c.Root, pkg)
		dst := filepath.Join(dir, "ext", name)
		if err := c.buildGo("./"+filepath.ToSlash(rel), filepath.Join(dst, bin)); err != nil {
			return err
		}
		manifest := cmdLine.ReplaceAllLiteralString(string(data), "command: [./"+bin+"]")
		if err := os.WriteFile(filepath.Join(dst, "ext.yaml"), []byte(manifest), 0o644); err != nil {
			return err
		}
		if b, err := os.ReadFile(filepath.Join(src, "README.md")); err == nil {
			if err := os.WriteFile(filepath.Join(dst, "README.md"), b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyTree 复制目录，跳过测试代码与缓存。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "tests" || d.Name() == "__pycache__") || strings.HasSuffix(d.Name(), "_test.go") {
			return fs.SkipDir
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

func fileSum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// packageFiles 是包内除清单外的全部文件（相对路径，斜杠，已排序）。
func packageFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == manifestName && filepath.Dir(p) == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(files)
	return files, err
}

func writeSums(dir string) error {
	files, err := packageFiles(dir)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, f := range files {
		sum, err := fileSum(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, f)
	}
	return os.WriteFile(filepath.Join(dir, manifestName), []byte(b.String()), 0o644)
}

// CheckSums 逐文件核对哈希清单（等价于 `sha256sum -c`），并要求清单覆盖包内每个文件。
func CheckSums(dir string) error {
	f, err := os.Open(filepath.Join(dir, manifestName))
	if err != nil {
		return err
	}
	defer f.Close()
	listed := map[string]bool{}
	var bad []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sum, rel, ok := strings.Cut(sc.Text(), "  ")
		if !ok {
			return fmt.Errorf("%s: malformed line %q", manifestName, sc.Text())
		}
		listed[rel] = true
		if got, err := fileSum(filepath.Join(dir, filepath.FromSlash(rel))); err != nil || got != sum {
			bad = append(bad, rel)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	files, err := packageFiles(dir)
	if err != nil {
		return err
	}
	for _, rel := range files {
		if !listed[rel] {
			bad = append(bad, rel+" (not in manifest)")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("hash check failed: %s", strings.Join(bad, ", "))
	}
	return nil
}

func writeTarball(dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	root := filepath.Base(dir)
	files, err := packageFiles(dir)
	if err != nil {
		return err
	}
	for _, rel := range append(files, manifestName) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: root + "/" + rel, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}); err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// required 是 §16.6 表里每一类内容在包内必须存在的代表文件。
var required = []string{"bin/apa", "sources/rss-source", "ext/export/ext.yaml", "ext/telegram/ext.yaml", "ext/webhookforward/ext.yaml",
	"plugins/hermes/core/plugin.yaml", "plugins/hermes/core/README.md", "deploy/systemd/apa.service", "deploy/nginx/apa.conf.example",
	"deploy/caddy/Caddyfile.example", "docs/user/quickstart.md", "docs/user/run-tiers.md", "docs/user/troubleshooting.md",
	"docs/user/security.md", "docs/user/upgrade.md", "docs/user/uninstall.md", "docs/user/changelog.md", manifestName}

const atomFeed = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>t</title><id>urn:feed</id><updated>2030-01-01T00:00:00Z</updated>` +
	`<entry><id>a</id><title>release-verify</title><link href="http://x/a"/><updated>2030-01-01T00:00:00Z</updated></entry></feed>`

// VerifyPackage 在干净环境里只用包内文件验收一个发布包：内容齐全、哈希清单通过、所有二进制不带 apa_testclock、
// 主程序 init/serve/Web/版本（Verify）、用包内路径登记 RSS 来源并拉到事件、`apa ext run export` 写出文件。
func VerifyPackage(dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for _, r := range required {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(r))); err != nil {
			return fmt.Errorf("package misses %s", r)
		}
	}
	if err := CheckSums(dir); err != nil {
		return err
	}
	var bins []string
	for _, pat := range []string{"bin/*", "sources/*", "ext/*/*"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, p := range m {
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
				bins = append(bins, p)
			}
		}
	}
	for _, b := range bins {
		if err := checkNoTestBuild(b); err != nil {
			return err
		}
	}
	apa := filepath.Join(dir, "bin", "apa")
	if err := Verify(apa); err != nil {
		return err
	}
	in, err := startInstance(apa)
	if err != nil {
		return err
	}
	defer in.stop()
	if err := in.login(); err != nil {
		return err
	}
	if err := in.pullPackagedRSS(filepath.Join(dir, "sources", "rss-source")); err != nil {
		return err
	}
	return in.runPackagedExport(dir)
}

// pullPackagedRSS 用包内来源程序登记 RSS 来源（指向本机静态 Atom），等它拉到事件。
func (in *instance) pullPackagedRSS(program string) error {
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = io.WriteString(w, atomFeed)
	}))
	defer feed.Close()
	code, body, err := in.do("POST", "/v1/sources", map[string]any{"id": "src_rel_rss", "type": "script", "command": program, "schedule": "1s",
		"timeout": 20, "source_type": "rss", "config": map[string]string{"url": feed.URL}})
	if err != nil || code/100 != 2 {
		return fmt.Errorf("register packaged rss source: status %d: %v %s", code, err, body)
	}
	return until(30*time.Second, "packaged rss source pulls an event", func() (bool, error) {
		code, body, err := in.do("GET", "/v1/sources/src_rel_rss/events", nil)
		if err != nil || code != http.StatusOK {
			return false, fmt.Errorf("GET source events: status %d: %v %s", code, err, body)
		}
		var out struct {
			Events []json.RawMessage `json:"events"`
		}
		return json.Unmarshal([]byte(body), &out) == nil && len(out.Events) > 0, nil
	})
}

// runPackagedExport 在包根目录运行 `apa ext run export`（默认 ext 目录），等它在导出目录写出文件。
func (in *instance) runPackagedExport(pkg string) error {
	code, body, err := in.do("POST", "/v1/settings/ext-keys", map[string]string{"label": "REL", "scope": "export"})
	var key struct {
		Secret string `json:"secret"`
	}
	if err != nil || code != http.StatusCreated || json.Unmarshal([]byte(body), &key) != nil || key.Secret == "" {
		return fmt.Errorf("issue export ext key: status %d: %v %s", code, err, body)
	}
	exportDir, err := os.MkdirTemp("", "apa-release-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(exportDir)
	var log strings.Builder
	cmd := exec.Command(in.bin, "ext", "run", "export")
	cmd.Dir, cmd.Stdout, cmd.Stderr = pkg, &log, &log
	cmd.Env = append(in.env, "APA_URL="+in.base, "APA_EXT_KEY="+key.Secret, "APA_EXPORT_DIR="+exportDir)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("apa ext run export: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}()
	return until(30*time.Second, "export ext writes a file", func() (bool, error) {
		select {
		case err := <-exited:
			return false, fmt.Errorf("apa ext run export exited early: %v: %s", err, log.String())
		default:
		}
		ents, _ := os.ReadDir(exportDir)
		return len(ents) > 0, nil
	})
}

func until(d time.Duration, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timed out waiting for: " + what)
}
