package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// 探测结论的机器码（Detail.Code）：Web 的"测试连接"与健康页按它显示本地化说明（设计 §15.5）。
const (
	CodeOK             = "ok"
	CodeNotConfigured  = "not_configured"
	CodeNoClient       = "no_client"
	CodeUnreachable    = "unreachable"
	CodeTimeout        = "timeout"
	CodeUnauthorized   = "unauthorized"
	CodeProtocolError  = "protocol_error"
	CodeSelfReported   = "self_reported"
	CodeInvalidRequest = "invalid_request"
	CodeNoAnswer       = "no_answer"
	CodeHTTPStatus     = "http_status"
	CodeSchemaMismatch = "schema_mismatch"
	CodeBadResponse    = "bad_response"
	// CodeRegionUnavailable：服务在当前网络位置不可用（HTTP 451 或说明地区受限的 403）。
	CodeRegionUnavailable = "region_unavailable"
)

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
	d.Data = map[string]any{"locality": policy.EffectiveLocality(ep.Locality), "model": ep.Model}
	if e.Model == nil {
		d.Status, d.Code, d.Why = Warning, CodeNoClient, "the model client is not available here, so the endpoint was not probed"
		return d
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := e.now()
	resp, err := e.Model.Complete(ctx, ep, model.Request{System: sampleSystem, User: sampleUser, Schema: json.RawMessage(sampleSchema), Timeout: probeTimeout})
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
			d.Why = fmt.Sprintf("the endpoint returned HTTP status %d for the structured-output sample request", se.Code)
			if se.Region {
				d.Code, d.Why, d.Next = CodeRegionUnavailable, regionWhy, regionNext
			}
		case errors.Is(err, model.ErrTimeout):
			d.Code, d.Why = CodeTimeout, "the endpoint did not answer the structured-output sample request in time"
		case errors.Is(err, model.ErrSchema): // 端点通了，只是输出没守 schema：提醒，不算坏
			d.Status, d.Code = Warning, CodeSchemaMismatch
			d.Why = "the endpoint answered, but its output does not follow the JSON schema of the structured-output sample request"
		case errors.Is(err, model.ErrBadResponse):
			d.Code, d.Why = CodeBadResponse, "the endpoint's response is not a valid chat completions response"
		default:
			d.Code, d.Why = CodeUnreachable, "the structured-output sample request failed: "+err.Error()
		}
		return d
	}
	_ = resp
	d.Why = fmt.Sprintf("answered the structured-output sample request in %d ms", ms)
	return d
}

func protocolOf(p string) string { return orDefault(p, policy.ProtocolProbability) }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
