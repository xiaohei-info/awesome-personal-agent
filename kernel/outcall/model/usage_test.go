package model

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 用量解析：输入含缓存读写；输出不合 schema 的答复照样带回用量；判断端点按请求累计，没带 usage 的不算回报。
func TestUsageReported(t *testing.T) {
	r, err := messagesParse([]byte(`{"content":[{"type":"text","text":"x"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}`), false)
	if err != nil || r.usage.InputTokens != 45 || r.usage.CacheReadTokens != 30 || r.usage.CacheWriteTokens != 5 {
		t.Fatalf("messages: %+v %v", r.usage, err)
	}
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) }))
	defer srv.Close()
	c := &Client{}
	body = `{"choices":[{"message":{"content":"not json"}}],"usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3}}}`
	resp, err := c.Complete(context.Background(), policy.GenerateEndpoint{URL: srv.URL, Model: "m"}, Request{Schema: []byte(`{"type":"object"}`)})
	if SchemaShape(err) == "" || resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 4 || resp.Usage.CacheReadTokens != 3 {
		t.Fatalf("complete: %+v %v", resp.Usage, err)
	}
	ep := policy.JudgeEndpoint{URL: srv.URL, Model: "m", Protocol: policy.ProtocolSystemOne, MaxBatch: 1}
	for _, tc := range []struct {
		body     string
		reported int
	}{
		{`{"model":"m","answers":{},"usage":{"input_tokens":5,"output_tokens":1,"prompt_tokens":5,"completion_tokens":1}}`, 2},
		{`{"model":"m","answers":{}}`, 0},
	} {
		body = tc.body
		res, err := c.Judge(context.Background(), ep, []Question{{Question: "a"}, {Question: "b"}}, JudgeOptions{})
		if err != nil || res.Requests != 2 || res.Reported != tc.reported || res.Usage.InputTokens != 5*tc.reported || res.Usage.OutputTokens != tc.reported {
			t.Fatalf("judge: %+v %v", res, err)
		}
	}
}
