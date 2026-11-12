package model

// 概率判断协议 v1 客户端（设计 §8.5、§8.8）：把一批是非题整批提交给判断端点，逐题取回 p_yes。
// 只做线上协议与错误处置，不做定案（是否落入不确定带由调用方按 §8.3 判定）、不写判断记录、不保存复用结果；
// 调用方按 Fingerprint 取已有结果后，只把未命中的题交给 Judge。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
)

// 按题与整批的错误码（§8.8 错误码表）。Answer.ErrCode 取这些值之一。
const (
	CodeInvalidRequest   = "invalid_request"
	CodeUnauthorized     = "unauthorized"
	CodeTimeout          = "timeout"
	CodeBatchTooLarge    = "batch_too_large"
	CodeRateLimited      = "rate_limited"
	CodeUnavailable      = "unavailable"
	CodeProtocolError    = "protocol_error"
	CodeInputTooLarge    = "input_too_large"
	CodeUnsupportedQuest = "unsupported_question"
	CodeInternal         = "internal"
	CodeMissingResult    = "missing_result"
	// CodeSelfReported 不在错误码表里：端点报 self_reported 时它不算分类端点，这一级记为不可用（§8.8 概率语义）。
	CodeSelfReported = "self_reported"
	// CodeNoAnswerMass 只出现在 chat-logprobs：yes 与 no 的概率和不足 0.5。
	CodeNoAnswerMass = "no_answer_mass"
)

const (
	// maxRetries 是超时、5xx 与限流等待共用的重试次数上限（§8.8）。
	maxRetries = 2
	// backoffBase 是第一次退避；之后每次翻倍。
	backoffBase = 500 * time.Millisecond
)

// Question 是一道是非题。Material 必须是已按外发范围裁剪的资料（§14.14），用 QuestionFrom 构造可在类型上保证这一点。
type Question struct {
	ID        string          // 批内唯一；留空时 Judge 按顺序编为 q1、q2…
	Question  string          // 固定问法
	Criteria  string          // 判据，可空
	Input     json.RawMessage // JSON 对象；空表示 {}
	Material  string          // 资料正文
	Truncated bool            // 调用方已截断时置真；Judge 自己截断时也会置真
}

// QuestionFrom 由裁剪后的来源内容组织一道题：元信息进 input，正文进 material。
// 只接受 registry.Trimmed，因此未经 registry.Trim 的内容送不出去；stats 是可选的 Event 统计（JSON 对象）。
func QuestionFrom(id, question, criteria string, t registry.Trimmed, stats json.RawMessage) Question {
	in := map[string]any{"source_type": t.Type, "sender": t.Sender, "subject": t.Subject,
		"occurred_at": t.OccurredAt, "link": t.Link, "summary": t.Summary}
	if len(stats) > 0 {
		in["stats"] = json.RawMessage(stats)
	}
	b, _ := json.Marshal(in)
	return Question{ID: id, Question: question, Criteria: criteria, Input: b, Material: t.Detail}
}

// JudgeOptions 是一次调用的选项。
type JudgeOptions struct {
	// SingleShot 为真时超时与 5xx 不重试（模板声明 single_shot，§8.3）。
	SingleShot bool
}

// Answer 是一道题的结果，与传入的题一一对应、顺序相同。
// ErrCode 为空表示有答案：PYes 有效；否则这道题交下一级，ErrCode 是原因。
type Answer struct {
	ID        string
	PYes      float64
	ErrCode   string
	Model     string // 端点自报（chat-logprobs 时为配置的模型名）
	Source    string // probability_source：native 或 logprobs
	Truncated bool   // 送出的资料被截断
}

// JudgeResult 是一次 Judge 的全部结果。
type JudgeResult struct {
	Answers []Answer
	// Faults 是整批层面的故障码（去重，按出现顺序），调用方据此写健康视图：
	// invalid_request、protocol_error、unauthorized、self_reported 要进健康视图；超时与限流只体现在各题的 ErrCode。
	Faults []string
	// Unavailable 为真时调用方应把判断端点标为不可用，直到下一次 Healthy（401、403 或 self_reported）。
	Unavailable bool
	// Healthy 为真表示至少一批得到了合法的 200 响应且可采信，调用方据此清除不可用标记。
	Healthy bool
	// Requests 是实际发出的 HTTP 请求数（含重试与拆批），供诊断。
	Requests int
}

// Judge 按判断端点的协议取回每题的 p_yes。端点未配置返回 ErrNotConfigured；
// 其余失败都落在各题的 ErrCode 上（交下一级），不会返回伪造的概率。
func (c *Client) Judge(ctx context.Context, ep policy.JudgeEndpoint, qs []Question, opt JudgeOptions) (JudgeResult, error) {
	if ep.URL == "" {
		return JudgeResult{}, ErrNotConfigured
	}
	qs, err := prepare(qs, intOr(ep.MaxMaterialChars, policy.DefaultJudgeMaxMaterialChars))
	if err != nil {
		return JudgeResult{}, err
	}
	j := &judgeRun{c: c, ep: ep, opt: opt, res: JudgeResult{Answers: make([]Answer, len(qs))}}
	for i, q := range qs {
		j.res.Answers[i] = Answer{ID: q.ID, Truncated: q.Truncated}
	}
	switch ep.Protocol {
	case "", policy.ProtocolProbability:
		size := intOr(ep.MaxBatch, policy.DefaultJudgeMaxBatch)
		for lo := 0; lo < len(qs); lo += size {
			hi := min(lo+size, len(qs))
			if err := j.batch(ctx, qs, lo, hi, true); err != nil {
				return JudgeResult{}, err
			}
		}
	case policy.ProtocolChatLogprobs:
		if err := j.logprobs(ctx, qs); err != nil {
			return JudgeResult{}, err
		}
	default:
		return JudgeResult{}, fmt.Errorf("model: unsupported judge protocol %q", ep.Protocol)
	}
	return j.res, nil
}

func intOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// prepare 复制题目、补题号、截断资料。截断按字符（rune），不按字节。
func prepare(in []Question, maxChars int) ([]Question, error) {
	out := make([]Question, len(in))
	seen := map[string]bool{}
	for i, q := range in {
		if q.ID == "" {
			q.ID = "q" + strconv.Itoa(i+1)
		}
		if seen[q.ID] {
			return nil, fmt.Errorf("model: duplicate question id %q", q.ID)
		}
		seen[q.ID] = true
		if len(q.Input) == 0 {
			q.Input = json.RawMessage(`{}`)
		}
		if len(q.Material) > maxChars { // 字节数不超过上限则字符数必不超过
			if r := []rune(q.Material); len(r) > maxChars {
				q.Material, q.Truncated = string(r[:maxChars]), true
			}
		}
		out[i] = q
	}
	return out, nil
}

type judgeRun struct {
	c   *Client
	ep  policy.JudgeEndpoint
	opt JudgeOptions
	res JudgeResult
}

func (j *judgeRun) fault(code string) {
	for _, f := range j.res.Faults {
		if f == code {
			return
		}
	}
	j.res.Faults = append(j.res.Faults, code)
}

// failRange 把 [lo,hi) 的题记为同一个错误码，交下一级。
func (j *judgeRun) failRange(lo, hi int, code string) {
	for i := lo; i < hi; i++ {
		j.res.Answers[i].ErrCode = code
	}
}

// batch 提交 qs[lo:hi]。canSplit 为真时遇 413 对半拆开各发一次；拆开后的两半不再拆（§8.8）。
func (j *judgeRun) batch(ctx context.Context, qs []Question, lo, hi int, canSplit bool) error {
	body, err := j.encode(qs[lo:hi])
	if err != nil {
		return err
	}
	raw, f, err := j.retry(ctx, func(ctx context.Context) ([]byte, *callFault) {
		return j.c.post(ctx, j.ep.URL, j.ep.APIKey, body, j.timeout(), &j.res.Requests)
	})
	if err != nil {
		return err
	}
	if f != nil {
		switch f.code {
		case CodeBatchTooLarge:
			if canSplit && hi-lo > 1 {
				mid := lo + (hi-lo)/2
				if err := j.batch(ctx, qs, lo, mid, false); err != nil {
					return err
				}
				return j.batch(ctx, qs, mid, hi, false)
			}
		case CodeInvalidRequest, CodeProtocolError, CodeUnauthorized:
			j.fault(f.code)
			j.res.Unavailable = j.res.Unavailable || f.code == CodeUnauthorized
		}
		j.failRange(lo, hi, f.code)
		return nil
	}
	j.decode(raw, qs, lo, hi)
	return nil
}

func (j *judgeRun) timeout() time.Duration {
	return time.Duration(intOr(j.ep.TimeoutMs, policy.DefaultJudgeTimeoutMs)) * time.Millisecond
}

func (j *judgeRun) encode(qs []Question) ([]byte, error) {
	type wire struct {
		ID        string          `json:"id"`
		Question  string          `json:"question"`
		Criteria  string          `json:"criteria,omitempty"`
		Input     json.RawMessage `json:"input"`
		Material  string          `json:"material,omitempty"`
		Truncated bool            `json:"material_truncated,omitempty"`
	}
	ws := make([]wire, len(qs))
	for i, q := range qs {
		ws[i] = wire{q.ID, q.Question, q.Criteria, q.Input, q.Material, q.Truncated}
	}
	rid := make([]byte, 8)
	if _, err := rand.Read(rid); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"protocol": policy.ProtocolProbability, "request_id": "jdg_" + hex.EncodeToString(rid), "questions": ws})
}

// decode 校验 200 响应并填入 [lo,hi) 的结果。任何协议违例都按整批 protocol_error 处理，不采信其中任何一题。
func (j *judgeRun) decode(raw []byte, qs []Question, lo, hi int) {
	var resp struct {
		Protocol string `json:"protocol"`
		Model    string `json:"model"`
		Source   string `json:"probability_source"`
		Results  []struct {
			ID    string   `json:"id"`
			PYes  *float64 `json:"p_yes"`
			Error *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	bad := func() {
		j.fault(CodeProtocolError)
		j.failRange(lo, hi, CodeProtocolError)
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Protocol != policy.ProtocolProbability || resp.Model == "" || resp.Results == nil {
		bad()
		return
	}
	switch resp.Source {
	case "native", "logprobs":
	case "self_reported":
		// 自报数字不采信：这一级记为不可用并进健康视图，题交下一级。
		j.fault(CodeSelfReported)
		j.res.Unavailable = true
		j.failRange(lo, hi, CodeSelfReported)
		return
	default:
		bad()
		return
	}
	idx := make(map[string]int, hi-lo)
	for i := lo; i < hi; i++ {
		idx[qs[i].ID] = i
	}
	got := map[int]bool{}
	for _, r := range resp.Results {
		if (r.PYes == nil) == (r.Error == nil) {
			bad() // p_yes 与 error 必须二选一
			return
		}
		i, known := idx[r.ID]
		if !known {
			continue // 未知题号忽略
		}
		if got[i] {
			bad()
			return
		}
		got[i] = true
		a := &j.res.Answers[i]
		switch {
		case r.PYes != nil:
			if math.IsNaN(*r.PYes) || *r.PYes < 0 || *r.PYes > 1 {
				bad()
				return
			}
			a.PYes, a.Model, a.Source = *r.PYes, resp.Model, resp.Source
		default:
			a.ErrCode, a.Model = questionCode(r.Error.Code), resp.Model
		}
	}
	for i := lo; i < hi; i++ {
		if !got[i] {
			j.res.Answers[i].ErrCode = CodeMissingResult
		}
	}
	j.res.Healthy = true
}

// questionCode 把端点给的按题错误码收敛到协议允许的三个；未知码按 internal。
func questionCode(c string) string {
	switch c {
	case CodeInputTooLarge, CodeUnsupportedQuest, CodeInternal:
		return c
	}
	return CodeInternal
}

// callFault 是一次 HTTP 调用的失败，code 取错误码表。
type callFault struct {
	code       string
	retryAfter time.Duration // 仅 429，且响应带了 Retry-After
}

// post 发一次 POST，返回 2xx 的响应体或归类后的故障。err 非空只表示调用方的 ctx 被取消。
func (c *Client) post(ctx context.Context, url, key string, body []byte, timeout time.Duration, count *int) ([]byte, *callFault) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &callFault{code: CodeInvalidRequest}
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	*count++
	resp, err := hc.Do(req)
	if err != nil {
		return nil, transportFault(ctx, cctx)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, transportFault(ctx, cctx)
	}
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		if len(raw) > maxBody {
			return nil, &callFault{code: CodeProtocolError}
		}
		return raw, nil
	case s == 400:
		return nil, &callFault{code: CodeInvalidRequest}
	case s == 401 || s == 403:
		return nil, &callFault{code: CodeUnauthorized}
	case s == 408 || s == 504:
		return nil, &callFault{code: CodeTimeout}
	case s == 413:
		return nil, &callFault{code: CodeBatchTooLarge}
	case s == 429:
		return nil, &callFault{code: CodeRateLimited, retryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case s >= 500:
		return nil, &callFault{code: CodeUnavailable}
	}
	return nil, &callFault{code: CodeProtocolError} // 其余状态（重定向、其他 4xx）不在协议内
}

// transportFault 区分调用方取消（原样上报，由 retry 转成错误）与超时、连接失败。
func transportFault(parent, call context.Context) *callFault {
	switch {
	case parent.Err() != nil:
		return &callFault{code: "canceled"}
	case errors.Is(call.Err(), context.DeadlineExceeded):
		return &callFault{code: CodeTimeout}
	}
	return &callFault{code: CodeUnavailable}
}

// retryAfter 解析秒数；缺失或无法解析返回 -1（按默认退避）。
// shortcut: 不解析 HTTP 日期形式（需经 kernel/clock 取当前时间），遇到再补。
func retryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if n, err := strconv.Atoi(h); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return -1
}

// retry 按 §8.8 处置一次调用：超时与 5xx 退避重试至多 maxRetries 次（SingleShot 不重试），
// 429 等 Retry-After 后重发、等待不超过 timeout_ms，其余故障直接返回。err 非空只表示 ctx 被取消。
func (j *judgeRun) retry(ctx context.Context, call func(context.Context) ([]byte, *callFault)) ([]byte, *callFault, error) {
	for attempt := 0; ; attempt++ {
		raw, f := call(ctx)
		if f == nil {
			return raw, nil, nil
		}
		if f.code == "canceled" {
			return nil, nil, ctx.Err()
		}
		var wait time.Duration
		switch {
		case attempt >= maxRetries:
			return nil, f, nil
		case f.code == CodeTimeout || f.code == CodeUnavailable:
			if j.opt.SingleShot {
				return nil, f, nil
			}
			wait = backoffBase << attempt
		case f.code == CodeRateLimited:
			wait = f.retryAfter
			if wait < 0 {
				wait = backoffBase << attempt
			}
			if wait > j.timeout() {
				return nil, f, nil // 等不起：交下一级
			}
		default:
			return nil, f, nil
		}
		if err := j.c.sleep(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Fingerprint 是一道判断题的输入指纹（§8.5）：模板版本、问法与判据、结构化输入、裁剪后的资料正文、
// 后端种类与端点标识（地址、协议、模型名）。换端点或模型指纹随之变化；Event 统计不在 stats 之外的输入里，
// 调用方构造 input 时不要把统计放进去参与指纹（QuestionFrom 的 stats 因此被排除）。
func Fingerprint(ep policy.JudgeEndpoint, templateVersion string, q Question) string {
	in := q.Input
	if len(in) > 0 {
		var m map[string]json.RawMessage
		if json.Unmarshal(in, &m) == nil {
			delete(m, "stats")
			in, _ = json.Marshal(m) // map 序列化按键排序，指纹与键序无关
		}
	}
	if p, err := prepare([]Question{q}, intOr(ep.MaxMaterialChars, policy.DefaultJudgeMaxMaterialChars)); err == nil {
		q = p[0] // 指纹按实际送出的（截断后）资料算
	}
	proto := ep.Protocol
	if proto == "" {
		proto = policy.ProtocolProbability
	}
	b, _ := json.Marshal([]any{templateVersion, q.Question, q.Criteria, json.RawMessage(in), q.Material, "classifier", ep.URL, proto, ep.Model})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
