package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if StructuredOutput("") != "response_format" || StructuredOutput(policy.ProtocolMessages) != "tool_use" || StructuredOutput(policy.ProtocolResponses) != "text.format" || StructuredOutput(policy.ProtocolGenerateContent) != "responseJsonSchema" {
		t.Fatal("structured output names")
	}
}

// 内容生成协议：POST <base>/models/<model>:generateContent，x-goog-api-key，不带 Bearer；
// systemInstruction + contents + generationConfig(responseMimeType, responseJsonSchema)；支持 models/ 前缀剥离。
func TestGenerateContentRequestShape(t *testing.T) {
	url, got := wireServer(t, `{"candidates":[{"content":{"parts":[{"text":"thinking...","thought":true},{"text":"{\"choice\":\"remind\",\"confidence\":0.8}"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":120,"candidatesTokenCount":15,"thoughtsTokenCount":40,"totalTokenCount":175}}`)
	base := strings.TrimSuffix(url, "/v1") // 裸根地址应默认补 /v1beta
	ep := policy.GenerateEndpoint{URL: base, Model: "models/gemini-4-hydrogen", APIKey: "sekret-key", Protocol: policy.ProtocolGenerateContent}
	resp, err := (&Client{}).Complete(context.Background(), ep, Request{System: "sys", User: "usr", Schema: json.RawMessage(judgeSchema)})
	if err != nil || resp.Content != `{"choice":"remind","confidence":0.8}` {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if resp.Usage != (Usage{InputTokens: 120, OutputTokens: 15, ThoughtsTokens: 40, TotalTokens: 175}) {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if got.path != "/v1beta/models/gemini-4-hydrogen:generateContent" || got.header.Get("x-goog-api-key") != "sekret-key" || got.header.Get("Authorization") != "" {
		t.Fatalf("path %s header %v", got.path, got.header)
	}
	sysParts, _ := dig(got.body, "systemInstruction", "parts").([]any)
	contents, _ := got.body["contents"].([]any)
	if len(sysParts) != 1 || dig(sysParts[0].(map[string]any), "text") != "sys" || len(contents) != 1 {
		t.Fatalf("body %v", got.body)
	}
	if dig(got.body, "generationConfig", "responseMimeType") != "application/json" ||
		dig(got.body, "generationConfig", "responseJsonSchema", "type") != "object" {
		t.Fatalf("generationConfig %v", got.body["generationConfig"])
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
		{"generate-content thinking ignored", policy.ProtocolGenerateContent, `{"candidates":[{"content":{"parts":[{"text":"先想一下","thought":true},{"text":"{\"choice\":\"wait\",\"confidence\":0.3}"}]},"finishReason":"STOP"}]}`, true, "", false},
		{"generate-content fenced", policy.ProtocolGenerateContent, "{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"```json\\n{}\\n```\"}]}}]}", false, ShapeFence, false},
		{"generate-content only thinking", policy.ProtocolGenerateContent, `{"candidates":[{"content":{"parts":[{"text":"想太久","thought":true}]},"finishReason":"MAX_TOKENS"}]}`, false, ShapeEmpty, false},
		{"generate-content truncated", policy.ProtocolGenerateContent, `{"candidates":[{"content":{"parts":[{"text":"{\"choice\":\"wa"}]},"finishReason":"MAX_TOKENS"}]}`, false, ShapeTruncated, false},
		{"generate-content violates schema", policy.ProtocolGenerateContent, `{"candidates":[{"content":{"parts":[{"text":"{\"choice\":\"nope\",\"confidence\":1}"}]},"finishReason":"STOP"}]}`, false, "", false},
		{"generate-content prompt blocked", policy.ProtocolGenerateContent, `{"promptFeedback":{"blockReason":"SAFETY"}}`, false, "", true},
		{"generate-content empty candidates", policy.ProtocolGenerateContent, `{"candidates":[]}`, false, "", true},
		{"generate-content candidate blocked", policy.ProtocolGenerateContent, `{"candidates":[{"finishReason":"SAFETY"}]}`, false, "", true},
		{"generate-content not a gemini reply", policy.ProtocolGenerateContent, `{"choices":[{"message":{"content":"{}"}}]}`, false, "", true},
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
	gurl, ggot := wireServer(t, `{"models":[{"name":"models/gemini-4-hydrogen"},{"name":"gemini-4-neon"},{"name":"models/gemini-4-hydrogen"}]}`)
	gbase := strings.TrimSuffix(gurl, "/v1")
	gids, _, err := (&Client{}).ListModels(context.Background(), gbase, "sekret-key", policy.ProtocolGenerateContent)
	if err != nil || strings.Join(gids, ",") != "gemini-4-hydrogen,gemini-4-neon" {
		t.Fatalf("generate-content ids=%v err=%v", gids, err)
	}
	if ggot.path != "/v1beta/models" || ggot.query != "pageSize=1000" || ggot.header.Get("x-goog-api-key") != "sekret-key" || ggot.header.Get("Authorization") != "" {
		t.Fatalf("generate-content path=%s?%s header=%v", ggot.path, ggot.query, ggot.header)
	}
}

// 不带 schema 时内容生成协议过滤 thought 块、拼接文本块，不发 generationConfig 与 systemInstruction。
func TestGenerateContentFreeText(t *testing.T) {
	url, got := wireServer(t, `{"candidates":[{"content":{"parts":[{"text":"思路","thought":true},{"text":"你"},{"text":"好"}]},"finishReason":"STOP"}]}`)
	ep := policy.GenerateEndpoint{URL: url, Model: "gemini-4-hydrogen", Protocol: policy.ProtocolGenerateContent}
	resp, err := (&Client{}).Complete(context.Background(), ep, Request{User: "hi"})
	if err != nil || resp.Content != "你好" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if _, ok := got.body["generationConfig"]; ok {
		t.Fatalf("unexpected generationConfig: %v", got.body)
	}
	if _, ok := got.body["systemInstruction"]; ok {
		t.Fatalf("unexpected systemInstruction: %v", got.body)
	}
	if got.header.Get("x-goog-api-key") != "" {
		t.Fatalf("unexpected key header: %v", got.header)
	}
}

// 4 种协议的 usage 解析都落到统一的 Usage 结构。
func TestUsageParsingAllProtocols(t *testing.T) {
	for _, c := range []struct {
		name, protocol, reply string
		want                  Usage
	}{
		{"chat", policy.ProtocolChatCompletions, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`, Usage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14}},
		{"responses", policy.ProtocolResponses, `{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}`, Usage{InputTokens: 11, OutputTokens: 5, TotalTokens: 16}},
		{"messages", policy.ProtocolMessages, `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":6}}`, Usage{InputTokens: 12, OutputTokens: 6, TotalTokens: 18}},
		{"generate-content", policy.ProtocolGenerateContent, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":13,"candidatesTokenCount":7,"thoughtsTokenCount":5,"totalTokenCount":25}}`, Usage{InputTokens: 13, OutputTokens: 7, ThoughtsTokens: 5, TotalTokens: 25}},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, _ := wireServer(t, c.reply)
			ep := policy.GenerateEndpoint{URL: url, Model: "m1", Protocol: c.protocol}
			resp, err := (&Client{}).Complete(context.Background(), ep, Request{User: "hi"})
			if err != nil || resp.Usage != c.want {
				t.Fatalf("usage=%+v want=%+v err=%v", resp.Usage, c.want, err)
			}
		})
	}
}

// URL 规范化：裸根地址按协议补 /v1 或 /v1beta；已带 /v1、/v1beta、自定义前缀或误粘贴了完整子路径时不重复拼接。
func TestURLNormalizationAllProtocols(t *testing.T) {
	cases := []struct {
		raw, proto, model, wantCall, wantBase string
	}{
		{"https://gw.example.com", "", "m1", "https://gw.example.com/v1/chat/completions", "https://gw.example.com/v1"},
		{"https://gw.example.com/", policy.ProtocolChatCompletions, "m1", "https://gw.example.com/v1/chat/completions", "https://gw.example.com/v1"},
		{"https://gw.example.com/v1", policy.ProtocolChatCompletions, "m1", "https://gw.example.com/v1/chat/completions", "https://gw.example.com/v1"},
		{"https://gw.example.com/v1/", policy.ProtocolChatCompletions, "m1", "https://gw.example.com/v1/chat/completions", "https://gw.example.com/v1"},
		{"https://gw.example.com/v1/chat/completions", policy.ProtocolChatCompletions, "m1", "https://gw.example.com/v1/chat/completions", "https://gw.example.com/v1"},
		{"https://gw.example.com/api/v1", policy.ProtocolChatCompletions, "m1", "https://gw.example.com/api/v1/chat/completions", "https://gw.example.com/api/v1"},
		{"https://gw.example.com", policy.ProtocolResponses, "m1", "https://gw.example.com/v1/responses", "https://gw.example.com/v1"},
		{"https://gw.example.com/v1/responses", policy.ProtocolResponses, "m1", "https://gw.example.com/v1/responses", "https://gw.example.com/v1"},
		{"https://gw.example.com", policy.ProtocolMessages, "m1", "https://gw.example.com/v1/messages", "https://gw.example.com/v1"},
		{"https://gw.example.com/v1/messages", policy.ProtocolMessages, "m1", "https://gw.example.com/v1/messages", "https://gw.example.com/v1"},
		{"https://gw.example.com", policy.ProtocolGenerateContent, "gemini-4-hydrogen", "https://gw.example.com/v1beta/models/gemini-4-hydrogen:generateContent", "https://gw.example.com/v1beta"},
		{"https://gw.example.com/v1beta", policy.ProtocolGenerateContent, "gemini-4-hydrogen", "https://gw.example.com/v1beta/models/gemini-4-hydrogen:generateContent", "https://gw.example.com/v1beta"},
		{"https://gw.example.com/v1beta/", policy.ProtocolGenerateContent, "models/gemini-4-hydrogen", "https://gw.example.com/v1beta/models/gemini-4-hydrogen:generateContent", "https://gw.example.com/v1beta"},
		{"https://gw.example.com/v1", policy.ProtocolGenerateContent, "gemini-4-hydrogen", "https://gw.example.com/v1/models/gemini-4-hydrogen:generateContent", "https://gw.example.com/v1"},
		{"https://gw.example.com/v1beta/models/old:generateContent", policy.ProtocolGenerateContent, "gemini-4-hydrogen", "https://gw.example.com/v1beta/models/gemini-4-hydrogen:generateContent", "https://gw.example.com/v1beta"},
		{"https://gw.example.com/v1beta/models/old:streamGenerateContent", policy.ProtocolGenerateContent, "gemini-4-hydrogen", "https://gw.example.com/v1beta/models/gemini-4-hydrogen:generateContent", "https://gw.example.com/v1beta"},
	}
	for _, c := range cases {
		if got := endpointBase(c.raw, c.proto); got != c.wantBase {
			t.Errorf("endpointBase(%q, %q) = %q, want %q", c.raw, c.proto, got, c.wantBase)
		}
		if got := endpointURL(c.raw, c.proto, c.model); got != c.wantCall {
			t.Errorf("endpointURL(%q, %q, %q) = %q, want %q", c.raw, c.proto, c.model, got, c.wantCall)
		}
	}
}

// 数组包裹的错误体也能提取消息并脱敏 api_key。
func TestErrorMessageArrayWrapped(t *testing.T) {
	msg := errorMessage([]byte(`[{"error":{"code":400,"message":"bad key sekret-key"}}]`), "sekret-key")
	if msg != "bad key ***" {
		t.Fatalf("got %q", msg)
	}
}

// 思考强度只在 responses 协议里发；输出上限不发（上游丢弃，附录 D.7）。
func TestResponsesSendsEffortOnly(t *testing.T) {
	b := responsesBody("m", Request{User: "u", Effort: "low"}, nil)
	if r, _ := b["reasoning"].(map[string]any); r["effort"] != "low" || b["max_output_tokens"] != nil {
		t.Fatalf("body = %v", b)
	}
	if b := responsesBody("m", Request{User: "u"}, nil); b["reasoning"] != nil {
		t.Fatalf("no effort must send no reasoning: %v", b)
	}
	var th policy.Thinking
	if th.ModelEffort(true) != "low" || th.ModelEffort(false) != "medium" {
		t.Fatalf("defaults = %q %q", th.ModelEffort(true), th.ModelEffort(false))
	}
}
