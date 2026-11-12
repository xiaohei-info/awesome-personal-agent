// Package main 是发布打包工具（设计 §18「分发」、§16.4）：构建嵌入 Web 与内置能力包的单文件二进制，
// 并在没有 go、node 的干净环境里验证 init、serve、打开 Web。只在本机产出文件，不发布到任何仓库或注册表。
package main

import (
	"bytes"
	"crypto/sha256"
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
	"runtime"
	"strings"
	"syscall"
	"time"
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

// Build 先构建 Web（除非 SkipWeb），再以 CGO_ENABLED=0 静态构建 cli/apa，并写出同目录的 SHA256SUMS。
// 是否真的嵌入了 Web 由 Verify 判定。
func Build(c Config) error {
	if c.Log == nil {
		c.Log = io.Discard
	}
	if !c.SkipWeb {
		if err := c.run(c.Root, nil, "npm", "--prefix", "web", "ci"); err != nil {
			return err
		}
		if err := c.run(c.Root, nil, "npm", "--prefix", "web", "run", "build"); err != nil {
			return err
		}
	}
	out, err := filepath.Abs(c.Out)
	if err != nil {
		return err
	}
	env := []string{"CGO_ENABLED=0"}
	if c.GOOS != "" {
		env = append(env, "GOOS="+c.GOOS)
	}
	if c.GOARCH != "" {
		env = append(env, "GOARCH="+c.GOARCH)
	}
	args := append([]string{"build", "-trimpath", "-ldflags", "-s -w", "-o", out}, c.GoFlags...)
	if err := c.run(c.Root, env, "go", append(args, "./cli/apa")...); err != nil {
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

// Verify 在干净环境里跑 bin：空目录作 PATH（没有 go、node、任何工具）、临时 HOME 与 APA_HOME。
// 依次检查：静态链接（linux）、apa init、apa serve、GET / 返回嵌入的 Web 页、登录后 /v1/packs 含全部内置包。
func Verify(bin string) error {
	bin, err := filepath.Abs(bin)
	if err != nil {
		return err
	}
	if _, err := os.Stat(bin); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		if f, err := elf.Open(bin); err == nil {
			defer f.Close()
			if f.Section(".interp") != nil {
				return errors.New("binary is dynamically linked; a release binary must be static")
			}
		}
	}
	tmp, err := os.MkdirTemp("", "apa-release-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	emptyPath := filepath.Join(tmp, "emptypath")
	home := filepath.Join(tmp, "apa-home")
	if err := os.Mkdir(emptyPath, 0o755); err != nil {
		return err
	}
	env := []string{"PATH=" + emptyPath, "HOME=" + tmp, "APA_HOME=" + home}

	initCmd := exec.Command(bin, "init", "--password-stdin")
	initCmd.Env, initCmd.Stdin = env, strings.NewReader(verifyPassword+"\n")
	if out, err := initCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apa init: %w: %s", err, out)
	}
	// 用空闲端口，避免与本机已有服务冲突；init 写出的默认配置只改监听地址。
	port, err := freePort()
	if err != nil {
		return err
	}
	cfg := fmt.Sprintf("listen: 127.0.0.1:%d\n", port)
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		return err
	}

	var stdout, stderr bytes.Buffer
	serve := exec.Command(bin, "serve")
	serve.Env, serve.Stdout, serve.Stderr = env, &stdout, &stderr
	if err := serve.Start(); err != nil {
		return fmt.Errorf("apa serve: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- serve.Wait() }()
	defer func() {
		_ = serve.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = serve.Process.Kill()
			<-exited
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitReady(base, exited, &stderr); err != nil {
		return err
	}
	jar, _ := cookiejar.New(nil)
	cl := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	body, code, err := get(cl, base+"/")
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("GET /: status %d, web UI is not embedded (build web first): %s", code, strings.TrimSpace(body))
	}
	if !strings.Contains(body, `id="root"`) {
		return fmt.Errorf("GET /: response is not the embedded web page: %.200s", body)
	}

	login, _ := json.Marshal(map[string]string{"password": verifyPassword})
	resp, err := cl.Post(base+"/v1/auth/login", "application/json", bytes.NewReader(login))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("web login with the init password: status %d", resp.StatusCode)
	}
	packs, code, err := get(cl, base+"/v1/packs")
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
