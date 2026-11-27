package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 探测用的内置固定文本：不取账本里的任何内容（设计 §15.5）。
const (
	sampleQuestion = "Is 7 greater than 3?"
	sampleCriteria = "Answer yes only if the statement is arithmetically true."
	sampleMaterial = "The statement to judge is: seven is greater than three."
	sampleSystem   = "You are a configuration probe. Reply with the requested JSON only."
	sampleUser     = "Is 7 greater than 3? Answer yes or no; set sufficient to true when the statement is enough to decide."
	sampleSchema   = `{"type":"object","properties":{"answer":{"type":"string","enum":["yes","no"]},"sufficient":{"type":"boolean"}},"required":["answer","sufficient"],"additionalProperties":false}`

	probeTimeout = 30 * time.Second
)

// 决策卡片文案的模板：生成端点的真实用途（§8）。自检用它的结构约束与提示词探测，端点只答得了小样例却答不了它时才看得出来。
const (
	DraftPack     = "decision-card"
	DraftTemplate = "decision-card-copy"

	sampleDraftInput    = `{"item_id":"itm_doctor_sample","title":"选择发布时间","options":[{"id":"A","label":"今晚发布"},{"id":"B","label":"明天上午发布"}]}`
	sampleDraftMaterial = "服务已完成灰度验证，请选择正式发布窗口：A 今晚 20:00，B 明天上午 10:00。"
)

// draftProbe 取生效中的起草模板，组出探测请求；包或模板不可用（包被拒载、不是生成型）返回 false，由调用方退回内置小样例。
func draftProbe(e Env) (model.Request, bool) {
	if e.Packs == nil {
		return model.Request{}, false
	}
	for _, p := range e.Packs.List() {
		if p.Name != DraftPack {
			continue
		}
		pr, ok := p.Prompts[DraftTemplate]
		if !ok || pr.Meta.Agent != "generate" || len(pr.Meta.OutputSchema) == 0 {
			return model.Request{}, false
		}
		schema, err := json.Marshal(pr.Meta.OutputSchema)
		if err != nil {
			return model.Request{}, false
		}
		req := direct.PromptRequest(pr, direct.Call{Input: json.RawMessage(sampleDraftInput), Material: sampleDraftMaterial}, "run_doctor", nil, schema)
		req.Timeout = probeTimeout
		return req, true
	}
	return model.Request{}, false
}

const (
	regionWhy  = "the service is not available from this network location (HTTP 451, or a 403 that cites the region)"
	regionNext = "use a reachable compatible gateway URL or a proxy"
)

// ProbeJudge 用内置示例题探测判断端点 ep（nil 表示未配置）。`apa doctor`、GET /doctor 与 Web 的"测试连接"共用。
func ProbeJudge(ctx context.Context, ep *policy.JudgeEndpoint, e Env) Detail {
	d := Detail{Subject: "judge_endpoint", Status: OK, Code: CodeOK}
	if ep == nil || ep.URL == "" {
		d.Code, d.Why = CodeNotConfigured, "not configured (optional); not an error"
		return d
	}
	d.Data = map[string]any{"locality": policy.EffectiveLocality(ep.Locality), "protocol": protocolOf(ep.Protocol)}
	if e.Model == nil {
		d.Status, d.Code, d.Why = Warning, CodeNoClient, "the model client is not available here, so the endpoint was not probed"
		return d
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := e.now()
	res, err := e.Model.Judge(ctx, *ep, []model.Question{{ID: "doctor-sample", Question: sampleQuestion, Criteria: sampleCriteria, Material: sampleMaterial}},
		model.JudgeOptions{SingleShot: true})
	ms := e.now().Sub(start).Milliseconds()
	d.Data["elapsed_ms"] = ms
	fail := func(code, why, next string) Detail {
		d.Status, d.Code, d.Why, d.Next = Error, code, why, next
		return d
	}
	if err != nil {
		return fail(CodeUnreachable, "the probe could not be sent: "+err.Error(), "check the judge endpoint settings")
	}
	for _, code := range res.Faults {
		switch code {
		case model.CodeSelfReported:
			return fail(CodeSelfReported, "the endpoint reports probability_source self_reported; it does not count as a classification endpoint (appendix B.2 / §8.8)",
				"use an endpoint whose probabilities are native or computed from logprobs")
		case model.CodeProtocolError:
			return fail(CodeProtocolError, fmt.Sprintf("the endpoint's response does not match protocol %s (protocol identifier or response shape is wrong)", protocolOf(ep.Protocol)),
				"check the endpoint's protocol and the protocol setting")
		case model.CodeRegionUnavailable:
			return fail(CodeRegionUnavailable, regionWhy, regionNext)
		case model.CodeUnauthorized:
			return fail(CodeUnauthorized, "the endpoint rejected the credentials (unauthorized)", "check the API key of the judge endpoint")
		case model.CodeInvalidRequest:
			return fail(CodeInvalidRequest, "the endpoint rejected the sample request as invalid", "check the endpoint's protocol and the model name")
		}
	}
	a := res.Answers[0]
	if a.ErrCode != "" || !res.Healthy {
		code := CodeNoAnswer
		if a.ErrCode == model.CodeTimeout {
			code = CodeTimeout
		}
		return fail(code, "the endpoint did not answer the sample question ("+orDefault(a.ErrCode, "no healthy response")+")", "check that the endpoint is reachable and within timeout_ms")
	}
	d.Data["probability_source"] = a.Source
	if a.Model != "" {
		d.Data["model"] = a.Model
	}
	d.Why = fmt.Sprintf("answered the built-in sample question in %d ms (protocol %s, probability_source %s)", ms, protocolOf(ep.Protocol), a.Source)
	return d
}

// ProbeGenerate 用内置的结构化输出示例请求探测生成端点 ep（URL 或模型为空表示未配置）。
func ProbeGenerate(ctx context.Context, ep policy.GenerateEndpoint, e Env) Detail {
	d := Detail{Subject: "generate_endpoint", Status: OK, Code: CodeOK}
	if ep.URL == "" || ep.Model == "" {
		d.Code, d.Why = CodeNotConfigured, "not configured (optional); not an error"
		return d
	}
	proto := policy.GenerateProtocol(ep.Protocol)
	d.Data = map[string]any{"locality": policy.EffectiveLocality(ep.Locality), "model": ep.Model, "protocol": proto, "structured_output": model.StructuredOutput(proto)}
	if e.Model == nil {
		d.Status, d.Code, d.Why = Warning, CodeNoClient, "the model client is not available here, so the endpoint was not probed"
		return d
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := e.now()
	req, real := draftProbe(e)
	if !real {
		req = model.Request{System: sampleSystem, User: sampleUser, Schema: json.RawMessage(sampleSchema), Timeout: probeTimeout}
	}
	req.Effort = policy.Thinking{}.ModelEffort(false) // 自检用默认的写作档
	d.Data["sample"] = map[bool]string{true: "draft", false: "builtin"}[real]
	resp, err := e.Model.Complete(ctx, ep, req)
	ms := e.now().Sub(start).Milliseconds()
	d.Data["elapsed_ms"] = ms
	if err != nil {
		d.Status, d.Next = Error, "check URL, API key and model name of the generate endpoint, and that the endpoint supports structured output"
		var se *model.StatusError
		switch {
		case errors.As(err, &se):
			d.Code, d.Data["status"] = CodeHTTPStatus, se.Code
			if se.Code == 401 || se.Code == 403 {
				d.Code = CodeUnauthorized
			}
			d.Why = fmt.Sprintf("the endpoint returned HTTP status %d for the structured-output sample request (protocol %s)", se.Code, proto)
			if se.Message != "" {
				d.Why += ": " + se.Message
			}
			if se.Region {
				d.Code, d.Why, d.Next = CodeRegionUnavailable, regionWhy, regionNext
			}
		case errors.Is(err, model.ErrTimeout):
			d.Code, d.Why = CodeTimeout, "the endpoint did not answer the structured-output sample request in time"
		case errors.Is(err, model.ErrSchema) && real: // 真实模板的输出过不了：起草会逐条失败，是坏的
			d.Code = CodeDraftSchema
			d.Data["template"] = DraftPack + "/" + DraftTemplate
			d.Why = "the endpoint answered, but its output does not follow the JSON schema of the " + DraftTemplate + " template, so drafting fails on every call: " + err.Error()
			d.Next = "the endpoint may not support structured output over protocol " + proto + "; try another protocol. The shape in the message names what the endpoint returned"
			if shape := model.SchemaShape(err); shape != "" {
				d.Data["shape"] = shape
			}
		case errors.Is(err, model.ErrSchema): // 端点通了，只是输出没守小样例的 schema：提醒，不算坏
			d.Status, d.Code = Warning, CodeSchemaMismatch
			d.Why = "the endpoint answered, but its output does not follow the JSON schema of the structured-output sample request"
		case errors.Is(err, model.ErrBadResponse):
			d.Code, d.Why = CodeBadResponse, "the endpoint's response is not a valid "+proto+" response"
			d.Next = "check that the endpoint speaks protocol " + proto + ", or choose the protocol it speaks"
		default:
			d.Code, d.Why = CodeUnreachable, "the structured-output sample request failed: "+err.Error()
		}
		return d
	}
	if !resp.Usage.IsZero() {
		d.Data["usage"] = resp.Usage
	}
	d.Why = fmt.Sprintf("answered the structured-output sample request in %d ms (protocol %s, %s)", ms, proto, model.StructuredOutput(proto))
	if real {
		d.Why = fmt.Sprintf("answered the %s structured-output sample request in %d ms (protocol %s, %s)", DraftTemplate, ms, proto, model.StructuredOutput(proto))
	}
	return d
}

func protocolOf(p string) string { return orDefault(p, policy.ProtocolProbability) }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
