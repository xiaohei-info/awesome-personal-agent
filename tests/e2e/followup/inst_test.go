//go:build e2e

package followup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// inst 是在一份账本夹具副本上运行的 `apa serve`（旧版本或待测版本）。它只做升级类流程需要的最少接线：
// 起停、Web 登录、Bearer 请求与 CLI；夹具的口令与凭据来自 manifest，不是 baseline 的 Password。
type inst struct {
	t       *testing.T
	bin     string
	home    string
	URL     string
	cmd     *exec.Cmd
	done    chan struct{}
	logs    bytes.Buffer
	session string
	csrf    string
}

// startInst 把夹具副本的监听改为本机空闲端口（保留其余配置，extra 追加到末尾）并启动 bin serve。
func startInst(t *testing.T, bin, home, extra string) *inst {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg := filepath.Join(home, "config.yaml")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(ln, "listen:") && ln != "" {
			lines = append(lines, ln)
		}
	}
	lines = append([]string{"listen: " + addr}, lines...)
	writeFile(t, cfg, strings.Join(lines, "\n")+"\n"+extra)
	i := &inst{t: t, bin: bin, home: home, URL: "http://" + addr}
	i.cmd = exec.Command(bin, "serve")
	i.cmd.Env = append(os.Environ(), "APA_HOME="+home)
	out, _ := i.cmd.StdoutPipe()
	i.cmd.Stderr = &i.logs
	if err := i.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	i.done = make(chan struct{})
	go func() { _ = i.cmd.Wait(); close(i.done) }()
	t.Cleanup(i.stop)
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if !strings.Contains(line, `"ok":true`) {
			t.Fatalf("%s serve 没有起来: %q %s", filepath.Base(bin), line, i.logs.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("%s serve 60 秒内没有就绪: %s", filepath.Base(bin), i.logs.String())
	}
	return i
}

func (i *inst) stop() {
	if i.cmd == nil || i.cmd.Process == nil {
		return
	}
	_ = i.cmd.Process.Signal(os.Interrupt)
	select {
	case <-i.done:
	case <-time.After(10 * time.Second):
		_ = i.cmd.Process.Kill()
		<-i.done
	}
	i.cmd = nil
}

// req 发一次请求；cookie、csrf 为 Web 会话，token 非空则带 Bearer。
func (i *inst) req(method, path, body, token string, web bool) (int, string) {
	i.t.Helper()
	r, err := http.NewRequest(method, i.URL+path, strings.NewReader(body))
	if err != nil {
		i.t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if web {
		r.Header.Set("Cookie", "apa_session="+i.session)
		r.Header.Set("X-CSRF-Token", i.csrf)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(r)
	if err != nil {
		i.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == "apa_session" {
			i.session = c.Value
		}
	}
	return resp.StatusCode, string(b)
}

// login 以夹具口令登录 Web 并记下会话与 CSRF。
func (i *inst) login(secret string) {
	i.t.Helper()
	code, body := i.req("POST", "/v1/auth/login", `{"password":"`+secret+`"}`, "", false)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil || i.session == "" {
		i.t.Fatalf("Web 登录失败: %d %s", code, body)
	}
	i.csrf = out.CSRF
}

func (i *inst) web(method, path, body string) (int, string) {
	i.t.Helper()
	return i.req(method, path, body, "", true)
}
func (i *inst) api(method, path, body, token string) (int, string) {
	i.t.Helper()
	return i.req(method, path, body, token, false)
}

// apa 以 token 运行 CLI（同一份 APA_HOME 与 APA_URL）。
func (i *inst) apa(token string, args ...string) (int, string) {
	i.t.Helper()
	cmd := exec.Command(i.bin, args...)
	cmd.Env = append(os.Environ(), "APA_HOME="+i.home, "APA_URL="+i.URL, "APA_TOKEN="+token)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		out = append(out, ee.Stderr...)
	} else if err != nil {
		i.t.Fatal(err)
	}
	return code, string(out)
}
