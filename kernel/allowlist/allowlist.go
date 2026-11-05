// Package allowlist 是转发白名单的判定（设计 §14.6）：谁读挂靠消息时要过滤，以及事项挂靠消息的读取视图。
// 名单本身在策略里，只在 Web 设置；读取时按当前版本判定，所以变更立即生效。
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

// Filtered：运行身份一律过滤；unattended 事项除本人（Web）和在场主会话（delegate）外都过滤，否则运行者一委派白名单就被绕过。
func Filtered(level authz.Level, it domain.Item) bool {
	return level == authz.LevelRun || (it.Unattended && level != authz.LevelWeb && level != authz.LevelDelegate)
}

// Allowed 报告来源事件的发送者是否进白名单。
func Allowed(pol policy.Policy, ev domain.SourceEvent) bool {
	return authz.ForwardAllowed(pol, ev.SourceID, ev.Sender, ev.SenderVerified)
}

// Keep 返回 level 读 it 时的过滤函数；不过滤时为 nil（followmsg.Forward 的约定）。
func Keep(pol policy.Policy, level authz.Level, it domain.Item) func(domain.SourceEvent) bool {
	if !Filtered(level, it) {
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

// Messages 返回 level 能看到的、事项的全部挂靠消息；只读，不推进 attach_state。
// web_only 初筛中的事项对非 Web 凭据返回空列表（§14.4）。
func Messages(tx *store.Tx, item string, level authz.Level) ([]Attached, error) {
	it, err := items.Get(tx, item)
	if err != nil {
		return nil, err
	}
	out := []Attached{}
	if level != authz.LevelWeb && it.Ask != nil && it.Ask.ScreeningPending {
		return out, nil
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return nil, err
	}
	as, err := runner.Attachments(tx, item)
	if err != nil {
		return nil, err
	}
	keep := Keep(pol, level, it)
	for _, a := range as {
		if keep == nil || keep(a.SourceEvent) {
			out = append(out, Attached{Message: followmsg.MessageOf(a.SourceEvent), AttachState: a.AttachState, ForwardAllowed: Allowed(pol, a.SourceEvent)})
		}
	}
	return out, nil
}
