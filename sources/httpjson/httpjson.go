// Package httpjson 是通用 HTTP JSON 轮询程序（设计 §9.1、§5.7）：
// 按 Source 执行契约（stdin JSON → 退出码 + stdout 标准事件 JSONL，末行新游标）
// GET config.url，按 config 里声明式的字段路径映射成标准事件。映射只是数据，不执行脚本；
// 程序不写账本、不访问内核存储，凭据只从来源自己目录里的文件读取。
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// 退出码沿用 §9.2 的 Source 契约。
const (
	ExitOK        = 0
	ExitTemporary = 1 // 5xx、429、网络错误、超时、响应不是 JSON：退避重试
	ExitAuth      = 2 // 401/403、其他 4xx、配置或映射不合法：生成 needs_input
)

const (
	maxBody        = 8 << 20
	defaultTimeout = 30 * time.Second
)

// Input 是 stdin 的 Source 契约输入。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Config   Config `json:"config"`
	Cursor   string `json:"cursor"`
	Window   struct {
		To string `json:"to"`
	} `json:"window"`
}

// Config 是声明式配置。路径是以 "." 分隔的对象键（数组下标用数字），不是脚本。
type Config struct {
	URL         string  `json:"url"`
	ItemsPath   string  `json:"items_path,omitempty"`   // 空表示响应本身就是数组
	CursorParam string  `json:"cursor_param,omitempty"` // 非空时把游标作为该查询参数发给服务端
	TokenFile   string  `json:"token_file,omitempty"`   // 来源自己目录里的凭据文件，内容是 bearer token
	TimeoutSec  int     `json:"timeout_sec,omitempty"`
	EventType   string  `json:"event_type,omitempty"` // mapping.type 缺省时使用的固定类型
	Mapping     Mapping `json:"mapping"`
}

// Mapping 把一个响应条目映射到标准事件字段，值都是条目内的路径。
type Mapping struct {
	ID              string   `json:"id"`
	Type            string   `json:"type,omitempty"`
	Subject         string   `json:"subject"`
	OccurredAt      string   `json:"occurred_at"` // RFC3339 字符串或 Unix 秒
	Summary         string   `json:"summary,omitempty"`
	Detail          string   `json:"detail,omitempty"`
	Link            string   `json:"link,omitempty"`
	Sender          string   `json:"sender,omitempty"`
	CorrelationKeys []string `json:"correlation_keys,omitempty"`
}

type event struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Subject         string   `json:"subject"`
	Summary         string   `json:"summary,omitempty"`
	Detail          string   `json:"detail,omitempty"`
	Link            string   `json:"link,omitempty"`
	OccurredAt      string   `json:"occurred_at"`
	CorrelationKeys []string `json:"correlation_keys,omitempty"`
	Sender          string   `json:"sender,omitempty"`
}

type runErr struct {
	exit int
	msg  string
}

func (e *runErr) Error() string { return e.msg }

func fail(exit int, f string, a ...any) error { return &runErr{exit, fmt.Sprintf(f, a...)} }

// Run 执行一次轮询并返回退出码；事件与游标写 stdout，诊断写 stderr。失败时 stdout 无内容。
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(ctx, stdin, stdout, stderr, http.DefaultClient, time.Now)
}

func run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, hc *http.Client, now func() time.Time) int {
	out, err := poll(ctx, stdin, hc, now)
	if err != nil {
		fmt.Fprintln(stderr, err)
		var re *runErr
		if errors.As(err, &re) {
			return re.exit
		}
		return ExitTemporary
	}
	_, _ = stdout.Write(out)
	return ExitOK
}

func poll(ctx context.Context, stdin io.Reader, hc *http.Client, now func() time.Time) ([]byte, error) {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, fail(ExitAuth, "stdin 不是合法的 JSON: %v", err)
	}
	cfg := in.Config
	if err := validate(cfg); err != nil {
		return nil, err
	}
	body, err := fetch(ctx, hc, cfg, in.Cursor)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fail(ExitTemporary, "响应不是合法 JSON: %v", err)
	}
	items, err := itemsOf(doc, cfg.ItemsPath)
	if err != nil {
		return nil, err
	}

	var floor time.Time
	if in.Cursor != "" {
		if floor, err = time.Parse(time.RFC3339Nano, in.Cursor); err != nil {
			return nil, fail(ExitAuth, "游标不是 RFC3339 时间: %q", in.Cursor)
		}
	}
	newest := floor
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for i, it := range items {
		ev, at, err := mapItem(cfg, it)
		if err != nil {
			return nil, fail(ExitAuth, "第 %d 条无法按 mapping 映射: %v", i, err)
		}
		if at.Before(floor) { // 等于游标的保留，由内核按事件 id 去重，避免同一时刻晚到的事件丢失
			continue
		}
		if at.After(newest) {
			newest = at
		}
		_ = enc.Encode(ev)
	}

	cursor := in.Cursor
	switch {
	case !newest.IsZero():
		cursor = newest.UTC().Format(time.RFC3339Nano)
	case cursor == "" && in.Window.To != "":
		cursor = in.Window.To
	case cursor == "":
		cursor = now().UTC().Format(time.RFC3339Nano)
	}
	_ = enc.Encode(map[string]string{"cursor": cursor})
	return buf.Bytes(), nil
}

func validate(c Config) error {
	u, err := url.Parse(c.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return fail(ExitAuth, "config.url 必须是 http(s) 地址")
	case c.Mapping.ID == "" || c.Mapping.Subject == "" || c.Mapping.OccurredAt == "":
		return fail(ExitAuth, "config.mapping 必须声明 id、subject、occurred_at")
	case c.Mapping.Type == "" && c.EventType == "":
		return fail(ExitAuth, "config 必须声明 mapping.type 或 event_type")
	}
	return nil
}

func fetch(ctx context.Context, hc *http.Client, c Config, cursor string) ([]byte, error) {
	u, _ := url.Parse(c.URL) // validate 已确认
	if c.CursorParam != "" && cursor != "" {
		q := u.Query()
		q.Set(c.CursorParam, cursor)
		u.RawQuery = q.Encode()
	}
	timeout := defaultTimeout
	if c.TimeoutSec > 0 {
		timeout = time.Duration(c.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fail(ExitAuth, "构造请求失败: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenFile != "" {
		tok, err := os.ReadFile(c.TokenFile)
		if err != nil || strings.TrimSpace(string(tok)) == "" {
			return nil, fail(ExitAuth, "凭据文件不可读或为空: %s", c.TokenFile)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fail(ExitTemporary, "请求失败: %v", err)
	}
	defer resp.Body.Close()
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
	case s >= 500 || s == http.StatusTooManyRequests || s == http.StatusRequestTimeout:
		return nil, fail(ExitTemporary, "服务端返回 %d", s)
	default: // 401/403 认证失败；其余 4xx 多为地址或参数配置有误，同样要本人修正
		return nil, fail(ExitAuth, "服务端返回 %d", s)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fail(ExitTemporary, "读取响应失败: %v", err)
	}
	if len(body) > maxBody {
		return nil, fail(ExitTemporary, "响应超过 %d 字节", maxBody)
	}
	return body, nil
}

func itemsOf(doc any, path string) ([]any, error) {
	v, ok := lookup(doc, path)
	if !ok {
		return nil, fail(ExitAuth, "响应里没有 items_path %q", path)
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fail(ExitAuth, "items_path %q 指向的不是数组", path)
	}
	return items, nil
}

func mapItem(c Config, it any) (event, time.Time, error) {
	m := c.Mapping
	var ev event
	var err error
	str := func(name, path string, required bool) string {
		if path == "" || err != nil {
			return ""
		}
		v, ok := lookup(it, path)
		s := scalar(v)
		if required && (!ok || s == "") {
			err = fmt.Errorf("缺少 %s（路径 %q）", name, path)
		}
		return s
	}
	ev.ID = str("id", m.ID, true)
	ev.Subject = str("subject", m.Subject, true)
	ev.Type = c.EventType
	if m.Type != "" {
		ev.Type = str("type", m.Type, true)
	}
	ev.Summary = str("summary", m.Summary, false)
	ev.Detail = str("detail", m.Detail, false)
	ev.Link = str("link", m.Link, false)
	ev.Sender = str("sender", m.Sender, false)
	for _, p := range m.CorrelationKeys {
		if k := str("correlation_keys", p, false); k != "" {
			ev.CorrelationKeys = append(ev.CorrelationKeys, k)
		}
	}
	raw := str("occurred_at", m.OccurredAt, true)
	if err != nil {
		return ev, time.Time{}, err
	}
	var at time.Time
	if secs, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
		at = time.Unix(secs, 0)
	} else if at, perr = time.Parse(time.RFC3339Nano, raw); perr != nil {
		return ev, at, fmt.Errorf("occurred_at 既不是 RFC3339 也不是 Unix 秒: %q", raw)
	}
	ev.OccurredAt = at.UTC().Format(time.RFC3339Nano)
	return ev, at, nil
}

// lookup 沿 "." 分隔的路径取值；空路径返回文档本身。
func lookup(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	for _, k := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = t[k]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// scalar 把标量转成字符串；对象、数组和 null 视为没有值。
func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}
