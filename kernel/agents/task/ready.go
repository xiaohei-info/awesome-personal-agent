package task

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// SkipAgentNotReady 是周期简报因 Agent 还不能后台运行而跳过时记下的原因（§7.3）。
const SkipAgentNotReady = "跳过：Agent 还不能后台运行"

// AgentReady 报告 Agent 能否后台运行：Web 设置了默认 Agent，且它的无头运行声明已确认（§13.6）。
// 档位与上限不在这里判：那是配置冲突，到点照常转 needs_input 交本人（§8.7）。
func AgentReady(tx *store.Tx) (bool, error) {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return false, err
	}
	selected, err := executors.Resolve(tx, pol, executors.Selection{})
	return selected.Reason == "", err
}

// Gate 是规则引擎在执行实例驱动的规则之前的检查（rules.Options.Gate）：周期简报（create_item 建 brief 事项、
// call_agent 指向任务型模板）在 Agent 还不能后台运行时整次跳过，不建事项、不打扰本人，返回跳过原因；
// 例行任务等其他实例照常执行，到点没有 Agent 时转 needs_input 问本人（§13.5）。
func (s *Service) Gate(ctx context.Context, rule string, do []packs.Action, params json.RawMessage) (string, error) {
	brief, prompt, harness := false, "", ""
	for _, a := range do {
		switch a["action"] {
		case "create_item":
			brief = brief || a["kind"] == string(domain.KindBrief)
		case "call_agent":
			prompt, _ = a["prompt"].(string)
			harness, _ = a["harness"].(string)
		}
	}
	if !brief || prompt == "" {
		return "", nil
	}
	pack, _, _ := strings.Cut(rule, "/")
	pk, ok := s.o.Packs.Get(pack)
	if !ok || pk.Prompts[prompt].Meta.Agent != "task" {
		return "", nil
	}
	var p instParams
	if json.Unmarshal(params, &p) == nil && p.Mode == modeCreateItem {
		return "", nil
	}
	var selected executors.Selected
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		selected, err = executors.Resolve(tx, pol, executors.Selection{Rule: rule, Schedule: p.AgentID, Harness: harness})
		return err
	})
	if err != nil || selected.Reason == "" {
		return "", err
	}
	if selected.Snapshot.AgentID != "" {
		return SkipAgentNotReady + "：" + selected.Reason, nil
	}
	return SkipAgentNotReady, nil
}
