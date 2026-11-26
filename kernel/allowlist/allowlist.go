// Package allowlist 是转发白名单的判定（设计 §14.6）：谁读挂靠消息时要过滤，以及事项挂靠消息的读取视图。
// 名单与"后台运行看哪些消息"都在策略里；读取时按当前版本判定，所以变更立即生效。
package allowlist

import (
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Filtered：后台运行，以及 unattended 事项上本人（Web）和在场主会话（delegate）之外的读取者，按所属 Agent 权限卡上的
// "后台运行看哪些消息"过滤（缺省只看信任发件人的，§14.6）；否则运行者一委派白名单就被绕过。
func Filtered(pol policy.Policy, level authz.Level, agent string, it domain.Item) bool {
	unattended := level == authz.LevelRun || (it.Unattended && level != authz.LevelWeb && level != authz.LevelDelegate)
	return unattended && !pol.Card(agent).RunsAllMessages
}

// Allowed 报告来源事件的发送者是否进白名单。
func Allowed(pol policy.Policy, ev domain.SourceEvent) bool {
	return authz.ForwardAllowed(pol, ev.SourceID, ev.Sender, ev.SenderVerified)
}

// Keep 返回 level（所属 Agent 为 agent）读 it 时的过滤函数；不过滤时为 nil（followmsg.Forward 的约定）。
func Keep(pol policy.Policy, level authz.Level, agent string, it domain.Item) func(domain.SourceEvent) bool {
	if !Filtered(pol, level, agent, it) {
		return nil
	}
	return func(ev domain.SourceEvent) bool { return Allowed(pol, ev) }
}

// Attached 是事项页看到的一条挂靠消息。
type Attached struct {
	followmsg.Message
	AttachState    string `json:"attach_state"`
	ForwardAllowed bool   `json:"forward_allowed"`
}

// Withheld 是 keep 滤掉的、还没确认的挂靠消息条数：运行上下文据它写明"有 N 条没给你看"（§14.6）。
func Withheld(tx *store.Tx, item string, keep func(domain.SourceEvent) bool) (int, error) {
	if keep == nil {
		return 0, nil
	}
	as, err := runner.Attachments(tx, item)
	n := 0
	for _, a := range as {
		if a.AttachState == runner.AttachPending && !keep(a.SourceEvent) {
			n++
		}
	}
	return n, err
}

// Messages 返回 level（所属 Agent 为 agent）能看到的、事项的全部挂靠消息；只读，不推进 attach_state。
func Messages(tx *store.Tx, item string, level authz.Level, agent string) ([]Attached, error) {
	it, err := items.Get(tx, item)
	if err != nil {
		return nil, err
	}
	out := []Attached{}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return nil, err
	}
	as, err := runner.Attachments(tx, item)
	if err != nil {
		return nil, err
	}
	keep := Keep(pol, level, agent, it)
	for _, a := range as {
		if keep == nil || keep(a.SourceEvent) {
			out = append(out, Attached{Message: followmsg.MessageOf(a.SourceEvent), AttachState: a.AttachState, ForwardAllowed: Allowed(pol, a.SourceEvent)})
		}
	}
	return out, nil
}
