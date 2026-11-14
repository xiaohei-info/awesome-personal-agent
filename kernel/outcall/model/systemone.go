package model

// system-one 适配器（§8.8）：原生分类模型服务的判断接口。POST <url>/systemone，
// 请求 {model, state, questions:{<题号>: {type: noul, instructions, criteria}}}，响应 {model, answers:{<题号>: {type: noul, noul}}}。
// 一批是同模板、不同资料的题，state 由批内各题共享，所以每题的 input 与已裁剪的 material 放进它自己的 instructions 对象，
// state 只放固定说明：每题的上下文只含自己的资料，不被别题撑大，也不串题。

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

const (
	systemOnePath  = "/systemone"
	systemOneState = "Each question carries its own `input` and `material` in its instructions; judge each question only against them."
	systemOneFalse = "The criteria for yes are not met."
	// systemOneBatchChars 是一批题的字符预算：接口按每请求的上下文长度限制（而不是题数）拒绝请求，
	// 预算按每字符至多约 1.5 个 token 估，留在 64k token 的默认上限以内。
	systemOneBatchChars = 40000
	// tokensExceeded 是接口在请求超出上下文长度时 400 响应体里的错误类型。
	tokensExceeded = "max_tokens_exceeded"
)

type systemOneWire struct{}

// SystemOneBase 去掉本人可能多填的 /systemone，得到接口根；列模型与判断都在它下面。
func SystemOneBase(url string) string {
	return strings.TrimSuffix(strings.TrimRight(url, "/"), systemOnePath)
}

func (systemOneWire) url(ep policy.JudgeEndpoint) string {
	return SystemOneBase(ep.URL) + systemOnePath
}

func (systemOneWire) chunks(qs []Question, size int) [][2]int {
	return pack(qs, size, systemOneBatchChars, func(q Question) int {
		return utf8.RuneCountInString(q.Question) + utf8.RuneCountInString(q.Criteria) + utf8.RuneCount(q.Input) + utf8.RuneCountInString(q.Material)
	})
}

func (systemOneWire) classify(f *callFault) *callFault {
	switch {
	case f.status == 400 && bytes.Contains(f.body, []byte(tokensExceeded)):
		f.code = CodeBatchTooLarge // 超出上下文长度：与 413 一样对半拆批
	case f.status == 422:
		f.code = CodeInvalidRequest // 请求校验失败，如模型名或题目畸形
	}
	return f
}

func (systemOneWire) encode(ep policy.JudgeEndpoint, qs []Question) ([]byte, error) {
	type instructions struct {
		Question  string          `json:"question"`
		Input     json.RawMessage `json:"input"`
		Material  string          `json:"material,omitempty"`
		Truncated bool            `json:"material_truncated,omitempty"`
	}
	type noul struct {
		Type         string            `json:"type"`
		Instructions instructions      `json:"instructions"`
		Criteria     map[string]string `json:"criteria,omitempty"`
	}
	questions := make(map[string]noul, len(qs))
	for _, q := range qs {
		n := noul{Type: "noul", Instructions: instructions{q.Question, q.Input, q.Material, q.Truncated}}
		if q.Criteria != "" {
			n.Criteria = map[string]string{"true": q.Criteria, "false": systemOneFalse}
		}
		questions[q.ID] = n
	}
	return json.Marshal(map[string]any{"model": ep.Model, "state": systemOneState, "questions": questions})
}

// decode 与 probability-judgment/1 同样严格：任何一题的答案不合形状，整批按 protocol_error 处理。
func (systemOneWire) decode(j *judgeRun, raw []byte, qs []Question, lo, hi int) {
	var resp struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Answers == nil {
		j.fault(CodeProtocolError)
		j.failRange(lo, hi, CodeProtocolError)
		return
	}
	model := resp.Model
	if model == "" {
		model = j.ep.Model
	}
	for i := lo; i < hi; i++ {
		a, ok := resp.Answers[qs[i].ID]
		if ok && (a.Type != "noul" || a.Noul == nil || math.IsNaN(*a.Noul) || *a.Noul < 0 || *a.Noul > 1) {
			j.fault(CodeProtocolError)
			j.failRange(lo, hi, CodeProtocolError)
			return
		}
	}
	for i := lo; i < hi; i++ {
		ans := &j.res.Answers[i]
		if a, ok := resp.Answers[qs[i].ID]; ok {
			ans.PYes, ans.Model, ans.Source = *a.Noul, model, "native"
		} else {
			ans.ErrCode = CodeMissingResult
		}
	}
	j.res.Healthy = true
}
