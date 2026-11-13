// Package main 是发布打包工具（设计 §18「分发」、§16.4）：构建嵌入 Web 与内置能力包的单文件二进制，
// 并在没有 go、node 的干净环境里验证 init、serve、打开 Web。只在本机产出文件，不发布到任何仓库或注册表。
package main

import (
	"bytes"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/version"
)

// Config 描述一次构建。Root 是仓库根，Out 是二进制输出路径。
type Config struct {
	Root         string
	Out          string
	GOOS, GOARCH string   // 为空取本机
	SkipWeb      bool     // 已有 web/dist 时跳过 npm（CI 已单独构建过）
	GoFlags      []string // 追加给 go build 的参数
	Log          io.Writer
}

func (c Config) run(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, c.Log, c.Log
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// buildWeb 构建嵌入的 Web（SkipWeb 时跳过）。
func (c Config) buildWeb() error {
	if c.SkipWeb {
		return nil
	}
	if err := c.run(c.Root, nil, "npm", "--prefix", "web", "ci"); err != nil {
		return err
	}
	return c.run(c.Root, nil, "npm", "--prefix", "web", "run", "build")
}

// buildGo 以 CGO_ENABLED=0 静态构建 pkg 到 out。发布构建不得带 apa_testclock 标签（REQ-TESTBUILD-1）。
func (c Config) buildGo(pkg, out string) error {
	if c.Log == nil {
		c.Log = io.Discard
	}
	for _, f := range c.GoFlags {
		if strings.Contains(f, "apa_testclock") {
			return errors.New("release build must not carry the apa_testclock tag")
		}
	}
	env := []string{"CGO_ENABLED=0"}
	if c.GOOS != "" {
		env = append(env, "GOOS="+c.GOOS)
	}
	if c.GOARCH != "" {
		env = append(env, "GOARCH="+c.GOARCH)
	}
	args := append([]string{"build", "-trimpath", "-ldflags", "-s -w", "-o", out}, c.GoFlags...)
	return c.run(c.Root, env, "go", append(args, pkg)...)
}

// Build 先构建 Web（除非 SkipWeb），再静态构建 cli/apa，并写出同目录的 SHA256SUMS。
// 是否真的嵌入了 Web 由 Verify 判定。
func Build(c Config) error {
	if err := c.buildWeb(); err != nil {
		return err
	}
	out, err := filepath.Abs(c.Out)
	if err != nil {
		return err
	}
	if err := c.buildGo("./cli/apa", out); err != nil {
		return err
	}
	bin, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	line := hex.EncodeToString(sum[:]) + "  " + filepath.Base(out) + "\n"
	return os.WriteFile(filepath.Join(filepath.Dir(out), "SHA256SUMS"), []byte(line), 0o644)
}

// builtinPacks 是随发行嵌入的内置能力包（附录 C.1）；Verify 要求它们全部被加载。
var builtinPacks = []string{"brief", "review", "follow-up", "decision-card", "delegation-verify",
	"inbound-triage", "agents-md", "guideline-draft", "background-run", "routine"}

const verifyPassword = "release-verify-pw"

// instance 是在干净环境里运行的 apa：空目录作 PATH（没有 go、node、任何工具）、临时 HOME 与 APA_HOME。
type instance struct {
	bin, home, base string
	env             []string
	cl              *http.Client
	csrf            string        // 登录返回的 csrf_token，写请求必带
	log             *bytes.Buffer // serve 的 stdout+stderr
	stop            func()
}

// startInstance 做 apa init 并启动 apa serve。发布二进制里 APA_TEST_MIGRATION_FAIL_AFTER 必须不生效，
// 所以 init 与 serve 都带着它：迁移不得被注入失败（ACT-REL-12）。
func startInstance(bin string) (*instance, error) {
	bin, err := filepath.Abs(bin)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(bin); err != nil {
		return nil, err
	}
	if runtime.GOOS == "linux" {
		if f, err := elf.Open(bin); err == nil {
			defer f.Close()
			if f.Section(".interp") != nil {
				return nil, errors.New("binary is dynamically linked; a release binary must be static")
			}
		}
	}
	tmp, err := os.MkdirTemp("", "apa-release-verify-")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*instance, error) { os.RemoveAll(tmp); return nil, err }
	emptyPath := filepath.Join(tmp, "emptypath")
	home := filepath.Join(tmp, "apa-home")
	if err := os.Mkdir(emptyPath, 0o755); err != nil {
		return fail(err)
	}
	env := []string{"PATH=" + emptyPath, "HOME=" + tmp, "APA_HOME=" + home, "APA_TEST_MIGRATION_FAIL_AFTER=1"}

	initCmd := exec.Command(bin, "init", "--password-stdin")
	initCmd.Env, initCmd.Stdin = env, strings.NewReader(verifyPassword+"\n")
	if out, err := initCmd.CombinedOutput(); err != nil {
		return fail(fmt.Errorf("apa init: %w: %s", err, out))
	}
	// 用空闲端口，避免与本机已有服务冲突；init 写出的默认配置只改监听地址。
	port, err := freePort()
	if err != nil {
		return fail(err)
	}
	cfg := fmt.Sprintf("listen: 127.0.0.1:%d\n", port)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		return fail(err)
	}

	log := &bytes.Buffer{}
	serve := exec.Command(bin, "serve")
	serve.Env, serve.Stdout, serve.Stderr = env, log, log
	if err := serve.Start(); err != nil {
		return fail(fmt.Errorf("apa serve: %w", err))
	}
	exited := make(chan error, 1)
	go func() { exited <- serve.Wait() }()
	stop := func() {
		_ = serve.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = serve.Process.Kill()
			<-exited
		}
		os.RemoveAll(tmp)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitReady(base, exited, log); err != nil {
		stop()
		return nil, err
	}
	if strings.Contains(log.String(), "migration fault injected") {
		stop()
		return nil, errors.New("release binary honours APA_TEST_MIGRATION_FAIL_AFTER")
	}
	jar, _ := cookiejar.New(nil)
	return &instance{bin: bin, home: home, base: base, env: env, log: log, stop: stop,
		cl: &http.Client{Jar: jar, Timeout: 10 * time.Second}}, nil
}

// do 发一个 JSON 请求（带登录会话），返回状态码与响应体。
func (in *instance) do(method, path string, body any) (int, string, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, in.base+path, rd)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if in.csrf != "" {
		req.Header.Set("X-CSRF-Token", in.csrf)
	}
	resp, err := in.cl.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

func (in *instance) login() error {
	code, body, err := in.do("POST", "/v1/auth/login", map[string]string{"password": verifyPassword})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("web login with the init password: status %d: %s", code, body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.CSRF == "" {
		return fmt.Errorf("login response has no csrf_token: %s", body)
	}
	in.csrf = out.CSRF
	return nil
}

// Verify 在干净环境里跑 bin：静态链接（linux）、apa init、apa serve、GET / 返回嵌入的 Web 页、
// 登录后 /v1/packs 含全部内置包、`apa version` 与 GET /v1/version 相同且 test_clock 为 false、构建标签不含 apa_testclock。
func Verify(bin string) error {
	if err := checkNoTestBuild(bin); err != nil {
		return err
	}
	in, err := startInstance(bin)
	if err != nil {
		return err
	}
	defer in.stop()

	body, code, err := get(in.cl, in.base+"/")
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("GET /: status %d, web UI is not embedded (build web first): %s", code, strings.TrimSpace(body))
	}
	if !strings.Contains(body, `id="root"`) {
		return fmt.Errorf("GET /: response is not the embedded web page: %.200s", body)
	}
	if err := in.checkVersion(); err != nil {
		return err
	}
	if err := in.login(); err != nil {
		return err
	}
	packs, code, err := get(in.cl, in.base+"/v1/packs")
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("GET /v1/packs: status %d: %v", code, err)
	}
	for _, name := range builtinPacks {
		if !strings.Contains(packs, `"`+name+`"`) {
			return fmt.Errorf("builtin pack %q is not loaded from the binary", name)
		}
	}
	return nil
}

// checkVersion：GET /v1/version 不需要凭据，与 `apa version` 内容相同，version 等于本发行，test_clock 为 false。
func (in *instance) checkVersion() error {
	api, code, err := get(&http.Client{Timeout: 10 * time.Second}, in.base+"/v1/version")
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("GET /v1/version without credentials: status %d: %v", code, err)
	}
	cmd := exec.Command(in.bin, "version")
	cmd.Env = append(in.env, "APA_URL="+in.base)
	cli, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("apa version: %w", err)
	}
	var a, b map[string]any
	if json.Unmarshal([]byte(api), &a) != nil || json.Unmarshal(cli, &b) != nil || !reflect.DeepEqual(a, b) {
		return fmt.Errorf("apa version and GET /v1/version differ: %q vs %q", cli, api)
	}
	if a["version"] != version.Release {
		return fmt.Errorf("version is %v, want %s", a["version"], version.Release)
	}
	if a["test_clock"] != false {
		return fmt.Errorf("release binary reports test_clock=%v", a["test_clock"])
	}
	return nil
}

// checkNoTestBuild 读二进制内嵌的构建信息，构建标签含 apa_testclock 即失败（ACT-REL-12）。
func checkNoTestBuild(bin string) error {
	bi, err := buildinfo.ReadFile(bin)
	if err != nil {
		return fmt.Errorf("%s: no Go build info: %w", bin, err)
	}
	for _, s := range bi.Settings {
		if s.Key == "-tags" && strings.Contains(s.Value, "apa_testclock") {
			return fmt.Errorf("%s was built with -tags %s", filepath.Base(bin), s.Value)
		}
	}
	return nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func get(cl *http.Client, url string) (string, int, error) {
	resp, err := cl.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, err
}

func waitReady(base string, exited <-chan error, stderr *bytes.Buffer) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("apa serve exited early: %v: %s", err, stderr.String())
		default:
		}
		if resp, err := http.Get(base + "/"); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("apa serve did not listen on %s within 20s: %s", base, stderr.String())
}
