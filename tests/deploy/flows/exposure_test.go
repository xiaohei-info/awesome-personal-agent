//go:build e2e

package flows

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const proxyHost = "apa.example.invalid" // 代理域名（.invalid 保证不指向任何真实主机）

// serveBin 在 home 上启动 `apa serve` 并等端口可连；返回的 stop 可重复调用。
func serveBin(t *testing.T, bin, home, port string) (stop func()) {
	t.Helper()
	var logs bytes.Buffer
	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop = func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	t.Cleanup(stop)
	for i := 0; i < 300; i++ {
		select {
		case <-done:
			t.Fatalf("apa serve 提前退出: %s", logs.String())
		default:
		}
		if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
			c.Close()
			return stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("apa serve 30 秒内未监听 %s: %s", port, logs.String())
	return stop
}

// exposedFx 是监听全接口、public_url 为代理域名的部署（apa 自己不做 TLS）。
func exposedFx(t *testing.T) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	port := freePort(t)
	cfg := "listen: 0.0.0.0:" + port + "\npublic_url: https://" + proxyHost + "\n"
	if err := os.WriteFile(filepath.Join(e.Home, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	serveBin(t, e.Bin["apa"], e.Home, port)
	e.URL = "http://127.0.0.1:" + port // 本机内部入口
	e.WebLogin()
	e.IssueCredentials()
	return &fx{e, t}
}

// tlsProxy 是回环上的 HTTPS 反向代理（测试证书），行为与 deploy/ 下的 nginx、caddy 样例一致：
// 终止 TLS、保留 Host、带 X-Forwarded-Proto: https 并追加 X-Forwarded-For。onlyIngest 时只转发 POST /ingest/，其余 404。
func tlsProxy(t *testing.T, upstream string, onlyIngest bool) *httptest.Server {
	t.Helper()
	u, _ := url.Parse(upstream)
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(u)
		r.Out.Host = r.In.Host
		r.SetXForwarded()
		r.Out.Header.Set("X-Forwarded-Proto", "https")
	}}
	h := http.Handler(rp)
	if onlyIngest {
		mux := http.NewServeMux()
		mux.Handle("POST /ingest/", rp)
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
		h = mux
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fx) viaProxy(c *http.Client, proxy *httptest.Server, method, path, body string, hdr map[string]string) resp {
	f.t.Helper()
	h := map[string]string{"Host": proxyHost, "Origin": "https://" + proxyHost}
	for k, v := range hdr {
		h[k] = v
	}
	return send(f.t, c, method, proxy.URL+path, body, h)
}

const policyBody = `{"disturb":{"routes":{"decision":"web","reminder":"web","brief":"web","health":"web","wake":"web"},"merge_window_sec":2}}`

func TestFlowDeployExposure(t *testing.T) {
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		t.Run(name, fn)
	}

	step("ACT-DPL-01 经 HTTPS 反向代理暴露", func(t *testing.T) {
		f := exposedFx(t) // 非回环监听 + https public_url：服务启动即说明配置被接受
		proxy := tlsProxy(t, f.URL, false)
		c := proxy.Client() // 信任测试证书

		r := f.viaProxy(c, proxy, "POST", "/v1/auth/login", `{"password":"`+baseline.Password+`"}`, map[string]string{"X-Forwarded-For": "203.0.113.9"})
		mustCode(t, "经代理登录", r, 200)
		var ck *http.Cookie
		for _, x := range r.Cookies {
			if x.Name == "apa_session" {
				ck = x
			}
		}
		if ck == nil || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
			t.Fatalf("Web 登录 Cookie 应带 Secure、HttpOnly、SameSite=Strict: %+v", ck)
		}
		var tok struct {
			CSRF string `json:"csrf_token"`
		}
		_ = json.Unmarshal([]byte(r.Body), &tok)
		authed := map[string]string{"Cookie": "apa_session=" + ck.Value, "X-CSRF-Token": tok.CSRF}
		with := func(extra map[string]string) map[string]string {
			m := map[string]string{}
			for k, v := range authed {
				m[k] = v
			}
			for k, v := range extra {
				m[k] = v
			}
			return m
		}
		mustCode(t, "同源写请求（对照）", f.viaProxy(c, proxy, "PUT", "/v1/settings/policy", policyBody, authed), 200)
		// 跨站写请求被拒绝：外站 Origin；不带 CSRF 令牌。
		if r := f.viaProxy(c, proxy, "PUT", "/v1/settings/policy", policyBody, with(map[string]string{"Origin": "https://evil.invalid"})); r.Code != 403 {
			t.Fatalf("外站 Origin 的写请求应被拒绝: %d %s", r.Code, r.Body)
		}
		if r := f.viaProxy(c, proxy, "PUT", "/v1/settings/policy", policyBody, map[string]string{"Cookie": authed["Cookie"]}); r.Code != 403 {
			t.Fatalf("缺 CSRF 令牌的写请求应被拒绝: %d %s", r.Code, r.Body)
		}
		if r := f.viaProxy(c, proxy, "POST", "/v1/auth/login", `{"password":"`+baseline.Password+`"}`, map[string]string{"Origin": "https://evil.invalid"}); r.Code != 403 || len(r.Cookies) != 0 {
			t.Fatalf("跨站登录应被拒绝且不发会话: %d %v", r.Code, r.Cookies)
		}
		// API 同样经代理可达（Bearer 凭据）。
		if r := f.viaProxy(c, proxy, "GET", "/v1/health", "", map[string]string{"Authorization": "Bearer " + f.OBS}); r.Code != 200 {
			t.Fatalf("经代理访问 API: %d %s", r.Code, r.Body)
		}
		// 绕过代理、从非本机来源明文直连时不发会话（有非回环地址才能验）。
		if ext := nonLoopbackIP(); ext != "" {
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.URL, "http://"))
			r := send(t, httpc, "POST", "http://"+ext+":"+port+"/v1/auth/login", `{"password":"`+baseline.Password+`"}`, map[string]string{"Host": proxyHost})
			if r.Code != 403 || len(r.Cookies) != 0 {
				t.Fatalf("非本机来源的明文登录应被拒绝: %d %s", r.Code, r.Body)
			}
		} else {
			t.Log("没有非回环 IPv4 地址，跳过明文直连的对照断言（apa 单元测试 TestNonLoopbackSourceIsRefusedEvenBehindProxyHeader 同样依赖它）")
		}
	})

	step("ACT-DPL-02 只暴露 /ingest/ 也能工作", func(t *testing.T) {
		f := exposedFx(t)
		proxy := tlsProxy(t, f.URL, true)
		c := proxy.Client()

		const src = "src_dpl_gh"
		f.Apa(f.DEL, "", "source", "add", "--id", src, "--type", "webhook", "--source-type", "github",
			"--config", `{"scheme":"hmac-sha256-header","mappings":["github-issues"]}`, "--evidence", "owner asked")
		kr := f.Apa(f.DEL, "", "source", "webhook", "create", src, "--evidence", "owner asked")
		var key struct {
			Secret string `json:"secret"`
			URL    string `json:"url"`
		}
		if kr.Code != 0 || json.Unmarshal([]byte(kr.Stdout), &key) != nil || key.Secret == "" || key.URL != "/ingest/"+src {
			t.Fatalf("创建接收密钥: %+v", kr)
		}
		body, _ := json.Marshal(map[string]any{"action": "opened",
			"issue":      map[string]any{"number": 7, "title": "登录页 500", "body": "点击登录后 500", "html_url": "https://example.invalid/acc/test-repo/issues/7", "updated_at": "2026-11-01T08:00:00Z"},
			"repository": map[string]any{"full_name": "acc/test-repo"}, "sender": map[string]any{"login": "alice"}})
		m := hmac.New(sha256.New, []byte(key.Secret))
		m.Write(body)
		r := f.viaProxy(c, proxy, "POST", "/ingest/"+src, string(body), map[string]string{"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(m.Sum(nil)), "X-GitHub-Event": "issues", "X-GitHub-Delivery": "dpl-1"})
		if r.Code < 200 || r.Code >= 300 {
			t.Fatalf("外部经代理推 webhook 应得 2xx: %d %s", r.Code, r.Body)
		}
		// 外部访问 Web 与 API 不通。
		for _, p := range []struct{ m, path string }{{"GET", "/"}, {"GET", "/v1/health"}, {"POST", "/v1/auth/login"}, {"GET", "/v1/events"}} {
			hdr := map[string]string{"Authorization": "Bearer " + f.OBS}
			if rr := f.viaProxy(c, proxy, p.m, p.path, "", hdr); rr.Code != 404 {
				t.Fatalf("只转发 /ingest/ 时 %s %s 应不通(404): %d", p.m, p.path, rr.Code)
			}
		}
		if rr := f.viaProxy(c, proxy, "GET", "/ingest/"+src, "", nil); rr.Code != 404 {
			t.Fatalf("/ingest/ 只放行 POST: %d", rr.Code)
		}
		// 内部功能正常：本机登录 Web、CLI 读到刚落盘的入站记录。
		f.WebLogin()
		ir := f.Apa(f.OBS, "", "source", "ingests", src)
		var out struct {
			Ingests []struct {
				DeliveryKey string `json:"delivery_key"`
				Signed      bool   `json:"signed"`
			} `json:"ingests"`
		}
		if ir.Code != 0 || json.Unmarshal([]byte(ir.Stdout), &out) != nil || len(out.Ingests) != 1 || !out.Ingests[0].Signed || out.Ingests[0].DeliveryKey != "dpl-1" {
			t.Fatalf("经代理推送的 webhook 应已落盘为一条验签通过的入站记录: %+v %s", ir, ir.Stdout)
		}
		if n := f.countItems(); n != 0 {
			t.Logf("入站事件生成了 %d 个事项", n)
		}
	})

	step("ACT-DPL-03 渠道 ext 不需要公网地址", func(t *testing.T) {
		e := baseline.Setup(t) // APA 只监听回环
		cfg, err := os.ReadFile(filepath.Join(e.Home, "config.yaml"))
		if err != nil || !strings.Contains(string(cfg), "listen: 127.0.0.1:") || strings.Contains(string(cfg), "public_url") {
			t.Fatalf("前提：只监听本机且没有 public_url: %q %v", cfg, err)
		}
		bot := e.StartChannelExt() // ext 在同机运行，只主动连内核与机器人替身
		id := e.AskDecision("dpl-03 决策")
		var msg *struct{ ID int }
		if !e.WaitFor(60*time.Second, func() bool {
			for _, s := range bot.Sent() {
				if strings.Contains(s.Text, "dpl-03 决策") {
					msg = &struct{ ID int }{s.ID}
					return true
				}
			}
			return false
		}) {
			t.Fatalf("决策没有经渠道 ext 到达机器人替身: %+v", bot.Sent())
		}
		if r := e.Web("PUT", "/v1/settings/allowlists", `{"channel_answerers":{"acc-chan":["owner"]}}`); r.Code != 200 {
			t.Fatalf("登记渠道答复人: %d %s", r.Code, r.Body)
		}
		bot.Press(1, msg.ID, models.User{ID: 7, Username: "owner"}, "0")
		resolved := func() (choice string) {
			var w struct {
				Item struct {
					Ask struct {
						Resolution *struct {
							Choice string `json:"choice"`
						} `json:"resolution"`
					} `json:"ask"`
				} `json:"item"`
			}
			if r := e.Web("GET", "/v1/items/"+id, ""); r.Code == 200 && json.Unmarshal([]byte(r.Body), &w) == nil && w.Item.Ask.Resolution != nil {
				return w.Item.Ask.Resolution.Choice
			}
			return ""
		}
		if !e.WaitFor(60*time.Second, func() bool { return resolved() != "" }) {
			t.Fatalf("经机器人替身回答后决策没有被内核采纳: %+v", bot.Sent())
		}
		if got := resolved(); got != "A" {
			t.Fatalf("点第一个按钮应选中选项 A，实际 %q", got)
		}
	})

	exposureConfig := func(t *testing.T) (e *baseline.Env, port string, run func(args ...string) cliOut, cfg func(string)) {
		e = baseline.New(t)
		e.Init()
		port = freePort(t)
		cfg = func(body string) {
			if err := os.WriteFile(filepath.Join(e.Home, "config.yaml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return e, port, func(args ...string) cliOut { return runBin(t, e.Bin["apa"], e.Home, "", args...) }, cfg
	}
	portFree := func(t *testing.T, port string) {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			t.Fatalf("被拒绝的 serve 不应占用端口: %v", err)
		}
		l.Close()
	}

	step("ACT-DPL-07 监听非本机地址而没有 public_url 时拒绝启动", func(t *testing.T) {
		_, port, run, cfg := exposureConfig(t)
		cfg("listen: 0.0.0.0:" + port + "\n")
		r := run("serve")
		if r.Code == 0 || !strings.Contains(r.Stderr, "public_url") || !strings.Contains(r.Stderr, "https") {
			t.Fatalf("应非 0 退出并说明需要 https 的 public_url: %+v", r)
		}
		portFree(t, port)
	})

	step("ACT-DPL-08 public_url 不是 https 时同样拒绝启动", func(t *testing.T) {
		_, port, run, cfg := exposureConfig(t)
		cfg("listen: 0.0.0.0:" + port + "\npublic_url: http://" + proxyHost + "\n")
		r := run("serve")
		if r.Code == 0 || !strings.Contains(r.Stderr, "https") {
			t.Fatalf("应拒绝启动并说明 public_url 必须是 https: %+v", r)
		}
		portFree(t, port)
	})

	step("ACT-DPL-09 只监听本机时不要求 public_url", func(t *testing.T) {
		e, port, _, cfg := exposureConfig(t)
		cfg("listen: 127.0.0.1:" + port + "\n")
		serveBin(t, e.Bin["apa"], e.Home, port)
		s, r := tryLogin(t, "http://127.0.0.1:"+port, "", "local", baseline.Password)
		if s == nil {
			t.Fatalf("本机应可登录 Web: %d %s", r.Code, r.Body)
		}
	})

	step("ACT-DPL-10 自检报告缺少 public_url", func(t *testing.T) {
		_, port, run, cfg := exposureConfig(t)
		cfg("listen: 0.0.0.0:" + port + "\n") // 服务未启动
		r := run("doctor", "--json")
		var rep struct {
			Checks []struct{ Name, Status, Why string } `json:"checks"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &rep); err != nil {
			t.Fatalf("doctor --json 输出不是 JSON: %v\n%+v", err, r)
		}
		found := false
		for _, c := range rep.Checks {
			if c.Name == "config" {
				found = true
				if c.Status != "error" || !strings.Contains(c.Why, "public_url") {
					t.Fatalf("运行配置项应为错误级并说明缺少 https 的 public_url: %+v", c)
				}
			}
		}
		if !found || r.Code != 1 {
			t.Fatalf("应有运行配置项且退出码为 1: found=%v code=%d", found, r.Code)
		}
	})
}

func nonLoopbackIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() {
			return ip.IP.String()
		}
	}
	return ""
}
