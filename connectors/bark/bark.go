// Package bark 是推送到苹果设备的渠道插件：调用 Bark 推送服务的 HTTP 接口（POST <服务器>/push，JSON），只用标准库。
// 能力都为假（只能看）：决策提醒带一次性表单链接，点通知即打开；web_only 的决策只带 Web 链接。
// 服务器回 200 只表示已交给苹果的推送服务，不代表本人看到。字段语义的实测项见附录 D.5（SPIKE-PUSH-*）。
package bark

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/connectors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
)

//go:embed connector.yaml
var declaration []byte

// Plugin 是交给外呼调用器的插件。
func Plugin() conn.Plugin {
	return conn.Plugin{Decl: connectors.MustParse(declaration), New: New}
}

// maxBody 是正文的截断长度（字符）。shortcut: 按推送载荷 4 KB 上限估的保守值，SPIKE-PUSH-PARAMS 实测后再调。
const maxBody = 600

type channel struct {
	server, key, user, pass, group string
	timeSensitive                  bool
	hc                             *http.Client
}

// New 按一次调用的配置构造插件实例；只在这次调用里存活。
func New(cfg conn.Config) (conn.Channel, error) {
	c := &channel{server: strings.TrimRight(cfg.Settings["server"], "/"), key: cfg.Secrets["device_key"],
		user: cfg.Settings["auth_user"], pass: cfg.Secrets["auth_secret"], group: cfg.Settings["group"],
		timeSensitive: cfg.Settings["time_sensitive"] == "true",
		hc: &http.Client{Timeout: 15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if c.server == "" {
		return nil, conn.Fail(conn.ProblemConfigInvalid, "server is not set")
	}
	if (c.user == "") != (c.pass == "") {
		return nil, conn.Fail(conn.ProblemConfigInvalid, "login name and login secret go together")
	}
	return c, nil
}

// push 是 POST /push 的请求体；用不上的字段不发。
type push struct {
	DeviceKey string `json:"device_key"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Group     string `json:"group,omitempty"`
	URL       string `json:"url,omitempty"`
	Level     string `json:"level,omitempty"`
	ID        string `json:"id,omitempty"`
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// Send 把中立消息渲染成一条推送：点通知打开表单链接（有时）或 Web 链接；决策可用时效性通知。
func (c *channel) Send(ctx context.Context, m conn.Message) (conn.Receipt, error) {
	body := m.Summary
	if m.Recommend != "" {
		body = strings.TrimSpace(body + "\n推荐：" + m.Recommend)
	}
	if body == "" {
		body = m.Title
	}
	p := push{DeviceKey: c.key, Title: m.Title, Body: truncate(body, maxBody), Group: c.group, URL: m.WebURL, ID: m.Key}
	if m.FormURL != "" {
		p.URL = m.FormURL
	}
	if m.Class == "decision" && c.timeSensitive {
		p.Level = "timeSensitive"
	}
	b, err := json.Marshal(p)
	if err != nil {
		return conn.Receipt{}, err
	}
	if err := c.do(ctx, http.MethodPost, "/push", b); err != nil {
		return conn.Receipt{}, err
	}
	return conn.Receipt{Ref: m.Key}, nil // 服务端不回消息标识；幂等键同时作为推送的 id
}

// Test 只查服务器可达与登录（GET /ping）；设备密钥对不对只有发一条才知道，由「发一条测试」覆盖。
func (c *channel) Test(ctx context.Context) error { return c.do(ctx, http.MethodGet, "/ping", nil) }

func (c *channel) do(ctx context.Context, method, path string, body []byte) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, rd)
	if err != nil {
		return conn.Fail(conn.ProblemConfigInvalid, "bad server address")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return conn.Fail(conn.ProblemUnreachable, "the server did not answer in time")
		}
		return conn.Fail(conn.ProblemUnreachable, "cannot reach the server")
	}
	defer resp.Body.Close()
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = json.Unmarshal(raw, &r)
	detail := fmt.Sprintf("HTTP %d %s", resp.StatusCode, truncate(r.Message, 200))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return conn.Fail(conn.ProblemAuthFailed, detail)
	case resp.StatusCode == http.StatusTooManyRequests:
		return conn.Fail(conn.ProblemRateLimited, detail)
	case resp.StatusCode >= 500:
		return conn.Fail(conn.ProblemUnreachable, detail)
	case resp.StatusCode >= 300:
		return conn.Fail(conn.ProblemRejected, detail) // 400 多是设备密钥不对
	case r.Code != 0 && r.Code != http.StatusOK:
		return conn.Fail(conn.ProblemRejected, detail)
	}
	return nil
}
