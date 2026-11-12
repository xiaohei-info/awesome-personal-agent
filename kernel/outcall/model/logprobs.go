package model

// chat-logprobs（§8.8 末段）：对话补全端点返回首个输出 token 的对数概率，内核换算成 p_yes。
// 逐题一次请求，不能批量；并发受 max_batch 限制。是否对用户开放取决于 SPIKE-LOGPROB（§21.3），
// 本版只在对话端点替身上验证换算路径，设置页不提供该选项。

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

const logprobsSystem = "You are a classifier. Answer the user's yes/no question with exactly one word: yes or no."

// minAnswerMass 是 yes 与 no 的概率和下限，低于它说明模型没在回答这个问题。
const minAnswerMass = 0.5

func (j *judgeRun) logprobs(ctx context.Context, qs []Question) error {
	sem := make(chan struct{}, intOr(j.ep.MaxBatch, policy.DefaultJudgeMaxBatch))
	var wg sync.WaitGroup
	var mu sync.Mutex // 保护 j.res 的汇总字段与请求计数
	var firstErr error
	for i := range qs {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			p, f, err := j.logprobOne(ctx, qs[i], &mu)
			mu.Lock()
			defer mu.Unlock()
			a := &j.res.Answers[i]
			switch {
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			case f != "":
				a.ErrCode = f
				if f == CodeUnauthorized || f == CodeInvalidRequest || f == CodeProtocolError {
					j.fault(f)
					j.res.Unavailable = j.res.Unavailable || f == CodeUnauthorized
				}
			default:
				a.PYes, a.Model, a.Source = p, j.ep.Model, "logprobs"
				j.res.Healthy = true
			}
		}(i)
	}
	wg.Wait()
	return firstErr
}

// logprobOne 问一题，返回 p_yes 或错误码。
func (j *judgeRun) logprobOne(ctx context.Context, q Question, mu *sync.Mutex) (float64, string, error) {
	user := q.Question
	if q.Criteria != "" {
		user += "\n\n" + q.Criteria
	}
	user += "\n\ninput: " + string(q.Input)
	if q.Material != "" {
		user += "\n\nmaterial:\n" + q.Material
	}
	body, _ := json.Marshal(map[string]any{
		"model":        j.ep.Model,
		"messages":     []map[string]string{{"role": "system", "content": logprobsSystem}, {"role": "user", "content": user}},
		"max_tokens":   1,
		"temperature":  0,
		"logprobs":     true,
		"top_logprobs": 20,
	})
	var n int
	raw, f, err := j.retry(ctx, func(ctx context.Context) ([]byte, *callFault) {
		return j.c.post(ctx, strings.TrimRight(j.ep.URL, "/")+"/chat/completions", j.ep.APIKey, body, j.timeout(), &n)
	})
	mu.Lock()
	j.res.Requests += n
	mu.Unlock()
	if err != nil {
		return 0, "", err
	}
	if f != nil {
		return 0, f.code, nil
	}
	p, code := yesProbability(raw)
	return p, code, nil
}

// yesProbability 由响应里首个输出 token 的候选对数概率换算：
// 归一化后等于 yes、no 的候选概率各自相加，p_yes = P(yes)/(P(yes)+P(no))；两者之和不足 0.5 记 no_answer_mass。
func yesProbability(raw []byte) (float64, string) {
	var out struct {
		Choices []struct {
			Logprobs struct {
				Content []struct {
					Token       string  `json:"token"`
					Logprob     float64 `json:"logprob"`
					TopLogprobs []struct {
						Token   string  `json:"token"`
						Logprob float64 `json:"logprob"`
					} `json:"top_logprobs"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.Choices) == 0 || len(out.Choices[0].Logprobs.Content) == 0 {
		return 0, CodeProtocolError // 端点没有返回对数概率：不能用这条路径
	}
	first := out.Choices[0].Logprobs.Content[0]
	cands := first.TopLogprobs
	if len(cands) == 0 { // 有的端点只给被采样 token
		cands = append(cands, struct {
			Token   string  `json:"token"`
			Logprob float64 `json:"logprob"`
		}{first.Token, first.Logprob})
	}
	var yes, no float64
	for _, c := range cands {
		switch strings.ToLower(strings.Trim(strings.TrimSpace(c.Token), ".,!?;:\"'")) {
		case "yes":
			yes += math.Exp(c.Logprob)
		case "no":
			no += math.Exp(c.Logprob)
		}
	}
	if yes+no < minAnswerMass || math.IsNaN(yes+no) {
		return 0, CodeNoAnswerMass
	}
	return yes / (yes + no), ""
}
