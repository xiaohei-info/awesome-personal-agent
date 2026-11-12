package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config 是转发配置。Idle 是无事件时两次轮询的间隔，Backoff/MaxBackoff 是投递失败后指数退避的起点与上限。
type Config struct {
	URL, Key, Target, Secret, State string
	Wait                            int // 长轮询秒数
	// Types 非空时只转发这些事件类型；其余事件不投递，但游标照常越过并落盘。空表示转发全部登记类型（缺省）。
	Types                     []string
	Idle, Backoff, MaxBackoff time.Duration
}

// ConfigFromEnv 读取环境变量；缺任何一项都是配置错误。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	c := Config{URL: getenv("APA_URL"), Key: getenv("APA_EXT_KEY"), Target: getenv("APA_WEBHOOK_URL"),
		Secret: getenv("APA_WEBHOOK_SECRET"), State: getenv("APA_WEBHOOK_STATE"),
		Wait: 50, Idle: time.Second, Backoff: time.Second, MaxBackoff: 5 * time.Minute}
	for _, t := range strings.Split(getenv("APA_WEBHOOK_TYPES"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.Types = append(c.Types, t)
		}
	}
	for _, p := range [][2]string{{"APA_URL", c.URL}, {"APA_EXT_KEY", c.Key}, {"APA_WEBHOOK_URL", c.Target},
		{"APA_WEBHOOK_SECRET", c.Secret}, {"APA_WEBHOOK_STATE", c.State}} {
		if p[1] == "" {
			return Config{}, errors.New(p[0] + " is required")
		}
	}
	return c, nil
}

// statusError 是内核的非 2xx 回应。
type statusError struct {
	path string
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d %s", e.path, e.code, e.body)
}

type forwarder struct {
	Config
	http *http.Client
}

// Run 订阅并转发，直到 ctx 取消（返回 nil）或遇到不可恢复的错误（凭据被拒等内核 4xx、游标文件损坏）。
// 内核网络错误与 5xx 退避重试。
func Run(ctx context.Context, cfg Config) error {
	f := &forwarder{Config: cfg, http: &http.Client{
		Timeout: time.Duration(cfg.Wait+30) * time.Second,
		// 3xx 不是确认：跟随会把 POST 改成 GET，终点的 2xx 会被误当作送达。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	cursor, err := readCursor(cfg.State)
	if err != nil {
		return err
	}
	wait := cfg.Backoff
	for ctx.Err() == nil {
		next, n, err := f.step(ctx, cursor)
		cursor = next
		switch {
		case err == nil:
			wait = cfg.Backoff
			if n == 0 {
				sleep(ctx, cfg.Idle)
			}
		case ctx.Err() != nil:
		case retryable(err):
			fmt.Fprintln(os.Stderr, "webhookforward: retrying:", err)
			sleep(ctx, wait)
			wait = min(wait*2, cfg.MaxBackoff)
		default:
			return err
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// deliveryError 是接收方不确认（非 2xx 或连不上），一律重试，不丢事件。
type deliveryError struct{ err error }

func (e *deliveryError) Error() string { return "deliver: " + e.err.Error() }

func retryable(err error) bool {
	var de *deliveryError
	if errors.As(err, &de) {
		return true
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

// step 读一页事件，逐条投递；每条被确认后立即落盘游标。返回当前游标和本页事件数。
func (f *forwarder) step(ctx context.Context, cursor int64) (int64, int, error) {
	var page struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := f.getEvents(ctx, cursor, &page); err != nil {
		return cursor, 0, err
	}
	for _, raw := range page.Events {
		var e struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &e); err != nil || e.Seq <= 0 {
			return cursor, 0, fmt.Errorf("event without a valid seq: %.200s", raw)
		}
		if e.Seq <= cursor {
			continue
		}
		if len(f.Types) == 0 || slices.Contains(f.Types, e.Type) {
			if err := f.deliver(ctx, e.Seq, raw); err != nil {
				return cursor, 0, &deliveryError{err}
			}
		}
		if err := writeFileAtomic(f.State, []byte(strconv.FormatInt(e.Seq, 10)+"\n")); err != nil {
			return cursor, 0, err
		}
		cursor = e.Seq
	}
	return cursor, len(page.Events), nil
}

// deliver 签名 POST 一条事件信封；签名是 HMAC-SHA256(secret, timestamp + "." + body)，十六进制，前缀 sha256=。
func (f *forwarder) deliver(ctx context.Context, seq int64, body []byte) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	m := hmac.New(sha256.New, []byte(f.Secret))
	m.Write([]byte(ts + "." + string(body)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.Target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-APA-Event-Id", strconv.FormatInt(seq, 10))
	req.Header.Set("X-APA-Timestamp", ts)
	req.Header.Set("X-APA-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("receiver answered HTTP %d", resp.StatusCode)
	}
	return nil
}

func (f *forwarder) getEvents(ctx context.Context, cursor int64, out any) error {
	path := fmt.Sprintf("/events?cursor=%d&wait=%d", cursor, f.Wait)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.URL, "/")+"/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.Key)
	req.Header.Set("Accept", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &statusError{path: path, code: resp.StatusCode, body: strings.TrimSpace(string(data[:min(len(data), 200)]))}
	}
	return json.Unmarshal(data, out)
}

func readCursor(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("cursor file %s is corrupt", path)
	}
	return n, nil
}

// writeFileAtomic 先写临时文件再改名，崩溃不会留下半个游标。
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
