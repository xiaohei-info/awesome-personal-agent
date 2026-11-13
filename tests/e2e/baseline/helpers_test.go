//go:build e2e

package baseline

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// 以下是 ACT-ENV-04/05 直接对替身发请求用的小工具（验收文档里的 curl）。

func (e *Env) post(t *testing.T, url string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func clsRequest(qs [][2]string) map[string]any {
	var questions []map[string]any
	for _, q := range qs {
		questions = append(questions, map[string]any{"id": q[0], "question": q[1], "criteria": "c", "input": map[string]string{"k": "v"}})
	}
	return map[string]any{"protocol": "probability-judgment/1", "questions": questions}
}

func (e *Env) clsRaw(t *testing.T, qs [][2]string) string {
	t.Helper()
	_, body := e.post(t, e.Cls.URL(), clsRequest(qs))
	return body
}

func (e *Env) clsStatus(t *testing.T, qs [][2]string) int {
	t.Helper()
	code, _ := e.post(t, e.Cls.URL(), clsRequest(qs))
	return code
}

// clsBatch 发一批题（id, 内容），返回 id → p_yes。
func (e *Env) clsBatch(t *testing.T, qs [][2]string) map[string]float64 {
	t.Helper()
	code, body := e.post(t, e.Cls.URL(), clsRequest(qs))
	var out struct {
		Results []struct {
			ID   string   `json:"id"`
			PYes *float64 `json:"p_yes"`
		} `json:"results"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil {
		t.Fatalf("CLS_STUB: %d %s", code, body)
	}
	got := map[string]float64{}
	for _, r := range out.Results {
		if r.PYes != nil {
			got[r.ID] = *r.PYes
		}
	}
	return got
}

func (e *Env) judgeChat(text string, logprobs bool) map[string]any {
	req := map[string]any{"model": "acc-model", "messages": []map[string]string{{"role": "user", "content": text}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "judge",
			"schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}, "sufficient": map[string]any{"type": "boolean"}}}}}}
	if logprobs {
		req["logprobs"] = true
	}
	return req
}

func (e *Env) genChat(text string) map[string]any {
	return map[string]any{"model": "acc-model", "messages": []map[string]string{{"role": "user", "content": text}}}
}

func (e *Env) chatRaw(t *testing.T, req map[string]any) string {
	t.Helper()
	_, body := e.post(t, e.ModelStub.URL()+"/chat/completions", req)
	return body
}

func (e *Env) chatContent(t *testing.T, req map[string]any) string {
	t.Helper()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(e.chatRaw(t, req)), &out); err != nil || len(out.Choices) == 0 {
		t.Fatalf("MODEL_STUB 响应不可解析: %v", err)
	}
	return out.Choices[0].Message.Content
}
