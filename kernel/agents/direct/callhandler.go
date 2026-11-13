package direct

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
)

// CallHandler 是通用的判断型 call_agent 处理器（附录 C.7）：能力包事件规则调自定义判断型模板时，
// 对触发事项问一道题，答案只写判断记录，不改事项。委派回执核验（需要委派子事项）另有 verify.Handler。
func (s *Service) CallHandler() rules.Handler {
	return func(ctx context.Context, c rules.Call) error {
		tmpl, _ := c.Action["prompt"].(string)
		if tmpl == "" {
			return domain.NewError(domain.CodeInvalidRequest, "call_agent needs a prompt template name", "fix the rule")
		}
		if c.Item == nil {
			return domain.NewError(domain.CodeInvalidRequest, "call_agent judge needs a triggering item", "use an item event rule")
		}
		pack, _, _ := strings.Cut(c.Rule, "/")
		in, _ := json.Marshal(map[string]any{"item": c.Item.ID, "kind": c.Item.Kind, "title": c.Item.Title, "tags": c.Item.Tags})
		_, err := s.Judge(ctx, JudgeCall{Pack: pack, Template: tmpl, Input: in, MaterialText: c.Item.Title + "\n" + c.Item.Body,
			Subject: c.Item.ID, Project: c.Item.Project})
		return err
	}
}
