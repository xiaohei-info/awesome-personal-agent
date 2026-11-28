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

// AgentReady 报告日常执行者能否后台运行：分工里选了日常执行者，它由已确认的无头运行声明登记、在 APA 主机 host 上，
// 声明提供权限卡上的那一档（§13.6）。
func AgentReady(tx *store.Tx, host string) (bool, error) {
	t, err := ReadyFor(tx, "", "", host)
	return t.Reason == "", err
}

// ReadyFor 按任务型拉起时同一口径（规则 rule、安排参数里的执行者 schedule）选出执行者并判定能否拉起。
func ReadyFor(tx *store.Tx, rule, schedule, host string) (executors.Target, error) {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return executors.Target{}, err
	}
	id, source := executors.Pick(pol, executors.Selection{Rule: rule, Schedule: schedule, Runner: executors.RunnerTask})
	return executors.Resolve(tx, pol, executors.Want{Executor: id, Source: source, Runner: executors.RunnerTask, Host: host})
}

// Gate 是规则引擎在执行实例驱动的规则之前的检查（rules.Options.Gate）：周期简报（create_item 建 brief 事项、
// call_agent 指向任务型模板）在 Agent 还不能后台运行时整次跳过，不建事项、不打扰本人，返回跳过原因；
// 例行任务等其他实例照常执行，到点没有 Agent 时转 needs_input 问本人（§13.5）。
func (s *Service) Gate(ctx context.Context, rule string, do []packs.Action, params json.RawMessage) (string, error) {
	brief, prompt := false, ""
	for _, a := range do {
		switch a["action"] {
		case "create_item":
			brief = brief || a["kind"] == string(domain.KindBrief)
		case "call_agent":
			prompt, _ = a["prompt"].(string)
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
	var t executors.Target
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		t, err = ReadyFor(tx, rule, p.Executor, s.o.Host)
		return err
	})
	if err != nil || t.Reason == "" {
		return "", err
	}
	if t.Entry.ID != "" {
		return SkipAgentNotReady + "：" + t.Reason, nil
	}
	return SkipAgentNotReady, nil
}
