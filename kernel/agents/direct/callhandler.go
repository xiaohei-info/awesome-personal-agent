package direct

import (
	"context"
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// JudgeItem 对事项问一道判断题，答案只写判断记录，不改事项（附录 C.7：通用规则里判断型的答案只留痕）。
// 委派回执核验需要委派子事项，另有 verify.Handler。
func (s *Service) JudgeItem(ctx context.Context, pack, tmpl string, it domain.Item) error {
	in, _ := json.Marshal(map[string]any{"item": it.ID, "kind": it.Kind, "title": it.Title, "tags": it.Tags})
	_, err := s.Judge(ctx, JudgeCall{Pack: pack, Template: tmpl, Input: in, MaterialText: it.Title + "\n" + it.Body,
		Subject: it.ID, Project: it.Project})
	return err
}
