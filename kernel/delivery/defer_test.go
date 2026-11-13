package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

type stubJudge struct {
	v   direct.Verdict
	err error
}

func (s stubJudge) Judge(context.Context, direct.JudgeCall) (direct.Verdict, error) {
	return s.v, s.err
}

// 只有明确答"是"才推迟；否、未定取否返回 false 无错误；不可用与出错返回 false 加错误（调用方按规则路由）。
func TestCanDefer(t *testing.T) {
	for _, c := range []struct {
		name    string
		j       stubJudge
		want    bool
		wantErr bool
	}{
		{"yes", stubJudge{v: direct.Verdict{Answer: "yes"}}, true, false},
		{"no", stubJudge{v: direct.Verdict{Answer: "no"}}, false, false},
		{"undecided default no", stubJudge{v: direct.Verdict{Answer: "no", LowConfidence: true}}, false, false},
		{"unavailable", stubJudge{v: direct.Verdict{Answer: "no", Unavailable: true}}, false, true},
		{"error", stubJudge{err: errors.New("timeout")}, false, true},
	} {
		got, err := CanDefer(context.Background(), c.j, direct.JudgeCall{})
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%s: got %v, %v", c.name, got, err)
		}
	}
}

// 题只带结构化状态，不带事项正文；用 single_shot 的 delivery-defer 模板。
func TestDeferCallCarriesStructuredStateOnly(t *testing.T) {
	it := domain.Item{ID: "i1", Title: "secret title", Body: "secret body", Priority: "high"}
	c := DeferCall(it, true, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if c.Pack != "decision-card" || c.Template != "delivery-defer" || c.Subject != "i1" || c.MaterialText != "" {
		t.Fatalf("%+v", c)
	}
	var in map[string]any
	if err := json.Unmarshal(c.Input, &in); err != nil || in["in_quiet_hours"] != true || in["item"] != "i1" || in["title"] != nil {
		t.Fatalf("input %s %v", c.Input, err)
	}
}
