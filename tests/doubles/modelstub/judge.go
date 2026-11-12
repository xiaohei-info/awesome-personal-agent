package modelstub

import (
	"encoding/json"
	"math"
	"strings"
)

// ChatJudge 按题目内容（与 CLS_STUB 同一套 Key）配置判断题的作答。首个命中的规则生效，
// 与调用顺序无关，所以同一道题被两级各答一次时每次都得到同样的结果。
//
//	是非题 schema {"answer","sufficient"}：Answer 取 yes/no（缺省按 PYes 推出，再缺省 no）
//	选择题 schema {"choice","sufficient"}：Answer 为枚举值
//	打分题 schema {"score","sufficient"}：Score
//	chat-logprobs 请求（logprobs:true）：PYes 换成首 token 的对数概率
//
// Sufficient 缺省为 true；Raw 非空时原样作为输出（制造不合 schema 的内容）；
// Status 非 2xx 返回错误体；DelayMS 制造超时。
type ChatJudge struct {
	Key        Key      `json:"key"`
	Answer     string   `json:"answer,omitempty"`
	Score      *float64 `json:"score,omitempty"`
	Sufficient *bool    `json:"sufficient,omitempty"`
	PYes       *float64 `json:"p_yes,omitempty"`
	Raw        string   `json:"raw,omitempty"`
	Status     int      `json:"status,omitempty"`
	DelayMS    int      `json:"delay_ms,omitempty"`
}

// Judge 追加一条判断题配置。
func (s *Stub) Judge(j ChatJudge) {
	s.mu.Lock()
	s.judges = append(s.judges, j)
	s.mu.Unlock()
}

type chatBody struct {
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Logprobs       bool `json:"logprobs"`
	ResponseFormat struct {
		JSONSchema struct {
			Schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schema"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

func (b chatBody) props() map[string]json.RawMessage {
	if p := b.ResponseFormat.JSONSchema.Schema.Properties; len(p) > 0 {
		return p
	}
	return b.ResponseFormat.JSONSchema.Properties
}

// text 是全部消息正文，用来匹配 Key（问法、判据、输入、资料都在里面）。
func (b chatBody) text() string {
	var sb strings.Builder
	for _, m := range b.Messages {
		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			sb.WriteString(str)
		} else {
			sb.Write(m.Content) // 分段内容：保留原始 JSON，子串匹配照常可用
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// judgeReply 识别判断题（结构化输出 schema 含 sufficient，或 logprobs 请求）并按配置作答。
// 判断题没有命中配置时返回零值，交由原有脚本处理。
func (s *Stub) judgeReply(raw []byte) (Reply, map[string]any) {
	var b chatBody
	if json.Unmarshal(raw, &b) != nil {
		return Reply{}, nil
	}
	props := b.props()
	_, structured := props["sufficient"]
	if !structured && !b.Logprobs {
		return Reply{}, nil
	}
	text := b.text()
	s.mu.Lock()
	var hit *ChatJudge
	for i := range s.judges {
		if s.judges[i].Key.matches(text) {
			j := s.judges[i]
			hit = &j
			break
		}
	}
	s.mu.Unlock()
	if hit == nil {
		return Reply{}, nil
	}
	rep := Reply{DelayMS: hit.DelayMS, Status: hit.Status, judged: true}
	if hit.Raw != "" {
		rep.Content = hit.Raw
		return rep, nil
	}
	if b.Logprobs {
		p := 0.5
		if hit.PYes != nil {
			p = *hit.PYes
		}
		tok, lp := "yes", map[string]any{}
		if p < 0.5 {
			tok = "no"
		}
		rep.Content = tok
		lp["content"] = []any{map[string]any{"token": tok, "logprob": ln(math.Max(p, 1-p)), "top_logprobs": []any{
			map[string]any{"token": "yes", "logprob": ln(p)},
			map[string]any{"token": "no", "logprob": ln(1 - p)},
		}}}
		return rep, lp
	}
	suff := hit.Sufficient == nil || *hit.Sufficient
	out := map[string]any{"sufficient": suff}
	switch {
	case hasKey(props, "score"):
		if hit.Score != nil {
			out["score"] = *hit.Score
		} else {
			out["score"] = 0
		}
	case hasKey(props, "choice"):
		out["choice"] = hit.Answer
	default:
		ans := hit.Answer
		if ans == "" {
			ans = "no"
			if hit.PYes != nil && *hit.PYes >= 0.5 {
				ans = "yes"
			}
		}
		out["answer"] = ans
	}
	b2, _ := json.Marshal(out)
	rep.Content = string(b2)
	return rep, nil
}

func hasKey(m map[string]json.RawMessage, k string) bool { _, ok := m[k]; return ok }

// ln 把 0 概率映射为一个很小的有限对数值，保持 JSON 可编码。
func ln(p float64) float64 {
	if p <= 0 {
		return -1000
	}
	return math.Log(p)
}
