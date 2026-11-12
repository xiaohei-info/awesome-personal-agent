// Package model 是外呼调用器的模型 HTTP 部分（设计 §7.7、§8.1）：
// 按 chat completions 兼容协议 + JSON Schema 结构化输出调用用户配置的端点（base_url + api_key）。
// 不起子进程、不占额度、不重试、不统计用量；失败如实返回，由调用模块决定降级语义。
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

var (
	// ErrNotConfigured 表示策略里没有配置模型端点（URL 或 model 为空）。
	ErrNotConfigured = errors.New("model: endpoint not configured")
	// ErrTimeout 表示调用超过时限。
	ErrTimeout = errors.New("model: timeout")
	// ErrBadResponse 表示端点返回了非预期的响应体（不是合法的 chat completions 响应）。
	ErrBadResponse = errors.New("model: bad response")
	// ErrSchema 表示模型输出不是合法 JSON，或不符合请求的 JSON Schema。
	ErrSchema = errors.New("model: output violates schema")
)

// StatusError 是端点返回的非 2xx 响应。
type StatusError struct {
	Code    int
	Message string // 端点错误体里的 message（截断），可能为空
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("model: endpoint returned status %d", e.Code)
	}
	return fmt.Sprintf("model: endpoint returned status %d: %s", e.Code, e.Message)
}

// DefaultTimeout 是 Request.Timeout 为零时的时限。
const DefaultTimeout = 30 * time.Second

const (
	maxBody    = 4 << 20
	maxMessage = 200
)

// Request 是一次调用。Schema 非空时要求结构化输出，并校验返回内容。
type Request struct {
	System  string
	User    string
	Schema  json.RawMessage // JSON Schema（对象）；空表示自由文本
	Timeout time.Duration
}

// Response 是模型的回答。Schema 非空时 Content 已确认是符合 schema 的 JSON。
type Response struct {
	Content string
}

// Client 向配置的端点发起调用。HTTP 为空时用独立的 http.Client（不走全局 DefaultClient）。
type Client struct {
	HTTP *http.Client
	// Sleep 是 Judge 退避与等待 Retry-After 用的等待函数；为空用真实计时。测试注入以免真等。
	Sleep func(ctx context.Context, d time.Duration) error
}

// Complete 发起一次调用。端点未配置时不发请求，返回 ErrNotConfigured。
func (c *Client) Complete(ctx context.Context, ep policy.GenerateEndpoint, req Request) (Response, error) {
	if ep.URL == "" || ep.Model == "" {
		return Response{}, ErrNotConfigured
	}
	var schema map[string]any
	if len(req.Schema) > 0 {
		if err := json.Unmarshal(req.Schema, &schema); err != nil || schema == nil {
			return Response{}, fmt.Errorf("model: invalid request schema: %v", err)
		}
		if err := checkSchema(schema, "$"); err != nil {
			return Response{}, fmt.Errorf("model: unsupported request schema: %w", err)
		}
	}
	body := map[string]any{
		"model": ep.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
	}
	if schema != nil {
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "output", "strict": true, "schema": schema},
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(ep.URL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("model: bad endpoint url: %w", scrub(err, ep.APIKey))
	}
	hreq.Header.Set("Content-Type", "application/json")
	if ep.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(hreq)
	if err != nil {
		return Response{}, transportErr(ctx, err, ep.APIKey)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Response{}, transportErr(ctx, err, ep.APIKey)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, &StatusError{Code: resp.StatusCode, Message: errorMessage(raw, ep.APIKey)}
	}
	if len(raw) > maxBody {
		return Response{}, fmt.Errorf("%w: body exceeds %d bytes", ErrBadResponse, maxBody)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 || out.Choices[0].Message.Content == nil {
		return Response{}, fmt.Errorf("%w: no choices[0].message.content", ErrBadResponse)
	}
	content := *out.Choices[0].Message.Content
	if schema != nil {
		var v any
		dec := json.NewDecoder(strings.NewReader(content))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil || dec.More() {
			return Response{}, fmt.Errorf("%w: content is not a single JSON value", ErrSchema)
		}
		if err := validate(schema, v, "$"); err != nil {
			return Response{}, fmt.Errorf("%w: %v", ErrSchema, err)
		}
	}
	return Response{Content: content}, nil
}

func transportErr(ctx context.Context, err error, key string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrTimeout
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("model: request failed: %w", scrub(err, key))
}

// scrub 去掉错误文本里可能出现的 api_key（url.Error 会带请求 URL）。
func scrub(err error, key string) error {
	if key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "***"))
}

func errorMessage(raw []byte, key string) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	m := e.Error.Message
	if key != "" {
		m = strings.ReplaceAll(m, key, "***")
	}
	if r := []rune(m); len(r) > maxMessage {
		m = string(r[:maxMessage])
	}
	return m
}
