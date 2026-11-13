//go:build e2e

// Package flows 按验收文档 flow-web-login（ACT-LGN-01..13）与 flow-deploy-exposure（ACT-DPL-01、02、03、07..10）
// 用测试构建（apa_testclock）的真实 apa 二进制独立执行全流程。环境来自 flow-env-baseline 的共用脚本（tests/e2e/baseline）：
// 隔离 APA_HOME、真实 `apa serve`、测试时钟、渠道 ext 与机器人替身；不依赖真实模型端点、宿主、渠道账号。
//
// Web 步骤以本人的 Web 会话（独立 Cookie 罐，每个"浏览器配置"一份）请求 Web 页面调用的同一组 /v1 接口，
// 与其他 tests/e2e 流程一致；页面渲染由 web/src/features/settings-security 的组件测试覆盖。
// 来源地址：回环上的本机反向代理追加 X-Forwarded-For（内核只信回环来源），测试用它给每个步骤一个独立来源地址，
// 以免登录失败计数在步骤间互相污染；与 `curl --interface 127.0.0.n` 在内核看来等价（macOS 默认只配置 127.0.0.1）。
//
// 只改测试，不改产品代码。
package flows

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

var httpc = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type fx struct {
	*baseline.Env
	t *testing.T
}

// newFx 建一套环境：真实 apa init + serve，签发五个凭据。
func newFx(t *testing.T) *fx {
	t.Helper()
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	return &fx{e, t}
}

// resp 是一次 HTTP 调用的结果。
type resp struct {
	Code    int
	Body    string
	Header  http.Header
	Cookies []*http.Cookie
}

func (r resp) errCode() string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(r.Body), &e)
	if e.Error.Code != "" {
		return e.Error.Code
	}
	return e.Code
}

func send(t *testing.T, c *http.Client, method, url string, body string, hdr map[string]string) resp {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	r, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, string(b), r.Header, r.Cookies()}
}

// sess 是一个"浏览器配置"：独立的会话 Cookie 与 CSRF 令牌。
type sess struct {
	t     *testing.T
	base  string
	addr  string // 来源地址（X-Forwarded-For）
	agent string
	id    string // apa_session Cookie 值
	csrf  string
}

func (s *sess) hdr(extra map[string]string) map[string]string {
	h := map[string]string{"User-Agent": s.agent}
	if s.addr != "" {
		h["X-Forwarded-For"] = s.addr
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// tryLogin 以口令登录；成功时返回带会话的 sess。
func tryLogin(t *testing.T, base, addr, agent, password string) (*sess, resp) {
	t.Helper()
	s := &sess{t: t, base: base, addr: addr, agent: agent}
	body, _ := json.Marshal(map[string]string{"password": password})
	r := send(t, httpc, "POST", base+"/v1/auth/login", string(body), s.hdr(nil))
	if r.Code != 200 {
		return nil, r
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil || out.CSRF == "" {
		t.Fatalf("登录响应没有 csrf_token: %s", r.Body)
	}
	for _, c := range r.Cookies {
		if c.Name == "apa_session" {
			s.id = c.Value
		}
	}
	if s.id == "" {
		t.Fatalf("登录没有下发会话 Cookie")
	}
	s.csrf = out.CSRF
	return s, r
}

func mustLogin(t *testing.T, base, addr, agent, password string) *sess {
	t.Helper()
	s, r := tryLogin(t, base, addr, agent, password)
	if s == nil {
		t.Fatalf("登录 %s 失败: %d %s", addr, r.Code, r.Body)
	}
	return s
}

func (s *sess) do(method, path, body string) resp {
	s.t.Helper()
	return send(s.t, httpc, method, s.base+path, body, s.hdr(map[string]string{"Cookie": "apa_session=" + s.id, "X-CSRF-Token": s.csrf}))
}

// page 模拟"刷新任一页面"：带会话 Cookie 读一次 Web 接口，返回状态码。
func (s *sess) page() int { s.t.Helper(); return s.do("GET", "/v1/health", "").Code }

func (s *sess) want(code int, what string) {
	s.t.Helper()
	if got := s.page(); got != code {
		s.t.Fatalf("%s: 会话 %s 访问得到 %d，期望 %d", what, s.agent, got, code)
	}
}

func mustCode(t *testing.T, what string, r resp, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("%s: HTTP %d（期望 %d）%s", what, r.Code, want, r.Body)
	}
}

// ---- 命令行 ----

type cliOut struct {
	Code           int
	Stdout, Stderr string
}

// runBin 以 APA_HOME=home 运行二进制，stdin 为 in；超时视为失败。
func runBin(t *testing.T, bin, home, in string, args ...string) cliOut {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	cmd.Stdin = strings.NewReader(in)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return cliOut{code, so.String(), se.String()}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("%s %v 60 秒未退出", bin, args)
	}
	return cliOut{}
}

// ---- 账本与审计 ----

func (f *fx) ledgerPath() string { return filepath.Join(f.Home, "ledger.db") }

type ledgerEvent struct {
	Seq     int64
	Type    string
	Payload string
}

// ledgerEvents 直接读账本事件表（Event 的权威记录）。
func ledgerEvents(t *testing.T, home string) []ledgerEvent {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(home, "ledger.db")+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs, err := db.Query(`SELECT seq, type, payload FROM eventlog_event ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []ledgerEvent
	for rs.Next() {
		var e ledgerEvent
		var p []byte
		if err := rs.Scan(&e.Seq, &e.Type, &p); err != nil {
			t.Fatal(err)
		}
		e.Payload = string(p)
		out = append(out, e)
	}
	return out
}

func countType(evs []ledgerEvent, typ string) (n int) {
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return
}

func maxSeq(evs []ledgerEvent) int64 {
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].Seq
}

// apiEvents 经 GET /v1/events（审计页同源）翻页读完全部 Event 的原始 JSON 与类型计数。
func (f *fx) apiEvents() (raw string, types map[string]int, max int64) {
	f.t.Helper()
	types = map[string]int{}
	var all strings.Builder
	cursor := int64(0)
	for i := 0; i < 1000; i++ {
		r := f.APIRaw("GET", "/v1/events?limit=500&cursor="+itoa(cursor), "", f.OBS)
		if r.Code != 200 {
			f.t.Fatalf("GET /v1/events: %d %s", r.Code, r.Body)
		}
		all.WriteString(r.Body)
		var page struct {
			Events []struct {
				Seq  int64  `json:"seq"`
				Type string `json:"type"`
			} `json:"events"`
			Next int64 `json:"next"`
		}
		if err := json.Unmarshal([]byte(r.Body), &page); err != nil {
			f.t.Fatal(err)
		}
		for _, e := range page.Events {
			types[e.Type]++
			max = e.Seq
		}
		if len(page.Events) == 0 {
			break
		}
		cursor = page.Next
	}
	return all.String(), types, max
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ledgerBytes 是账本库文件（含 WAL）的全部字节，用来确认口令明文不落盘。
func ledgerBytes(t *testing.T, home string) string {
	t.Helper()
	var sb strings.Builder
	for _, n := range []string{"ledger.db", "ledger.db-wal"} {
		if b, err := os.ReadFile(filepath.Join(home, n)); err == nil {
			sb.Write(b)
		}
	}
	return sb.String()
}
