//go:build live

// Package deploy 按验收文档 flow-deploy-exposure（ACT-DPL-01..06）用真实 apa 二进制、真实 HTTPS 反向代理、
// 真实的"另一台机器"（经 ssh oracle 的 curl）和真实 Telegram 走部署暴露流程。
//
// 环境约定：
//   - 反向代理是本文件里的最小 Go 代理（net/http/httputil + crypto/tls，测试证书现场生成，IP SAN 为本机 tailnet 地址），
//     监听本机 tailnet 地址；apa 自己只监听回环。代理保留 Host，并为 HTTPS 请求带 X-Forwarded-Proto: https，
//     与 deploy/ 下 nginx、caddy 样例的契约一致。
//   - "另一台机器"是 oracle：只用 `ssh oracle 'curl ...'` 只读访问，不在 oracle 上安装、改配置或留文件。
//     证书不进 oracle：curl 用 --pinnedpubkey 钉住测试证书的公钥。
//   - ACT-DPL-04（中心 APA 加多台 harness 主机）首批不验：没有第二台可跑 Hermes 的主机。该步骤如实记为 skip，不是通过。
//   - ACT-DPL-05 的在线备份用 SQLite 的 VACUUM INTO（运行中导出一致快照）：内核没有对外的备份命令。
//   - ACT-DPL-06 的"上一发行版本"：仓库没有正式发行版，取 main 上第一个完整装配 apa serve 的提交（APA_LIVE_PREV_REV 可覆盖），
//     用 git archive 导出到临时目录构建，不改动工作区。
//
// 所有状态（APA_HOME、代理证书）都在 os.MkdirTemp 的 /tmp/apa-live.* 临时目录里，测后只删本测试创建的字面路径。
// Bot token 只从 APA_LIVE_TELEGRAM_BOT_TOKEN_FILE 读取，只放进 ext 子进程的环境，不进入输出。
package deploy

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

const (
	otherMachine = "oracle"
	channel      = "acc-chan"
	waitHuman    = 30 * time.Minute
	waitKern     = 3 * time.Minute
	prevRevDef   = "36adbe4f62aca47032b9bfa6ba15e73ddba5c587"
)

func repoRoot() string { d, _ := filepath.Abs(filepath.Join("..", "..", "..")); return d }

// tmpDir 在 /tmp 下建 apa-live.* 临时目录；清理只删这个字面路径。
func tmpDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "apa-live.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if strings.HasPrefix(d, "/tmp/apa-live.") {
			_ = os.RemoveAll(d)
		}
	})
	return d
}

// liveEnv 先看进程环境，没有就读 APA_LIVE_ENV_FILE（KEY=VALUE）。
func liveEnv(t *testing.T, k string) string {
	t.Helper()
	if v := os.Getenv(k); v != "" {
		return v
	}
	if f := os.Getenv("APA_LIVE_ENV_FILE"); f != "" {
		if fh, err := os.Open(f); err == nil {
			defer fh.Close()
			sc := bufio.NewScanner(fh)
			for sc.Scan() {
				if key, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && key == k {
					return strings.Trim(v, `"'`)
				}
			}
		}
	}
	t.Fatalf("缺少 %s（设置 APA_LIVE_ENV_FILE 指向 live.env）", k)
	return ""
}

// tailnetIP 返回本机 tailnet 地址（100.64.0.0/10）。
func tailnetIP(t *testing.T) string {
	t.Helper()
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && cgnat.Contains(ip.IP) {
			return ip.IP.String()
		}
	}
	t.Fatal("本机没有 tailnet 地址（100.64.0.0/10）")
	return ""
}

// cert 现场生成测试证书（IP SAN），返回证书与公钥指纹（curl --pinnedpubkey 用）。
func cert(t *testing.T, ip string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "apa-live-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(ip)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	spki, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	sum := sha256.Sum256(spki)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
}

// startProxy 在 tailnet 地址上起 HTTPS 反向代理，转发到 upstream（apa 的回环地址）。
// ingestOnly 为真时只转发 POST /ingest/*，其余一律 404（对应 deploy/ 的 ingest-only 样例）。
func startProxy(t *testing.T, ip, upstream string, c tls.Certificate, ingestOnly bool) (port string) {
	t.Helper()
	u, _ := url.Parse(upstream)
	rp := httputil.NewSingleHostReverseProxy(u) // 默认保留客户端 Host
	orig := rp.Director
	rp.Director = func(r *http.Request) {
		orig(r)
		r.Header.Del("X-Forwarded-Proto")
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	h := http.Handler(rp)
	if ingestOnly {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/ingest/") {
				rp.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		})
	}
	l, err := tls.Listen("tcp", ip+":0", &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ErrorLog: nil}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ = net.SplitHostPort(l.Addr().String())
	return port
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

type resp struct {
	Code    int
	Headers []string // 小写化的 "name: value"
	Body    string
}

func (r resp) header(name string) []string {
	var out []string
	for _, h := range r.Headers {
		if strings.HasPrefix(h, strings.ToLower(name)+":") {
			out = append(out, h[len(name)+1:])
		}
	}
	return out
}

// remote 在另一台机器（oracle）上跑 curl，访问 tailnet 上的代理。-k 配 --pinnedpubkey：只接受本测试现场生成的证书。
func remote(pin string, args ...string) (resp, error) {
	parts := []string{"curl", "-sk", "-i", "--max-time", "20", "--pinnedpubkey", q(pin)}
	for _, a := range args {
		parts = append(parts, q(a))
	}
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=20", otherMachine, strings.Join(parts, " "))
	out, err := cmd.Output()
	if err != nil {
		return resp{}, fmt.Errorf("ssh %s curl: %w (%s)", otherMachine, err, out)
	}
	return parseHTTP(string(out)), nil
}

func parseHTTP(s string) resp {
	head, body, _ := strings.Cut(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n")
	var r resp
	for i, l := range strings.Split(head, "\n") {
		if i == 0 {
			if f := strings.Fields(l); len(f) >= 2 {
				fmt.Sscan(f[1], &r.Code)
			}
			continue
		}
		if k, v, ok := strings.Cut(l, ":"); ok {
			r.Headers = append(r.Headers, strings.ToLower(k)+":"+strings.TrimSpace(v))
		}
	}
	r.Body = body
	return r
}

func (f *fx) must(args ...string) resp {
	f.t.Helper()
	r, err := remote(f.pin, args...)
	if err != nil {
		f.t.Fatalf("另一台机器访问失败: %v", err)
	}
	return r
}

type fx struct {
	*baseline.Env
	t    *testing.T
	pin  string
	cert tls.Certificate
	ip   string
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) until(limit time.Duration, what string, cond func() bool) {
	f.t.Helper()
	for end := time.Now().Add(limit); !cond(); time.Sleep(time.Second) {
		if time.Now().After(end) {
			f.t.Fatalf("%v 内没有等到：%s", limit, what)
		}
	}
}

func (f *fx) task(title string) string {
	f.t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建个任务")
	if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %v %s", err, r.Stdout)
	}
	return m.ID
}

func (f *fx) projectNames() string { return f.webOK("GET", "/projects", "", 200) }

const accPack = `name: acc-webhook
version: 1.0.0
description: 验收用 webhook 校验方案与映射（数据目录包，不属于发行）
source_rules:
  - id: acc-hook-created
    match: {source_type: acc-hook, event_type: [acc.created]}
    kind: task
    project_by: none
    then: proposed
webhook_schemes:
  - id: acc-token
    signature_header: X-Acc-Token
    algorithm: token
webhook_mappings:
  - id: acc-generic
    match: {header: {X-Acc-Event: created}}
    fields:
      id: "{$.id}"
      type: "acc.created"
      subject: "{$.title}"
`

func TestFlowDeployExposure(t *testing.T) {
	ip := tailnetIP(t)
	c, pin := cert(t, ip)
	e := baseline.New(t)
	e.Init()
	pk := filepath.Join(e.Home, "packs", "acc-webhook")
	if err := os.MkdirAll(pk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pk, "pack.yaml"), []byte(accPack), 0o644); err != nil {
		t.Fatal(err)
	}
	e.Serve()
	e.IssueCredentials()
	e.CreateProjects()
	f := &fx{Env: e, t: t, pin: pin, cert: c, ip: ip}
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}

	step("ACT-DPL-01 经 HTTPS 反向代理暴露，另一台机器访问 Web 与 API", func(t *testing.T) {
		upstream := e.URL
		if h, _, _ := net.SplitHostPort(strings.TrimPrefix(upstream, "http://")); h != "127.0.0.1" {
			t.Fatalf("apa 必须只监听回环: %s", upstream)
		}
		port := startProxy(t, ip, upstream, c, false)
		base := "https://" + ip + ":" + port
		t.Logf("代理 %s → apa %s；另一台机器 %s", base, upstream, otherMachine)

		// apa 自己的端口不对外：另一台机器直连 tailnet 地址上的 apa 端口应被拒绝。
		_, apaPort, _ := net.SplitHostPort(strings.TrimPrefix(upstream, "http://"))
		cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=20", otherMachine,
			"curl -s -o /dev/null --max-time 8 http://"+ip+":"+apaPort+"/")
		if err := cmd.Run(); err == nil {
			t.Errorf("apa 的回环端口不应能从另一台机器直连")
		}

		web := f.must(base + "/")
		if web.Code != 200 || !strings.Contains(strings.ToLower(web.Body), "<html") {
			t.Fatalf("另一台机器应能打开 Web: %d %.200s", web.Code, web.Body)
		}
		login := f.must("-X", "POST", base+"/v1/auth/login", "-H", "Origin: "+base, "-H", "Content-Type: application/json",
			"-d", `{"password":"`+baseline.Password+`"}`)
		if login.Code != 200 {
			t.Fatalf("经代理登录失败: %d %s", login.Code, login.Body)
		}
		var lb struct {
			CSRF string `json:"csrf_token"`
		}
		_ = json.Unmarshal([]byte(login.Body), &lb)
		var cookie string
		for _, sc := range login.header("set-cookie") {
			if strings.HasPrefix(sc, "apa_session=") {
				cookie, _, _ = strings.Cut(sc, ";")
				low := strings.ToLower(sc)
				if !strings.Contains(low, "; secure") || !strings.Contains(low, "; httponly") {
					t.Errorf("登录 Cookie 必须带 Secure 与 HttpOnly: %s", sc[:strings.Index(sc, "=")+1]+"<redacted>"+sc[strings.Index(sc, ";"):])
				} else {
					t.Logf("Set-Cookie 属性: %s", sc[:strings.Index(sc, "=")+1]+"<redacted>"+sc[strings.Index(sc, ";"):])
				}
			}
		}
		if cookie == "" || lb.CSRF == "" {
			t.Fatalf("登录没有返回会话 Cookie 或 csrf_token: %v", login.Headers)
		}
		write := func(origin, name string) resp {
			return f.must("-X", "POST", base+"/v1/projects", "-H", "Origin: "+origin, "-H", "Cookie: "+cookie,
				"-H", "X-CSRF-Token: "+lb.CSRF, "-H", "Content-Type: application/json", "-d", `{"name":"`+name+`"}`)
		}
		if r := write("https://evil.example.invalid", "dpl-cross-site"); r.Code != 403 {
			t.Errorf("跨站写请求应被拒绝（403），实际 %d %s", r.Code, r.Body)
		}
		if r := write(base, "dpl-same-site"); r.Code != 201 {
			t.Errorf("同源写请求应成功（对照），实际 %d %s", r.Code, r.Body)
		}
		names := f.projectNames()
		if strings.Contains(names, "dpl-cross-site") || !strings.Contains(names, "dpl-same-site") {
			t.Errorf("跨站写不得落账、同源写应落账: %s", names)
		}
		api := f.must(base+"/v1/projects", "-H", "Authorization: Bearer "+e.OBS)
		if api.Code != 200 || !strings.Contains(api.Body, "dpl-same-site") {
			t.Errorf("另一台机器应能用 token 访问 API: %d %.200s", api.Code, api.Body)
		}
	})

	step("ACT-DPL-02 只暴露 /ingest/ 也能工作", func(t *testing.T) {
		f.apa(f.DEL, 0, "source", "add", "--id", "src_dpl_hook", "--type", "webhook", "--source-type", "acc-hook",
			"--config", `{"scheme":"acc-token","mappings":["acc-generic"]}`, "--evidence", "owner asked")
		var k struct {
			Secret, URL string
		}
		if err := json.Unmarshal([]byte(f.apa(f.DEL, 0, "source", "webhook", "create", "src_dpl_hook", "--evidence", "owner asked").Stdout), &k); err != nil || k.Secret == "" {
			t.Fatalf("没有返回接收密钥: %v", err)
		}
		port := startProxy(t, ip, e.URL, c, true)
		base := "https://" + ip + ":" + port
		body := `{"id":"dpl-1","title":"[APA-LIVE-TEST] 外部 webhook"}`
		r := f.must("-X", "POST", base+k.URL, "-H", "X-Acc-Token: "+k.Secret, "-H", "X-Acc-Event: created",
			"-H", "Content-Type: application/json", "-d", body)
		if r.Code/100 != 2 {
			t.Fatalf("外部推 webhook 应回 2xx: %d %s", r.Code, r.Body)
		}
		for _, p := range [][]string{
			{base + "/"},
			{base + "/v1/projects", "-H", "Authorization: Bearer " + e.OBS},
			{"-X", "POST", base + "/v1/auth/login", "-H", "Content-Type: application/json", "-d", `{"password":"` + baseline.Password + `"}`},
			{base + k.URL}, // 非 POST 的 /ingest/ 也不转发
		} {
			if x := f.must(p...); x.Code != 404 {
				t.Errorf("外部访问 %v 应不通（404），实际 %d", p[0], x.Code)
			}
		}
		var ing struct {
			Ingests []struct {
				State, Reason string
				Signed        bool
				Events        int
			} `json:"ingests"`
		}
		f.until(waitKern, "入站记录处理完成", func() bool {
			_ = json.Unmarshal([]byte(f.apa(e.OBS, 0, "source", "ingests", "src_dpl_hook").Stdout), &ing)
			return len(ing.Ingests) == 1 && ing.Ingests[0].State == "processed"
		})
		if ing.Ingests[0].Events != 1 {
			t.Errorf("入站记录应转换出 1 个事件: %+v", ing.Ingests[0])
		}
		// 对照：经同一代理，错误的接收密钥被内核拒绝（401），说明校验在内核而不是代理。
		if x := f.must("-X", "POST", base+k.URL, "-H", "X-Acc-Token: apa_whk_wrong", "-H", "X-Acc-Event: created", "-d", body); x.Code != 401 {
			t.Errorf("错误密钥应回 401，实际 %d", x.Code)
		}
		f.until(waitKern, "来源事件进入建议流", func() bool {
			return strings.Contains(f.webOK("GET", "/sources/src_dpl_hook/events", "", 200), `"disposition":"proposed"`)
		})
		// 内部功能正常：回环上的 Web、CLI 都照常工作。
		if id := f.task("[APA-LIVE-TEST] 内部功能"); id == "" {
			t.Error("内部创建事项失败")
		}
	})

	step("ACT-DPL-03 渠道 ext 不需要公网地址（Telegram 主路径）", func(t *testing.T) {
		tokB, err := os.ReadFile(liveEnv(t, "APA_LIVE_TELEGRAM_BOT_TOKEN_FILE"))
		if err != nil {
			t.Fatal(err)
		}
		token, chat, botName := strings.TrimSpace(string(tokB)), liveEnv(t, "APA_LIVE_TELEGRAM_CHAT_ID"), liveEnv(t, "APA_LIVE_TELEGRAM_BOT")
		scrub := func(s string) string { return strings.ReplaceAll(s, token, "<bot-token>") }
		hr, err := http.Get("https://api.telegram.org/bot" + token + "/getChat?chat_id=" + chat)
		if err != nil {
			t.Fatal("getChat 请求失败（网络）")
		}
		var ch struct {
			OK     bool `json:"ok"`
			Result struct {
				Username string `json:"username"`
			} `json:"result"`
		}
		_ = json.NewDecoder(hr.Body).Decode(&ch)
		hr.Body.Close()
		if !ch.OK {
			t.Fatalf("getChat 失败 HTTP %d", hr.StatusCode)
		}
		answerer := ch.Result.Username
		if answerer == "" {
			answerer = chat
		}

		root := tmpDir(t)
		extBin := filepath.Join(root, "telegram-ext")
		b := exec.Command("go", "build", "-o", extBin, "./ext/telegram/cmd/telegram-ext")
		b.Dir = repoRoot()
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("构建 ext: %v\n%s", err, out)
		}
		extDir := filepath.Join(root, "ext")
		if err := os.MkdirAll(filepath.Join(extDir, "telegram"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(extDir, "telegram", "ext.yaml"),
			[]byte("kind: channel\ndescription: Telegram 渠道 ext\ncommand: ["+extBin+"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pol, _ := json.Marshal(map[string]any{"disturb": map[string]any{
			"routes": map[string]string{"decision": channel, "reminder": "web", "brief": "web", "health": "web", "wake": "web"}}})
		f.webOK("PUT", "/settings/policy", string(pol), 200)
		al, _ := json.Marshal(map[string]any{"channel_answerers": map[string][]string{channel: {answerer}}})
		f.webOK("PUT", "/settings/allowlists", string(al), 200)

		// apa 只监听回环；ext 与 apa 同机，只经回环 HTTP 连内核，向外长轮询 Telegram。
		var out bytes.Buffer
		cmd := exec.Command(e.Bin["apa"], "ext", "run", "telegram")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "APA_HOME=" + e.Home, "APA_EXT_DIR=" + extDir,
			"APA_URL=" + e.URL, "APA_EXT_KEY=" + e.CHAN, "APA_WEB_URL=" + e.URL, "TG_BOT=" + token, "TG_CHAT=" + chat,
			"APA_EXT_STATE=" + filepath.Join(root, "ext-telegram.json")}
		var mu sync.Mutex
		cmd.Stdout, cmd.Stderr = lockedWriter{&mu, &out}, lockedWriter{&mu, &out}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
			}
		})
		time.Sleep(3 * time.Second)
		select {
		case <-done:
			t.Fatalf("ext 启动后退出: %s", scrub(out.String()))
		default:
		}

		item := f.task("[APA-LIVE-TEST] 渠道不需要公网")
		f.apa(e.WRK, 0, "ask", item, "--question", "[APA-LIVE-TEST] 选哪个方案？请在 Telegram 点选项 A（验收步骤 ACT-DPL-03）",
			"--option", "A=方案A", "--option", "B=方案B", "--recommend", "A")
		db := func() (state, ref string) {
			d, err := sql.Open("sqlite", "file:"+filepath.Join(e.Home, "ledger.db")+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			_ = d.QueryRow(`SELECT state, external_ref FROM delivery_delivery WHERE item = ? ORDER BY created_at, rowid LIMIT 1`, item).Scan(&state, &ref)
			return
		}
		f.until(waitKern, "决策投递 delivered 且有 external_ref", func() bool { s, r := db(); return s == "delivered" && r != "" })
		_, ref := db()
		if !strings.HasPrefix(ref, chat+":") {
			t.Errorf("external_ref 应形如 <会话>:<消息id>: %q", ref)
		}

		// 没有入站连接：ext 进程（含子进程）不监听任何端口，也没有被连入的连接；apa 只在回环上监听。
		pids := []string{fmt.Sprint(cmd.Process.Pid)}
		if o, _ := exec.Command("pgrep", "-P", fmt.Sprint(cmd.Process.Pid)).Output(); len(o) > 0 {
			pids = append(pids, strings.Fields(string(o))...)
		}
		ls, _ := exec.Command("lsof", "-nP", "-a", "-p", strings.Join(pids, ","), "-iTCP", "-sTCP:LISTEN").Output()
		if strings.TrimSpace(string(ls)) != "" {
			t.Errorf("ext 不应监听任何端口:\n%s", ls)
		}
		apaLs, _ := exec.Command("lsof", "-nP", "-a", "-p", fmt.Sprint(e.ServePID()), "-iTCP", "-sTCP:LISTEN").Output()
		if !strings.Contains(string(apaLs), "127.0.0.1:") || strings.Contains(string(apaLs), "*:") || strings.Contains(string(apaLs), ip) {
			t.Errorf("apa 应只监听回环:\n%s", apaLs)
		}

		t.Logf("等待本人在 Telegram（@%s）点选项 A，最多 %v", botName, waitHuman)
		f.until(waitHuman, "本人在 Telegram 回答", func() bool {
			var o struct {
				Item map[string]any `json:"item"`
			}
			_ = json.Unmarshal([]byte(f.apa(e.OBS, 0, "item", "show", item).Stdout), &o)
			a, _ := o.Item["ask"].(map[string]any)
			return a["resolution"] != nil
		})
		var o struct {
			Item map[string]any `json:"item"`
		}
		_ = json.Unmarshal([]byte(f.apa(e.OBS, 0, "item", "show", item).Stdout), &o)
		a, _ := o.Item["ask"].(map[string]any)
		res, _ := a["resolution"].(map[string]any)
		if res["resolved_via"] != channel || res["choice"] != "A" || res["answered_by"] != answerer {
			t.Errorf("回答记录不符: %v（期望 resolved_via=%s choice=A answered_by=%s）", res, channel, answerer)
		}
		t.Logf("delivery external_ref=%s resolved_via=%v bot=@%s（无入站连接、无公网地址）", ref, res["resolved_via"], botName)
	})

	step("ACT-DPL-04 中心 APA 加多台 harness 主机", func(t *testing.T) {
		t.Skip("未执行：无第二台可跑 Hermes 的主机（首批范围调整，本人已定）；不记为通过")
	})
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// ledgerEvents 只读打开账本，按序返回事件的 seq|type|object。
func ledgerEvents(t *testing.T, home string) []string {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(`SELECT seq, type, object FROM eventlog_event ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s int64
		var ty, ob string
		if err := rows.Scan(&s, &ty, &ob); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d|%s|%s", s, ty, ob))
	}
	return out
}

func migrationCount(t *testing.T, home string) int {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM store_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// copyDir 整目录复制（含权限）；skip 返回真的相对路径不复制。
func copyDir(t *testing.T, src, dst string, skip func(rel string) bool) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return os.MkdirAll(dst, 0o700)
		}
		if skip(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), fi.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, fi.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeployBackupRestore(t *testing.T) {
	root := tmpDir(t)
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	e.CreateProjects()
	f := &fx{Env: e, t: t}

	// 工具适配来源：凭据在工具自己的目录（数据目录之外），换主机后需重新认证 → 退出码 2。
	toolDir := filepath.Join(root, "tool-auth")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, toolDir, "auth-ok", "ok")
	script := writeFile(t, root, "tool_source.sh", `#!/bin/sh
[ -f "$1/auth-ok" ] || exit 2
printf '%s\n' '{"id":"t1","type":"acc.event","subject":"tool event","occurred_at":"2030-01-01T00:00:00Z"}' '{"cursor":"c1"}'
`)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	reg, _ := json.Marshal(map[string]any{"id": "src_dpl_tool", "type": "script", "source_type": "acc",
		"command": "/bin/sh " + script + " " + toolDir, "schedule": "1s", "timeout": 30})
	f.webOK("POST", "/sources", string(reg), 201)

	item := f.task("[APA-LIVE-TEST] 备份前的事项")
	blobSrc := writeFile(t, root, "a.png", "\x89PNG dpl-blob-bytes")
	var blob struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(f.apa(e.WRK, 0, "blob", "put", blobSrc).Stdout), &blob); err != nil || len(blob.Hash) != 64 {
		t.Fatalf("blob put: %v", err)
	}
	matSrc := writeFile(t, root, "m.md", "[APA-LIVE-TEST] 材料正文 apa://blob/"+blob.Hash)
	var mat struct {
		Material string `json:"material"`
	}
	if err := json.Unmarshal([]byte(f.apa(e.WRK, 0, "material", "commit", "--from", matSrc, "--reason", "备份验收", "--kind", "worknote",
		"--title", "备份材料", "--project", e.Projects["acc-open"]).Stdout), &mat); err != nil || mat.Material == "" {
		t.Fatalf("material commit: %v", err)
	}
	f.until(waitKern, "工具来源首次拉取成功", func() bool {
		return strings.Contains(f.webOK("GET", "/sources/src_dpl_tool/events", "", 200), `"t1"`)
	})
	before := ledgerEvents(t, e.Home)

	// 运行中在线备份（apa serve 仍在运行）：导出一致快照，再整目录复制到新路径。
	snap := filepath.Join(root, "ledger.snapshot.db")
	d, err := sql.Open("sqlite", "file:"+filepath.Join(e.Home, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`VACUUM INTO ?`, snap); err != nil {
		t.Fatalf("在线备份失败: %v", err)
	}
	d.Close()
	if e.ServePID() == 0 {
		t.Fatal("备份时 apa serve 应仍在运行")
	}
	newHome := filepath.Join(root, "restored")
	copyDir(t, e.Home, newHome, func(rel string) bool {
		return rel == "ledger.db" || rel == "ledger.db-wal" || rel == "ledger.db-shm"
	})
	snapBytes, _ := os.ReadFile(snap)
	if err := os.WriteFile(filepath.Join(newHome, "ledger.db"), snapBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// 换主机：工具自己的认证不会跟着数据目录走。
	if err := os.Remove(filepath.Join(toolDir, "auth-ok")); err != nil {
		t.Fatal(err)
	}
	e.StopKernel()

	// 新路径启动：APA_HOME=<新路径> apa serve（监听改到新端口，其余沿用复制来的配置）。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	if err := os.WriteFile(filepath.Join(newHome, "config.yaml"), []byte("listen: 127.0.0.1:"+port+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	var mu sync.Mutex
	srv := exec.Command(e.Bin["apa"], "serve")
	srv.Env = append(os.Environ(), "APA_HOME="+newHome)
	srv.Stdout, srv.Stderr = lockedWriter{&mu, &logs}, lockedWriter{&mu, &logs}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = srv.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = srv.Process.Signal(syscall.SIGINT)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = srv.Process.Kill()
		}
	})
	url := "http://127.0.0.1:" + port
	cli := func(token string, want int, args ...string) baseline.Result {
		t.Helper()
		cmd := exec.Command(e.Bin["apa"], args...)
		cmd.Env = append(os.Environ(), "APA_HOME="+newHome, "APA_URL="+url, "APA_TOKEN="+token)
		var o, er bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &er
		code := 0
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		if code != want {
			t.Fatalf("apa %s 退出码 %d，期望 %d\n%s%s", strings.Join(args, " "), code, want, o.String(), er.String())
		}
		return baseline.Result{Code: code, Stdout: o.String(), Stderr: er.String()}
	}
	f.until(60*time.Second, "新路径的 apa serve 就绪", func() bool {
		select {
		case <-done:
			t.Fatalf("新路径 apa serve 提前退出: %s", logs.String())
		default:
		}
		r, err := http.Get(url + "/")
		if err == nil {
			r.Body.Close()
		}
		return err == nil
	})

	// 事项、Event、材料、blob 都在，apa why 正常。
	if r := cli(e.OBS, 0, "item", "show", item); !strings.Contains(r.Stdout, "备份前的事项") {
		t.Errorf("事项缺失: %s", r.Stdout)
	}
	after := ledgerEvents(t, newHome)
	if len(after) < len(before) {
		t.Fatalf("Event 缺失：备份前 %d 条，恢复后 %d 条", len(before), len(after))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("Event[%d] 变了: %s → %s", i, before[i], after[i])
			break
		}
	}
	if r := cli(e.OBS, 0, "material", "show", mat.Material); !strings.Contains(r.Stdout, "材料正文") {
		t.Errorf("材料缺失: %s", r.Stdout)
	}
	out := filepath.Join(root, "back.png")
	cli(e.OBS, 0, "blob", "get", blob.Hash, "--out", out)
	if b, _ := os.ReadFile(out); string(b) != "\x89PNG dpl-blob-bytes" {
		t.Errorf("blob 内容不一致: %q", b)
	}
	if r := cli(e.OBS, 0, "why", item); strings.TrimSpace(r.Stdout) == "" {
		t.Error("apa why 无输出")
	}
	// 工具适配来源需要重新认证：生成 needs_input，内核不崩溃，其他功能照常。
	f.until(waitKern, "工具来源因认证失效生成 needs_input", func() bool {
		select {
		case <-done:
			t.Fatalf("apa serve 崩溃: %s", logs.String())
		default:
		}
		return strings.Contains(cli(e.OBS, 0, "source", "show", "src_dpl_tool").Stdout, `"needs_input_item":"itm_`)
	})
	select {
	case <-done:
		t.Fatalf("apa serve 在认证失效后退出: %s", logs.String())
	default:
	}
	t.Logf("恢复后事件 %d 条（备份前 %d 条），材料 %s、blob %s…、事项 %s 都在；工具来源认证失效 → needs_input", len(after), len(before), mat.Material, blob.Hash[:8], item)
}

func TestDeployUpgradeMigratesSchema(t *testing.T) {
	root := tmpDir(t)
	rev := os.Getenv("APA_LIVE_PREV_REV")
	if rev == "" {
		rev = prevRevDef
	}
	// 上一版本：git archive 导出到临时目录构建，不动工作区。
	src := filepath.Join(root, "prev-src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	ar := exec.Command("sh", "-c", "git archive "+q(rev)+" | tar -x -C "+q(src))
	ar.Dir = repoRoot()
	if out, err := ar.CombinedOutput(); err != nil {
		t.Fatalf("git archive %s: %v\n%s", rev, err, out)
	}
	if err := os.MkdirAll(filepath.Join(src, "web", "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyDir(t, filepath.Join(repoRoot(), "web", "dist"), filepath.Join(src, "web", "dist"), func(string) bool { return false })
	prevBin := filepath.Join(root, "apa-prev")
	b := exec.Command("go", "build", "-o", prevBin, "./cli/apa")
	b.Dir = src
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("构建上一版本 %s: %v\n%s", rev, err, out)
	}

	e := baseline.New(t)
	newBin := e.Bin["apa"]
	e.Bin["apa"] = prevBin
	e.StartKernel()
	e.IssueCredentials()
	e.CreateProjects()
	f := &fx{Env: e, t: t}
	ids := []string{f.task("[APA-LIVE-TEST] 升级前事项一"), f.task("[APA-LIVE-TEST] 升级前事项二")}
	before := ledgerEvents(t, e.Home)
	migBefore := migrationCount(t, e.Home)
	if len(before) == 0 {
		t.Fatal("旧版本没有写入任何 Event")
	}
	e.StopKernel()

	// 换新版本启动同一个数据目录。
	e.Bin["apa"] = newBin
	e.StartKernelAgain()
	after := ledgerEvents(t, e.Home)
	if len(after) < len(before) {
		t.Fatalf("升级后 Event 变少: %d → %d", len(before), len(after))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("事件序号/内容变了 [%d]: %s → %s", i, before[i], after[i])
		}
	}
	migAfter := migrationCount(t, e.Home)
	if migAfter < migBefore {
		t.Errorf("迁移记录变少: %d → %d", migBefore, migAfter)
	}
	for _, id := range ids {
		var o struct {
			Item map[string]any `json:"item"`
		}
		r := f.apa(e.OBS, 0, "item", "show", id)
		if err := json.Unmarshal([]byte(r.Stdout), &o); err != nil || o.Item["id"] != id {
			t.Errorf("旧事项 %s 读不出: %v %s", id, err, r.Stdout)
		}
		if w := f.apa(e.OBS, 0, "why", id); strings.TrimSpace(w.Stdout) == "" {
			t.Errorf("旧事项 %s 的 apa why 无输出", id)
		}
	}
	// 新版本能继续写，序号接着旧的往后。
	f.task("[APA-LIVE-TEST] 升级后事项")
	last := ledgerEvents(t, e.Home)
	if len(last) <= len(after) {
		t.Error("升级后写入应追加 Event")
	}
	t.Logf("上一版本 %s：Event %d 条、迁移 %d 条；新版本启动后 Event 前缀不变、迁移 %d 条，旧事项可读", rev[:9], len(before), migBefore, migAfter)
}
