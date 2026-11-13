package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 投递推迟判定（设计 §7.4、§8.9）：策略开启 threshold_judge 时，决策投递只再问一道判断型"这条决策可以晚点送吗"
// （decision-card 包的 delivery-defer，single_shot，上限 0.85，未定取否）。判定只能降低打扰：
// 是 → 推迟到合并窗口结束后单独投递；否、未定、端点超时或不可用、判断失败 → 按优先级路由，不重试。
const (
	DeferPack     = "decision-card"
	DeferTemplate = "delivery-defer"
)

// Judge 是级联判断入口（*direct.Service 满足它）。
type Judge interface {
	Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error)
}

// DeferCall 组织推迟判定的题。事项正文是外部内容，只给结构化状态，不进资料段。
func DeferCall(it domain.Item, inQuiet bool, now time.Time) direct.JudgeCall {
	var askDeadline *time.Time
	if it.Ask != nil {
		askDeadline = it.Ask.Deadline
	}
	in, _ := json.Marshal(map[string]any{"item": it.ID, "priority": it.Priority, "deadline": it.Deadline,
		"ask_deadline": askDeadline, "in_quiet_hours": inQuiet, "now": now.UTC().Truncate(time.Minute)})
	return direct.JudgeCall{Pack: DeferPack, Template: DeferTemplate, Input: in, Subject: it.ID, Project: it.Project, NoStats: true}
}

// CanDefer 只有明确答"是"才返回 true；否与未定（保守默认"否"）返回 false。
// 返回的 Verdict 带判断记录（Written 时），调用方把它挂到 Event 载荷的 judgment 字段，供 apa why 回放（§6.7）；
// 端点不可用或判断失败返回 false 和错误，调用方按规则路由并记健康视图，不重试。
func CanDefer(ctx context.Context, j Judge, c direct.JudgeCall) (bool, direct.Verdict, error) {
	v, err := j.Judge(ctx, c)
	if err != nil {
		return false, v, err
	}
	if v.Unavailable {
		return false, v, errors.New("no judge endpoint or chat endpoint answered")
	}
	return v.Answer == "yes", v, nil
}
