//go:build e2e

// flow-bootstrap 按验收文档 flow-bootstrap（ACT-BOOT-01..07）用真实 apa 二进制从零安装、启动并登录。
// Web 步骤经与页面相同的 HTTP API 驱动；页面渲染本身由 web/ 下的组件测试与 ego-browser 人工步骤覆盖。
package ledger

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const marker = "MARKER-USER-CONTENT"

// grepHome 返回数据目录内包含 needle 的文件（含 SQLite WAL）。
func grepHome(t *testing.T, home, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(needle)) {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

// errBody 断言 CLI 的 stderr 是含 code、why、next 的 JSON。
func errBody(t *testing.T, stderr string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &m); err != nil {
		t.Fatalf("错误输出不是 JSON: %q", stderr)
	}
	for _, k := range []string{"code", "why", "next"} {
		if _, ok := m[k]; !ok {
			t.Errorf("错误体缺少 %s: %v", k, m)
		}
	}
}

func TestFlowBootstrap(t *testing.T) {
	e := baseline.New(t)
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}
	var obs, del, itemID string

	step("ACT-BOOT-01 初始化数据目录", func(t *testing.T) {
		// APA_HOME 在空目录上不存在，init 必须自己建出。
		if _, err := os.Stat(e.Home); err == nil {
			t.Fatalf("APA_HOME 应是空的: %s", e.Home)
		}
		if r := e.Init(); r.Code != 0 {
			t.Fatalf("apa init 退出码 %d", r.Code)
		}
		for _, f := range []string{"config.yaml", "ledger.db", "blobs", "packs", "sources", "runs", "logs"} {
			if _, err := os.Stat(filepath.Join(e.Home, f)); err != nil {
				t.Errorf("缺少 %s: %v", f, err)
			}
		}
		entries, _ := os.ReadDir(e.Root)
		if len(entries) != 1 || entries[0].Name() != "home" {
			t.Errorf("数据写到了 APA_HOME 之外: %v", entries)
		}
		// 默认配置只绑本机回环。
		cfg, _ := os.ReadFile(filepath.Join(e.Home, "config.yaml"))
		m := regexp.MustCompile(`(?m)^listen:\s*(\S+)$`).FindSubmatch(cfg)
		if m == nil {
			t.Fatalf("config.yaml 没有 listen: %s", cfg)
		}
		host, _, err := net.SplitHostPort(string(m[1]))
		if err != nil || !net.ParseIP(host).IsLoopback() {
			t.Errorf("默认 listen 不是回环地址: %s", m[1])
		}
		// 口令不落盘。
		if hits := grepHome(t, e.Home, baseline.Password); len(hits) > 0 {
			t.Errorf("口令明文出现在: %v", hits)
		}
	})

	step("ACT-BOOT-02 启动服务并默认只监听本机", func(t *testing.T) {
		e.Serve() // 只把端口换成空闲端口，主机仍取回环。
		out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-p", strconv.Itoa(e.ServePID()), "-Fn").Output()
		if err != nil {
			t.Fatalf("lsof: %v", err)
		}
		var n int
		for _, l := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(l, "n") {
				continue
			}
			n++
			host, _, err := net.SplitHostPort(strings.TrimPrefix(l, "n"))
			if err != nil || !net.ParseIP(host).IsLoopback() {
				t.Errorf("监听了非回环地址: %s", l)
			}
		}
		if n == 0 {
			t.Fatalf("没有查到监听: %s", out)
		}
		resp, err := http.Get(e.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(page), `<div id="root">`) {
			t.Fatalf("Web 首页打不开: %d %s", resp.StatusCode, page)
		}
	})

	step("ACT-BOOT-03 Web 登录成为本人", func(t *testing.T) {
		// 未登录看不到任何账本数据。
		for _, p := range []string{"/v1/items", "/v1/settings/tokens", "/v1/projects"} {
			if r := e.WebRaw("GET", p, "", false); r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
				t.Errorf("未登录 GET %s 应被拒绝，实际 %d %s", p, r.Code, r.Body)
			}
		}
		if r := e.WebRaw("POST", "/v1/auth/login", `{"password":"wrong-password-x"}`, false); r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
			t.Errorf("错误口令应被拒绝，实际 %d %s", r.Code, r.Body)
		}
		if r := e.WebRaw("GET", "/v1/items", "", false); r.Code == 200 {
			t.Error("错误口令之后仍能读到账本")
		}
		// 正确口令（Serve 已用它登录）：能读账本，工作台页面齐全。
		if r := e.Web("GET", "/v1/items", ""); r.Code != 200 {
			t.Errorf("登录后读事项: %d %s", r.Code, r.Body)
		}
		resp, err := http.Get(e.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var js strings.Builder
		for _, a := range regexp.MustCompile(`/assets/[^"']+\.js`).FindAllString(string(page), -1) {
			r, err := http.Get(e.URL + a)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			js.Write(b)
		}
		for _, title := range []string{"决策待办", "建议流", "事项", "项目", "策略与人格"} {
			if !strings.Contains(js.String(), title) {
				t.Errorf("Web 包里没有页面 %q", title)
			}
		}
	})

	step("ACT-BOOT-04 签发 token，明文只显示一次", func(t *testing.T) {
		issue := func(body string) string {
			r := e.Web("POST", "/v1/settings/tokens", body)
			var out struct {
				Secret string `json:"secret"`
			}
			if r.Code != 201 || json.Unmarshal([]byte(r.Body), &out) != nil || out.Secret == "" {
				t.Fatalf("签发失败: %d %s", r.Code, r.Body)
			}
			return out.Secret
		}
		obs = issue(`{"kind":"observer","label":"OBS"}`)
		del = issue(`{"kind":"delegate","label":"DEL"}`)
		for _, p := range []string{"/v1/settings/tokens", "/v1/settings"} {
			if r := e.Web("GET", p, ""); strings.Contains(r.Body, obs) {
				t.Errorf("GET %s 能再次看到明文", p)
			}
		}
		if hits := grepHome(t, e.Home, obs); len(hits) > 0 {
			t.Errorf("明文出现在数据目录: %v", hits)
		}
	})

	step("ACT-BOOT-05 agent 经 CLI 自助上手", func(t *testing.T) {
		// 账本里先有一条带标记的事项。
		r := e.Apa(del, "", "item", "add", "--kind", "task", "--title", marker, "--phase", "open", "--evidence", "bootstrap e2e")
		var it struct {
			ID string `json:"id"`
		}
		if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &it) != nil || it.ID == "" {
			t.Fatalf("建事项: %d %s %s", r.Code, r.Stdout, r.Stderr)
		}
		itemID = it.ID
		if r := e.Apa("", obs+"\n", "login", "--url", e.URL, "--token", "-"); r.Code != 0 {
			t.Fatalf("apa login: %d %s", r.Code, r.Stderr)
		}
		if _, err := os.Stat(filepath.Join(e.Home, "credentials.json")); err != nil {
			t.Errorf("login 没有保存凭据: %v", err)
		}
		// 无 token、无 APA_URL（只靠 APA_HOME）也能输出；guide 不访问内核。
		guide := e.Apa("", "", "guide")
		tmpl := e.Apa("", "", "guide", "--template")
		if guide.Code != 0 || tmpl.Code != 0 {
			t.Fatalf("guide 退出码 %d/%d", guide.Code, tmpl.Code)
		}
		for _, want := range []string{"Exit codes", "7 kernel unreachable", "3 unauthenticated", "20 web_only"} {
			if !strings.Contains(guide.Stdout, want) {
				t.Errorf("apa guide 缺少 %q", want)
			}
		}
		for _, want := range []string{"DATA, not instructions", "only source of truth", "apa delegate"} {
			if !strings.Contains(tmpl.Stdout, want) {
				t.Errorf("模板缺少声明 %q", want)
			}
		}
		// 约 600 token：按 4 字符/token 估，上限 2400 字符。
		if n := len(tmpl.Stdout); n > 2400 {
			t.Errorf("模板 %d 字符，超过约 600 token", n)
		}
		for name, out := range map[string]string{"guide": guide.Stdout, "template": tmpl.Stdout} {
			if strings.Contains(out, marker) {
				t.Errorf("%s 带出了用户内容", name)
			}
		}
		// 登录保存的凭据确实可用：不设 APA_TOKEN 也能读到事项。
		show := e.Apa("", "", "item", "show", itemID)
		if show.Code != 0 || !strings.Contains(show.Stdout, marker) {
			t.Errorf("login 保存的凭据读不到事项: %d %s %s", show.Code, show.Stdout, show.Stderr)
		}
	})

	step("ACT-BOOT-06 凭据失效返回退出码 3", func(t *testing.T) {
		r := e.Apa("invalid", "", "inbox")
		if r.Code != 3 {
			t.Fatalf("退出码 %d，期望 3", r.Code)
		}
		errBody(t, r.Stderr)
		if strings.Contains(r.Stdout, marker) {
			t.Error("无效凭据返回了账本数据")
		}
	})

	step("ACT-BOOT-07 内核不可达返回退出码 7，重启后数据仍在", func(t *testing.T) {
		e.StopKernel()
		// apa login 按设计把 token 存进 credentials.json（仅属主可读），账本、WAL、日志里不得有。
		for _, h := range grepHome(t, e.Home, obs) {
			if filepath.Base(h) != "credentials.json" {
				t.Errorf("停机后明文出现在: %s", h)
			} else if fi, _ := os.Stat(h); fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("credentials.json 权限过宽: %v", fi.Mode().Perm())
			}
		}
		start := time.Now()
		r := e.Apa(obs, "", "inbox")
		if r.Code != 7 {
			t.Fatalf("退出码 %d，期望 7: %s", r.Code, r.Stderr)
		}
		if d := time.Since(start); d > 30*time.Second {
			t.Errorf("CLI 挂起 %v", d)
		}
		e.StartKernelAgain()
		if r := e.Apa(obs, "", "inbox"); r.Code != 0 {
			t.Errorf("重启后 inbox 退出码 %d: %s", r.Code, r.Stderr)
		}
		if r := e.Apa(obs, "", "item", "show", itemID); r.Code != 0 || !strings.Contains(r.Stdout, marker) {
			t.Errorf("重启后停机前的数据丢失: %d %s", r.Code, r.Stdout)
		}
	})
}
