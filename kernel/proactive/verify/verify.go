// Package verify 是委派结果核验（设计 §7.5）：回执之后由事件规则的 call_agent 动作调判断型初筛，
// 存疑时在父事项上生成决策请求交本人。
//
//   - 只核验 done 回执；进展、阻塞回执不核验。
//   - 初筛提示词必须是 yes_no 判断型：probability 是"结果可信"的概率。置信度不足、需要解释、
//     需要升级到任务型，或可信概率低于阈值，都算存疑；可信则不写任何业务状态。
//   - 不自动返工、不重新委派：存疑只产生决策请求，选项都由本人回答，本人事后自行决定是否重新委派。
//   - 模型未配置或持续不可用时返回真实错误（规则引擎记 health.alert），不当作"可信"放行。
//   - 默认 observe 由规则引擎保证（包文件不能声明 auto）；本包只提供动作处理器，不起子进程、不调网络，
//     不直写其他模块的数据，写入一律经规则给的命令模型入口。
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// DefaultMinCredibility 是可信概率的默认下限：低于它算存疑。
const DefaultMinCredibility = 0.7

// 决策请求的选项 id。两个选项都回到 open，由本人自己决定后续。
const (
	OptAccept = "accept"
	OptRework = "rework"
)

// Judge 是专属 agent 的判断型入口（*direct.Service 满足它）。
type Judge interface {
	Do(ctx context.Context, c direct.Call) (direct.Outcome, error)
}

// Options 配置处理器。
type Options struct {
	Store *store.Store
	Judge Judge
	// MinCredibility 是可信概率下限，取值 (0,1]；零值取 DefaultMinCredibility。
	MinCredibility float64
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the rule or the prompt template")
}

// Handler 返回 call_agent 动作的处理器：动作的 prompt 字段是初筛模板名，能力包取规则键的包名。
func Handler(o Options) (rules.Handler, error) {
	if o.Store == nil || o.Judge == nil {
		return nil, errors.New("verify: Store and Judge are required")
	}
	if o.MinCredibility == 0 {
		o.MinCredibility = DefaultMinCredibility
	}
	if o.MinCredibility < 0 || o.MinCredibility > 1 {
		return nil, errors.New("verify: MinCredibility must be within (0,1]")
	}
	return func(ctx context.Context, c rules.Call) error { return o.run(ctx, c) }, nil
}

// subject 是被核验的回执及其上下文，取自账本。
type subject struct {
	child, parent domain.Item
	report        delegation.Entry
	policyVersion int
}

func (o Options) load(ctx context.Context, c rules.Call) (subject, bool, error) {
	var s subject
	if c.Item == nil {
		return s, false, invalid("delegation verify needs a triggering item")
	}
	err := o.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		if s.policyVersion, _, err = policy.Current(tx); err != nil {
			return err
		}
		if s.child, err = items.Get(tx, c.Item.ID); err != nil {
			return err
		}
		pid, has, err := items.ParentOf(tx, s.child.ID)
		if err != nil {
			return err
		}
		if s.child.Kind != domain.KindDelegation || !has {
			return invalid("item " + s.child.ID + " is not a delegated child item")
		}
		if s.parent, err = items.Get(tx, pid); err != nil {
			return err
		}
		es, err := delegation.Reports(tx, s.child.ID)
		if err != nil {
			return err
		}
		for i := len(es) - 1; i >= 0; i-- { // 优先取触发本次规则的那条回执
			if c.Seq == 0 || es[i].Seq == c.Seq {
				s.report = es[i]
				return nil
			}
		}
		return invalid(fmt.Sprintf("no delegation report at event %d on %s", c.Seq, s.child.ID))
	})
	if err != nil {
		return s, false, err
	}
	return s, s.report.Outcome == delegation.OutcomeDone, nil
}

func (o Options) run(ctx context.Context, c rules.Call) error {
	tmpl, _ := c.Action["prompt"].(string)
	if tmpl == "" {
		return invalid("call_agent needs a prompt template name")
	}
	s, verifiable, err := o.load(ctx, c)
	if err != nil || !verifiable {
		return err
	}
	in, _ := json.Marshal(map[string]any{"item": s.child.ID, "parent": s.parent.ID, "outcome": s.report.Outcome, "evidence": s.report.Evidence})
	pack, _, _ := strings.Cut(c.Rule, "/")
	out, err := o.Judge.Do(ctx, direct.Call{Pack: pack, Template: tmpl, Input: in, Material: s.report.Summary,
		Target: s.child.ID, Project: s.child.Project,
		Actor: domain.Actor{Type: domain.ActorAgent, Principal: "rule:" + c.Rule, Via: domain.ViaRule},
		Basis: &domain.Basis{PolicyVersion: s.policyVersion, RuleID: c.Rule}})
	if err != nil {
		return err
	}
	why, err := o.doubt(out)
	if err != nil || why == "" {
		return err
	}
	_, err = c.Do("decision.request", map[string]any{"id": s.parent.ID, "reason": "delegation verify: " + why, "ask": domain.AskInput{
		Question: fmt.Sprintf("委派「%s」已回执完成，核验存疑（%s）。是否采信该结果？", s.child.Title, why),
		Options: []domain.AskOption{
			{ID: OptAccept, Label: "采信该结果"},
			{ID: OptRework, Label: "不采信，由我决定如何重新委派"},
		},
		Recommend: OptRework,
	}})
	return err
}

// doubt 返回存疑原因；空串表示可信。
func (o Options) doubt(out direct.Outcome) (string, error) {
	if out.HandoffTask != "" {
		return "需要任务型核验 " + out.HandoffTask, nil
	}
	if !out.Settled {
		return "初筛未定：" + out.Reason, nil
	}
	var j direct.Judgement
	if err := json.Unmarshal(out.Final().Result, &j); err != nil || j.Probability == nil {
		return "", invalid("delegation verify prompt " + out.Final().Template + " must be a yes_no judge")
	}
	if *j.Probability < o.MinCredibility {
		return fmt.Sprintf("可信概率 %.2f 低于 %.2f", *j.Probability, o.MinCredibility), nil
	}
	return "", nil
}
