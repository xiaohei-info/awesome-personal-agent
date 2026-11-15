package model

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 生成协议的线上差异（设计 §8.1）：每种协议各用原生的结构化输出方式，各自解析，再走同一套 schema 校验。
// 新增协议在这里加一项，并同步 policy 的协议枚举与附录 B.2。

const (
	// outputTool 是消息协议里承载结构化输出的工具名：tool_choice 强制调用它，结果取它的 input。
	outputTool = "output"
	// messagesMaxTokens 是消息协议必填的 max_tokens；强制工具调用时模型不先思考（附录 D.7），起草的输出远小于它。
	messagesMaxTokens = 4096
	// messagesVersion 是消息协议的版本头取值。
	messagesVersion = "2023-06-01"
)

// reply 是一种协议解析出的回答。missing 非空：结构化通道里没有结果，值是内容形态（Shape*）。
type reply struct {
	content   string
	truncated bool
	missing   string
}

type wire struct {
	path       string
	structured string // 结构化输出的方式，写进自检结论（data.structured_output）
	body       func(model string, req Request, schema map[string]any) map[string]any
	parse      func(raw []byte, structured bool) (reply, error)
}

var wires = map[string]wire{
	policy.ProtocolChatCompletions: {path: "/chat/completions", structured: "response_format", body: chatBody, parse: chatParse},
	policy.ProtocolResponses:       {path: "/responses", structured: "text.format", body: responsesBody, parse: responsesParse},
	policy.ProtocolMessages:        {path: "/messages", structured: "tool_use", body: messagesBody, parse: messagesParse},
}

// StructuredOutput 返回协议 p（空 = chat-completions）用哪种原生方式约束输出：response_format、text.format 或 tool_use。
func StructuredOutput(p string) string { return wires[policy.GenerateProtocol(p)].structured }

// setAuth 按协议放鉴权头：消息协议用 x-api-key 与版本头，其余用 Bearer。key 为空时不带鉴权头。
func setAuth(h http.Header, protocol, key string) {
	if protocol == policy.ProtocolMessages {
		h.Set("anthropic-version", messagesVersion)
		if key != "" {
			h.Set("x-api-key", key)
		}
		return
	}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
}

func chatBody(model string, req Request, schema map[string]any) map[string]any {
	body := map[string]any{
		"model": model,
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
	return body
}

func chatParse(raw []byte, _ bool) (reply, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 || out.Choices[0].Message.Content == nil {
		return reply{}, fmt.Errorf("%w: no choices[0].message.content", ErrBadResponse)
	}
	return reply{content: *out.Choices[0].Message.Content, truncated: out.Choices[0].FinishReason == "length"}, nil
}

// responsesBody：系统段放 instructions，用户段放 input；store 关掉，端点不留存请求内容（§14.14）。
func responsesBody(model string, req Request, schema map[string]any) map[string]any {
	body := map[string]any{"model": model, "input": req.User, "store": false}
	if req.System != "" {
		body["instructions"] = req.System
	}
	if schema != nil {
		body["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "output", "strict": true, "schema": schema}}
	}
	return body
}

// responsesParse 取 output[] 里 message 的 output_text 段；推理项（reasoning）等其他项忽略。
// output[] 缺失时用顶层 output_text（部分网关只给这个便捷字段）。
func responsesParse(raw []byte, _ bool) (reply, error) {
	var out struct {
		Output *[]struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText *string `json:"output_text"`
		Status     string  `json:"status"`
		Incomplete *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || (out.Output == nil && out.OutputText == nil) {
		return reply{}, fmt.Errorf("%w: no output[] or output_text", ErrBadResponse)
	}
	r := reply{truncated: out.Status == "incomplete" && out.Incomplete != nil && out.Incomplete.Reason == "max_output_tokens"}
	if out.Output == nil {
		r.content = *out.OutputText
		return r, nil
	}
	var b strings.Builder
	for _, item := range *out.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				b.WriteString(c.Text)
			}
		}
	}
	r.content = b.String()
	return r, nil
}

// messagesBody：有 schema 时定义一个输出工具，input_schema 即模板 schema，tool_choice 强制调用它。
// 不带 thinking 字段：强制工具调用与显式开启的思考冲突（附录 D.7）。
func messagesBody(model string, req Request, schema map[string]any) map[string]any {
	body := map[string]any{
		"model":      model,
		"max_tokens": messagesMaxTokens,
		"messages":   []map[string]string{{"role": "user", "content": req.User}},
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if schema != nil {
		body["tools"] = []map[string]any{{"name": outputTool, "description": "Return the answer. Call this tool exactly once with the complete result.", "input_schema": schema}}
		body["tool_choice"] = map[string]any{"type": "tool", "name": outputTool}
	}
	return body
}

// messagesParse 按块的 type 取结果：结构化时取输出工具的 input，否则拼接 text 块；thinking、redacted_thinking 块忽略。
func messagesParse(raw []byte, structured bool) (reply, error) {
	var out struct {
		Content *[]struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			Text  string          `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Content == nil {
		return reply{}, fmt.Errorf("%w: no content[]", ErrBadResponse)
	}
	r := reply{truncated: out.StopReason == "max_tokens"}
	var b strings.Builder
	for _, blk := range *out.Content {
		switch {
		case structured && blk.Type == "tool_use" && blk.Name == outputTool:
			if r.truncated { // 截断时工具参数可能不全
				r.missing = ShapeTruncated
				return r, nil
			}
			r.content = string(blk.Input)
			return r, nil
		case blk.Type == "text":
			b.WriteString(blk.Text)
		}
	}
	if structured {
		r.missing = ShapeNoToolCall
		if r.truncated {
			r.missing = ShapeTruncated
		}
		return r, nil
	}
	r.content = b.String()
	return r, nil
}
