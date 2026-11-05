package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 本文件是 ext 与 APA 之间的 HTTP 协议（设计 §10.3）。类型在这里自备：ext 是独立进程，只经 HTTP 接入，
// 不 import 内核包。

// Delivery 是领取到的投递（只含渠道 ext 用得到的字段）。
type Delivery struct {
	ID      string  `json:"id"`
	Class   string  `json:"class"`
	Version int64   `json:"version"`
	Content Content `json:"content"`
}

// Content 是投递内容。Options 的每项形如 "<选项 id>: <标签>"。
type Content struct {
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	Options   []string `json:"options"`
	Recommend string   `json:"recommend"`
	WebURL    string   `json:"web_url"`
	FormURL   string   `json:"form_url"`
	WebOnly   bool     `json:"web_only"` // 内核置位：只给 Web 链接，不渲染选项，不转达回答
}

// APIError 是内核的统一错误体加 HTTP 状态。
type APIError struct {
	Status int     `json:"-"`
	Code   string  `json:"code"`
	Why    string  `json:"why"`
	Next   string  `json:"next"`
	WebURL *string `json:"web_url"`
}

func (e *APIError) Error() string { return fmt.Sprintf("apa %d %s: %s", e.Status, e.Code, e.Why) }

// IsConflict：版本已作废（ack/fail）或回答已有结果（answer）。
func (e *APIError) IsConflict() bool { return e.Status == http.StatusConflict }

type apa struct {
	base string
	key  string
	hc   *http.Client
}

func newAPA(base, key string) *apa {
	return &apa{base: strings.TrimRight(base, "/"), key: key, hc: &http.Client{Timeout: 90 * time.Second}}
}

func (a *apa) do(ctx context.Context, method, path string, in any) (int, []byte, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+"/v1"+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode >= 300 {
		e := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, e) != nil || e.Code == "" {
			e.Code, e.Why = "http_"+strconv.Itoa(resp.StatusCode), strings.TrimSpace(string(data))
		}
		return resp.StatusCode, data, e
	}
	return resp.StatusCode, data, nil
}

// next 长轮询领取；没有投递返回 nil。
func (a *apa) next(ctx context.Context, waitSeconds int) (*Delivery, error) {
	st, data, err := a.do(ctx, http.MethodGet, "/deliveries/next?wait="+strconv.Itoa(waitSeconds), nil)
	if err != nil {
		return nil, err
	}
	if st == http.StatusNoContent {
		return nil, nil
	}
	var d Delivery
	if err := json.Unmarshal(data, &d); err != nil || d.ID == "" {
		return nil, fmt.Errorf("apa returned an unreadable delivery: %v", err)
	}
	return &d, nil
}

func (a *apa) ack(ctx context.Context, id string, version int64, ref string) error {
	_, _, err := a.do(ctx, http.MethodPost, "/deliveries/"+url.PathEscape(id)+"/ack", map[string]any{"version": version, "external_ref": ref})
	return err
}

func (a *apa) fail(ctx context.Context, id string, version int64, reason string) error {
	_, _, err := a.do(ctx, http.MethodPost, "/deliveries/"+url.PathEscape(id)+"/fail", map[string]any{"version": version, "reason": reason})
	return err
}

func (a *apa) answer(ctx context.Context, id, ref, sender, choice, text string) error {
	_, _, err := a.do(ctx, http.MethodPost, "/deliveries/"+url.PathEscape(id)+"/answer",
		map[string]any{"ref": ref, "sender": sender, "choice": choice, "text": text})
	return err
}

func asAPIError(err error) *APIError {
	var e *APIError
	if errors.As(err, &e) {
		return e
	}
	return nil
}
