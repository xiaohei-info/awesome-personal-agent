package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// seen 记下替身端点收到的一次请求。
type seen struct {
	path, query string
	header      http.Header
	body        map[string]any
}

// wireServer 起一个替身端点：记录请求，回固定的响应体。
func wireServer(t *testing.T, reply string) (string, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.query, got.header = r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		_ = json.Unmarshal(b, &got.body)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", got
}

func complete(url, protocol string) (Response, error) {
	ep := policy.GenerateEndpoint{URL: url, Model: "m1", APIKey: "sekret-key", Protocol: protocol}
	return (&Client{}).Complete(context.Background(), ep, Request{System: "sys", User: "usr", Schema: json.RawMessage(judgeSchema)})
}

func dig(m map[string]any, keys ...string) any {
	var v any = m
	for _, k := range keys {
		mm, _ := v.(map[string]any)
		v = mm[k]
	}
	return v
}

// 消息协议：POST <url>/messages，x-api-key 与版本头，不带 Bearer；输出工具的 input_schema 即模板 schema，tool_choice 强制调用它，不带 thinking。
func TestMessagesRequestShape(t *testing.T) {
	url, got := wireServer(t, `{"content":[{"type":"tool_use","name":"output","input":{"choice":"wait","confidence":0.5}}],"stop_reason":"tool_use"}`)
	resp, err := complete(url, policy.ProtocolMessages)
	if err != nil || resp.Content != `{"choice":"wait","confidence":0.5}` {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if got.path != "/v1/messages" || got.header.Get("x-api-key") != "sekret-key" || got.header.Get("anthropic-version") == "" || got.header.Get("Authorization") != "" {
		t.Fatalf("path %s header %v", got.path, got.header)
	}
	tools, _ := got.body["tools"].([]any)
	if len(tools) != 1 || dig(tools[0].(map[string]any), "input_schema", "type") != "object" || dig(tools[0].(map[string]any), "name") != "output" {
		t.Fatalf("tools %v", got.body["tools"])
	}
	if dig(got.body, "tool_choice", "type") != "tool" || dig(got.body, "tool_choice", "name") != "output" || got.body["system"] != "sys" || got.body["max_tokens"] == nil {
		t.Fatalf("body %v", got.body)
	}
	if _, ok := got.body["thinking"]; ok {
		t.Fatal("thinking must not be sent with a forced tool_choice")
	}
}

// 响应协议：POST <url>/responses，Bearer；text.format 为严格 json_schema，store 关闭。
func TestResponsesRequestShape(t *testing.T) {
	url, got := wireServer(t, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"choice\":\"remind\",\"confidence\":1}"}]}]}`)
	resp, err := complete(url, policy.ProtocolResponses)
	if err != nil || resp.Content != `{"choice":"remind","confidence":1}` {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if got.path != "/v1/responses" || got.header.Get("Authorization") != "Bearer sekret-key" || got.header.Get("x-api-key") != "" {
		t.Fatalf("path %s header %v", got.path, got.header)
	}
	f := dig(got.body, "text", "format")
	fm, _ := f.(map[string]any)
	if fm["type"] != "json_schema" || fm["strict"] != true || dig(fm, "schema", "type") != "object" || got.body["store"] != false || got.body["instructions"] != "sys" || got.body["input"] != "usr" {
		t.Fatalf("body %v", got.body)
	}
}

// 没写协议的旧配置按 chat-completions 走，请求与改动前一致。
func TestMissingProtocolIsChatCompletions(t *testing.T) {
	url, got := wireServer(t, `{"choices":[{"message":{"content":"{\"choice\":\"wait\",\"confidence\":0}"}}]}`)
	if _, err := complete(url, ""); err != nil {
		t.Fatal(err)
	}
	if got.path != "/v1/chat/completions" || got.header.Get("Authorization") != "Bearer sekret-key" || dig(got.body, "response_format", "type") != "json_schema" {
		t.Fatalf("path %s body %v", got.path, got.body)
	}
	if StructuredOutput("") != "response_format" || StructuredOutput(policy.ProtocolMessages) != "tool_use" || StructuredOutput(policy.ProtocolResponses) != "text.format" {
		t.Fatal("structured output names")
	}
}

// 各协议的解析：思考与推理项被忽略；结构化通道里没结果、截断、非 JSON、形状不对都如实失败，不剥代码块兜底。
func TestProtocolParsing(t *testing.T) {
	for _, c := range []struct {
		name, protocol, reply string
		ok                    bool
		shape                 string
		bad                   bool
	}{
		{"messages thinking then tool", policy.ProtocolMessages, `{"content":[{"type":"thinking","thinking":"","signature":"x"},{"type":"redacted_thinking","data":"y"},{"type":"text","text":"好的"},{"type":"tool_use","name":"output","input":{"choice":"remind","confidence":0.2}}],"stop_reason":"tool_use"}`, true, "", false},
		{"messages text only", policy.ProtocolMessages, `{"content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"{\"choice\":\"remind\",\"confidence\":1}"}],"stop_reason":"end_turn"}`, false, ShapeNoToolCall, false},
		{"messages other tool", policy.ProtocolMessages, `{"content":[{"type":"tool_use","name":"search","input":{}}],"stop_reason":"tool_use"}`, false, ShapeNoToolCall, false},
		{"messages truncated", policy.ProtocolMessages, `{"content":[{"type":"tool_use","name":"output","input":{"choice":"remind"}}],"stop_reason":"max_tokens"}`, false, ShapeTruncated, false},
		{"messages input violates schema", policy.ProtocolMessages, `{"content":[{"type":"tool_use","name":"output","input":{"choice":"nope","confidence":1}}]}`, false, "", false},
		{"messages input is a string", policy.ProtocolMessages, `{"content":[{"type":"tool_use","name":"output","input":"{\"choice\":\"remind\",\"confidence\":1}"}]}`, false, "", false},
		{"messages not a messages reply", policy.ProtocolMessages, `{"choices":[]}`, false, "", true},
		{"responses reasoning ignored", policy.ProtocolResponses, `{"output":[{"type":"reasoning","summary":[]},{"type":"message","content":[{"type":"output_text","text":"{\"choice\":\"wait\",\"confidence\":0.1}"}]}]}`, true, "", false},
		{"responses output_text only", policy.ProtocolResponses, `{"output_text":"{\"choice\":\"wait\",\"confidence\":0.1}"}`, true, "", false},
		{"responses fenced", policy.ProtocolResponses, "{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"```json\\n{}\\n```\"}]}]}", false, ShapeFence, false},
		{"responses only reasoning", policy.ProtocolResponses, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning"}]}`, false, ShapeEmpty, false},
		{"responses truncated", policy.ProtocolResponses, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"{\"choice\":\"wa"}]}]}`, false, ShapeTruncated, false},
		{"responses not a responses reply", policy.ProtocolResponses, `{"choices":[{"message":{"content":"{}"}}]}`, false, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, _ := wireServer(t, c.reply)
			_, err := complete(url, c.protocol)
			switch {
			case c.ok:
				if err != nil {
					t.Fatal(err)
				}
			case c.bad:
				if !errors.Is(err, ErrBadResponse) {
					t.Fatalf("want ErrBadResponse, got %v", err)
				}
			default:
				if !errors.Is(err, ErrSchema) || SchemaShape(err) != c.shape {
					t.Fatalf("err=%v shape=%q, want %q", err, SchemaShape(err), c.shape)
				}
			}
		})
	}
}

// 不带 schema 时消息协议拼接 text 块（思考块不进正文），且不定义输出工具。
func TestMessagesFreeText(t *testing.T) {
	url, got := wireServer(t, `{"content":[{"type":"thinking","thinking":"想"},{"type":"text","text":"你"},{"type":"text","text":"好"}]}`)
	ep := policy.GenerateEndpoint{URL: url, Model: "m1", Protocol: policy.ProtocolMessages}
	resp, err := (&Client{}).Complete(context.Background(), ep, Request{User: "hi"})
	if err != nil || resp.Content != "你好" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if _, ok := got.body["tools"]; ok || got.header.Get("x-api-key") != "" {
		t.Fatalf("body %v header %v", got.body, got.header)
	}
}

// 取模型列表按协议放鉴权头；消息协议带 limit 取整页。
func TestListModelsByProtocol(t *testing.T) {
	url, got := wireServer(t, `{"data":[{"id":"b","type":"model"},{"id":"a","type":"model"}],"has_more":false}`)
	ids, _, err := (&Client{}).ListModels(context.Background(), url, "sekret-key", policy.ProtocolMessages)
	if err != nil || len(ids) != 2 || ids[0] != "a" {
		t.Fatalf("ids %v err %v", ids, err)
	}
	if got.path != "/v1/models" || got.query != "limit=1000" || got.header.Get("x-api-key") != "sekret-key" || got.header.Get("anthropic-version") == "" || got.header.Get("Authorization") != "" {
		t.Fatalf("path %s?%s header %v", got.path, got.query, got.header)
	}
	if _, _, err := (&Client{}).ListModels(context.Background(), url, "sekret-key", policy.ProtocolResponses); err != nil || got.header.Get("Authorization") != "Bearer sekret-key" || got.query != "" {
		t.Fatalf("responses: err %v header %v", err, got.header)
	}
}
