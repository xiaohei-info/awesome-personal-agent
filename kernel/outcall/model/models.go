package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// ModelsTimeout 是取模型列表的时限。
	ModelsTimeout = 10 * time.Second
	// MaxModels 是返回的模型 id 上限；超出时截断并报告。
	MaxModels   = 500
	maxListBody = 1 << 20
)

// ListModels 取端点的模型列表：GET <url>/models，读 data[].id 或 models[].name（设计 §4.1 "设置 › 模型"）。
// 请求只带鉴权头，不带任何账本内容。返回去重排序后的 id，至多 MaxModels 个，超出时 truncated 为真。
// 非 2xx 返回 *StatusError；超时返回 ErrTimeout；响应不是模型列表返回 ErrBadResponse。
func (c *Client) ListModels(ctx context.Context, url, key string) (ids []string, truncated bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, ModelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/models", nil)
	if err != nil {
		return nil, false, fmt.Errorf("model: bad endpoint url: %w", scrub(err, key))
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false, transportErr(ctx, err, key)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxListBody+1))
	if err != nil {
		return nil, false, transportErr(ctx, err, key)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, false, &StatusError{Code: resp.StatusCode, Message: errorMessage(raw, key)}
	}
	if len(raw) > maxListBody {
		return nil, false, fmt.Errorf("%w: body exceeds %d bytes", ErrBadResponse, maxListBody)
	}
	// 对话补全兼容端点给 data[].id；system-one 判断接口给 models[].name。
	var out struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
		Models *[]struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || (out.Data == nil && out.Models == nil) {
		return nil, false, fmt.Errorf("%w: no data[] or models[] in model list", ErrBadResponse)
	}
	var names []string
	if out.Data != nil {
		for _, m := range *out.Data {
			names = append(names, m.ID)
		}
	} else {
		for _, m := range *out.Models {
			names = append(names, m.Name)
		}
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n != "" && !seen[n] {
			seen[n] = true
			ids = append(ids, n)
		}
	}
	sort.Strings(ids)
	if len(ids) > MaxModels {
		return ids[:MaxModels], true, nil
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, false, nil
}
