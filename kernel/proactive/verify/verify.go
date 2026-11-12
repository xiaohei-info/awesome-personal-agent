// Package verify 是回执核实门控（设计 §7.5、§8.9）：回执之后由事件规则的 call_agent 动作经级联问一道是非题
// "这份回执需要本人核实吗"，要核实时在父事项上生成决策请求交本人。
//
//   - 只核实 done 回执；进展、阻塞回执不问。
//   - 模型只决定要不要打扰本人，不判断回执真假：低于下限（答"否"）不打扰；高于下限、不确定带内、各级未定（答"是"）
//     都交本人核实。门限与保守默认来自提示词模板（delegation-screen），本包不持有阈值。
//   - 不自动返工、不重新委派：决策请求的选项都由本人回答，本人事后自行决定是否重新委派。
//   - 两个端点都不可用时门控不运行：不写判断记录、不生成决策请求，返回"未核验"错误，由规则引擎记 health.alert
//     标在子事项上；回执本身照常被接受。
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

// 决策请求的选项 id。两个选项都回到 open，由本人自己决定后续。
const (
	OptAccept = "accept"
	OptRework = "rework"
)

// Judge 是级联判断入口（*direct.Service 满足它）。
type Judge interface {
	Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error)
}

// Options 配置处理器。
type Options struct {
	Store *store.Store
	Judge Judge
}

// ErrUnverified 表示两个端点都不可用，门控没有运行，回执未核验。
var ErrUnverified = errors.New("delegation receipt unverified: no judge endpoint or chat endpoint answered")

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the rule or the prompt template")
}

// Handler 返回 call_agent 动作的处理器：动作的 prompt 字段是初筛模板名，能力包取规则键的包名。
func Handler(o Options) (rules.Handler, error) {
	if o.Store == nil || o.Judge == nil {
		return nil, errors.New("verify: Store and Judge are required")
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
	// 回执自述是执行者写的，不是来源内容，不受外发范围裁剪；只送资料段，不附 Event 统计。
	v, err := o.Judge.Judge(ctx, direct.JudgeCall{Pack: pack, Template: tmpl, Input: in, MaterialText: s.report.Summary,
		Subject: s.child.ID, Project: s.child.Project, NoStats: true, UnjudgedOnUnavailable: true})
	if err != nil {
		return err
	}
	if v.Unavailable {
		return ErrUnverified
	}
	if v.Answer != "yes" {
		return nil
	}
	why := "判断为需要核实（" + v.DecidedBy + "）"
	if v.LowConfidence {
		why = "各级判断未定，按保守默认交本人核实"
	}
	_, err = c.Do("decision.request", map[string]any{"id": s.parent.ID, "reason": "delegation verify gate: judgment " + v.Record.ID, "ask": domain.AskInput{
		Question: fmt.Sprintf("委派「%s」已回执完成，需要你核实（%s）。是否采信该结果？", s.child.Title, why),
		Options: []domain.AskOption{
			{ID: OptAccept, Label: "采信该结果"},
			{ID: OptRework, Label: "不采信，由我决定如何重新委派"},
		},
		Recommend: OptRework,
	}})
	return err
}
