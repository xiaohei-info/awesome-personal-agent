// Package rulejudge 把判断型 call_agent 接到规则引擎。单列一包：direct 不能依赖 kernel/rules（delivery→direct，rules 的测试依赖 delivery，会成环）。
package rulejudge

import (
	"context"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
)

// Judger 由 direct.Service 满足。
type Judger interface {
	JudgeItem(ctx context.Context, pack, tmpl string, it domain.Item) error
}

// Handler 是通用判断型 call_agent 处理器：对触发事项问 prompt 指定的判断型模板，答案只留痕。
func Handler(j Judger) rules.Handler {
	return func(ctx context.Context, c rules.Call) error {
		tmpl, _ := c.Action["prompt"].(string)
		if tmpl == "" {
			return domain.NewError(domain.CodeInvalidRequest, "call_agent needs a prompt template name", "fix the rule")
		}
		if c.Item == nil {
			return domain.NewError(domain.CodeInvalidRequest, "call_agent judge needs a triggering item", "use an item event rule")
		}
		pack, _, _ := strings.Cut(c.Rule, "/")
		return j.JudgeItem(ctx, pack, tmpl, *c.Item)
	}
}
