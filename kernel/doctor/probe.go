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

func probeJudge(ctx context.Context, f Facts, e Env) Detail {
	d := Detail{Subject: "judge_endpoint", Status: OK}
	ep := f.Policy.JudgeEndpoint
	if ep == nil || ep.URL == "" {
		d.Why = "not configured (optional); not an error"
		return d
	}
	d.Data = map[string]any{"locality": policy.EffectiveLocality(ep.Locality), "protocol": protocolOf(ep.Protocol)}
	if e.Model == nil {
		d.Status, d.Why = Warning, "the model client is not available here, so the endpoint was not probed"
		return d
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := e.now()
	res, err := e.Model.Judge(ctx, *ep, []model.Question{{ID: "doctor-sample", Question: sampleQuestion, Criteria: sampleCriteria, Material: sampleMaterial}},
		model.JudgeOptions{SingleShot: true})
	ms := e.now().Sub(start).Milliseconds()
	d.Data["elapsed_ms"] = ms
	fail := func(why, next string) Detail {
		d.Status, d.Why, d.Next = Error, why, next
		return d
	}
	if err != nil {
		return fail("the probe could not be sent: "+err.Error(), "check the judge endpoint settings")
	}
	for _, code := range res.Faults {
		switch code {
		case model.CodeSelfReported:
			return fail("the endpoint reports probability_source self_reported; it does not count as a classification endpoint (appendix B.2 / §8.8)",
				"use an endpoint whose probabilities are native or computed from logprobs")
		case model.CodeProtocolError:
			return fail(fmt.Sprintf("the endpoint's response does not match protocol %s (protocol identifier or response shape is wrong)", protocolOf(ep.Protocol)),
				"check the endpoint's protocol and the protocol setting")
		case model.CodeUnauthorized:
			return fail("the endpoint rejected the credentials (unauthorized)", "check the API key of the judge endpoint")
		case model.CodeInvalidRequest:
			return fail("the endpoint rejected the sample request as invalid", "check the endpoint's protocol and the model name")
		}
	}
	a := res.Answers[0]
	if a.ErrCode != "" || !res.Healthy {
		return fail("the endpoint did not answer the sample question ("+orDefault(a.ErrCode, "no healthy response")+")", "check that the endpoint is reachable and within timeout_ms")
	}
	d.Data["probability_source"] = a.Source
	d.Why = fmt.Sprintf("answered the built-in sample question in %d ms (protocol %s, probability_source %s)", ms, protocolOf(ep.Protocol), a.Source)
	return d
}

func probeGenerate(ctx context.Context, f Facts, e Env) Detail {
	d := Detail{Subject: "generate_endpoint", Status: OK}
	ep := f.Policy.GenerateEndpoint
	if ep.URL == "" || ep.Model == "" {
		d.Why = "not configured (optional); not an error"
		return d
	}
	d.Data = map[string]any{"locality": policy.EffectiveLocality(ep.Locality)}
	if e.Model == nil {
		d.Status, d.Why = Warning, "the model client is not available here, so the endpoint was not probed"
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
			d.Why = fmt.Sprintf("the endpoint returned HTTP status %d for the structured-output sample request", se.Code)
		case errors.Is(err, model.ErrTimeout):
			d.Why = "the endpoint did not answer the structured-output sample request in time"
		case errors.Is(err, model.ErrSchema): // 端点通了，只是输出没守 schema：提醒，不算坏
			d.Status = Warning
			d.Why = "the endpoint answered, but its output does not follow the JSON schema of the structured-output sample request"
		case errors.Is(err, model.ErrBadResponse):
			d.Why = "the endpoint's response is not a valid chat completions response"
		default:
			d.Why = "the structured-output sample request failed: " + err.Error()
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
