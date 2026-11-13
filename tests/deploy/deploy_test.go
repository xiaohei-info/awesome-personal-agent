// Package deploy 验证 deploy/ 下的部署样例（设计 §16）：样例内容满足暴露约束，
// 且样例所依赖的代理契约在真实的 apa 二进制上成立。
package deploy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
)

const root = "../../deploy/"

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(root + rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func has(t *testing.T, file, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("%s 缺少 %q", file, w)
		}
	}
}

func TestSystemdUnitRunsServeAsDedicatedAccount(t *testing.T) {
	u := read(t, "systemd/apa.service")
	has(t, "apa.service", u, "ExecStart=/usr/local/bin/apa serve", "User=apa", "Environment=APA_HOME=", "Restart=on-failure", "WantedBy=multi-user.target")
	if regexp.MustCompile(`(?m)^User=root`).MatchString(u) {
		t.Error("不得以 root 运行")
	}
}

func TestFullProxySamplesTerminateTLSAndForwardIdentity(t *testing.T) {
	n := read(t, "nginx/apa.conf.example")
	has(t, "nginx", n, "listen 443 ssl", "return 301 https://", "proxy_pass http://127.0.0.1:7777",
		"proxy_set_header Host $host", "proxy_set_header X-Forwarded-Proto https")
	c := read(t, "caddy/Caddyfile.example")
	has(t, "caddy", c, "reverse_proxy 127.0.0.1:7777")
}

func TestIngestOnlySamplesExposeNothingElse(t *testing.T) {
	n := read(t, "nginx/apa-ingest-only.conf.example")
	if got := strings.Count(n, "proxy_pass"); got != 1 {
		t.Errorf("nginx 只应有一处转发，实际 %d", got)
	}
	has(t, "nginx-ingest", n, "location /ingest/ {", "limit_except POST", "location / {\n        return 404;", "proxy_set_header X-Forwarded-Proto https")
	c := read(t, "caddy/Caddyfile.ingest-only.example")
	if got := strings.Count(c, "reverse_proxy"); got != 1 {
		t.Errorf("caddy 只应有一处转发，实际 %d", got)
	}
	has(t, "caddy-ingest", c, "handle /ingest/* {", "respond 404")
}

// 样例不得写入真实地址或机器名：域名只能是 .invalid，IP 只能是回环或全接口占位。
func TestSamplesCarryNoRealHostsOrAddresses(t *testing.T) {
	host := regexp.MustCompile(`[a-z0-9-]+(\.[a-z0-9-]+)*\.(com|net|org|io|dev|cn|info|local|lan|internal)\b`)
	ip := regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
	err := filepathWalk(func(rel string, s string) {
		if m := host.FindString(s); m != "" && !strings.Contains(m, "example.invalid") && m != "gcr.io" {
			t.Errorf("%s 含疑似真实域名 %q", rel, m)
		}
		for _, a := range ip.FindAllString(s, -1) {
			if a != "127.0.0.1" && a != "0.0.0.0" {
				t.Errorf("%s 含非占位地址 %s", rel, a)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func filepathWalk(fn func(rel, content string)) error {
	return filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || strings.HasSuffix(p, ".md") {
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil {
			fn(p, string(b))
		}
		return err
	})
}

func TestContainerSampleRunsNonRootServeOnHostLoopback(t *testing.T) {
	d := read(t, "container/Dockerfile")
	has(t, "Dockerfile", d, "go build -o /out/apa ./cli/apa", "distroless/static-debian12:nonroot", "ENV APA_HOME=/data", `CMD ["serve"]`,
		"--chown=65532:65532 /data", "docker run -d --network host")
	// Web 前端经 go:embed 进二进制：npm 构建必须先于 go build，且产物拷入 go 构建阶段。
	npm, gob := strings.Index(d, "npm run build"), strings.Index(d, "go build")
	if npm < 0 || gob < 0 || npm > gob {
		t.Error("Dockerfile 必须在 go build 之前构建 Web 前端")
	}
	has(t, "Dockerfile", d, "COPY --from=web /src/web/dist ./web/dist")
	for _, bad := range []string{"EXPOSE", " -p ", "0.0.0.0"} {
		if strings.Contains(d, bad) {
			t.Errorf("容器样例不得发布端口或监听非回环（%q）：端口发布会让 apa 看不到回环来源", bad)
		}
	}
	if _, err := os.Stat("../../cli/apa/main.go"); err != nil {
		t.Errorf("Dockerfile 构建的入口不存在: %v", err)
	}
}

func startApa(t *testing.T, host string) (addr string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "apa")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cli/apa").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := filepath.Join(dir, "home")
	env := append(os.Environ(), "APA_HOME="+home)
	init := exec.Command(bin, "init", "--password-stdin")
	init.Env, init.Stdin = env, strings.NewReader("correct horse battery\n")
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	def, err := config.Load(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if h, _, _ := net.SplitHostPort(def.Listen); h != "127.0.0.1" {
		t.Fatalf("默认监听必须是回环，实际 %q", def.Listen)
	}
	l, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen: "+host+":"+port+"\npublic_url: https://apa.example.invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	srv := exec.Command(bin, "serve")
	srv.Env, srv.Stdout, srv.Stderr = env, &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Signal(os.Interrupt); _ = srv.Wait() })
	waitUp(t, "127.0.0.1:"+port, &logs)
	return "127.0.0.1:" + port
}

func login(t *testing.T, addr, forwarded string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/auth/login", strings.NewReader(`{"password":"correct horse battery"}`))
	req.Host = "apa.example.invalid" // 代理保留 Host
	req.Header.Set("Origin", "https://apa.example.invalid")
	req.Header.Set("Content-Type", "application/json")
	if forwarded != "" {
		req.Header.Set("X-Forwarded-Proto", forwarded)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return r
}

// 容器样例不发布端口的原因：来源不是回环时，即使带 X-Forwarded-Proto: https，apa 也拒绝登录。
func TestNonLoopbackSourceIsRefusedEvenBehindProxyHeader(t *testing.T) {
	var ext string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
			ext = ip.IP.String()
			break
		}
	}
	if ext == "" {
		t.Skip("没有非回环 IPv4 地址")
	}
	addr := startApa(t, "0.0.0.0")
	_, port, _ := net.SplitHostPort(addr)
	r := login(t, ext+":"+port, "https")
	if r.StatusCode != 403 || len(r.Cookies()) != 0 {
		t.Errorf("非回环来源应被拒绝: status=%d", r.StatusCode)
	}
}

// 真实 apa：init 写出默认回环监听；serve 在代理样例所带的头下发 Secure 会话 Cookie，
// 缺这个头则不发 Secure；/ingest/ 可达。
func TestProxyContractHoldsOnRealBinary(t *testing.T) {
	addr := startApa(t, "127.0.0.1")
	r := login(t, addr, "https")
	if r.StatusCode != 200 || len(r.Cookies()) != 1 || !r.Cookies()[0].Secure || !r.Cookies()[0].HttpOnly {
		t.Errorf("经代理头登录应得 Secure Cookie: status=%d cookies=%v", r.StatusCode, r.Cookies())
	}
	if r := login(t, addr, ""); r.StatusCode != 200 || len(r.Cookies()) != 1 || r.Cookies()[0].Secure {
		t.Errorf("无转发头时不应标 Secure（说明 Secure 来自代理头）: status=%d", r.StatusCode)
	}
	resp, err := http.Post("http://"+addr+"/ingest/src_none", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 0 || resp.StatusCode >= 500 {
		t.Errorf("/ingest/ 应由 apa 处理并返回 4xx，实际 %d", resp.StatusCode)
	}
}

func waitUp(t *testing.T, addr string, logs *bytes.Buffer) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("serve 未启动: %s", logs.String())
}
