// Package trust 判定内容来源是否可信（设计 §14.5）：建议是否来自可信发件人（来源可信），后台运行是否由可信内容触发。
// 两者都按转发白名单（§14.6）与来源事件的 sender_verified 判定，读账本、不写数据；与 allowlist 分开是为了不依赖跟进消息模块。
package trust

import (
	"encoding/json"
	"errors"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// sourceEventOf 返回建立事项的来源事件 id（ext.source_event，入站建事项时写下）；没有时为空。
func sourceEventOf(it domain.Item) string {
	var x struct {
		SourceEvent string `json:"source_event"`
	}
	if len(it.Ext) > 0 && json.Unmarshal(it.Ext, &x) == nil {
		return x.SourceEvent
	}
	return ""
}

// eventAllowed 报告来源事件 id 是否满足转发白名单；事件已不在（被物理删除）按不满足处理。
func eventAllowed(tx *store.Tx, pol policy.Policy, id string) (bool, error) {
	ev, err := runner.Event(tx, id)
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.CodeNotFound {
			return false, nil
		}
		return false, err
	}
	return authz.ForwardAllowed(pol, ev.SourceID, ev.Sender, ev.SenderVerified), nil
}

// OriginTrusted 报告事项（建议）是否来自一条满足转发白名单的来源事件（§14.5 来源可信）。
// agent 自己提出的、对话捕获产生的事项没有来源事件，恒为假。
func OriginTrusted(tx *store.Tx, item string) (bool, error) {
	it, err := items.Get(tx, item)
	if err != nil {
		return false, err
	}
	id := sourceEventOf(it)
	if id == "" {
		return false, nil
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return false, err
	}
	return eventAllowed(tx, pol, id)
}

// TriggerTrusted 报告触发后台运行的内容是否可信（§14.5 后台运行使用授权单）：从目标事项沿父事项往上，
// 第一个由来源事件建立的事项决定结果（满足转发白名单才可信；对话捕获的内部来源不在任何名单里，恒不可信）；途中遇到 agent 建的事项即不可信；
// 一路都是本人、时钟、规则或实例建的，可信。判定时按当前白名单从账本算，所以白名单变更立即生效。
func TriggerTrusted(tx *store.Tx, target string) (bool, error) {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return false, err
	}
	seen := map[string]bool{}
	for id := target; id != "" && !seen[id]; {
		seen[id] = true
		it, err := items.Get(tx, id)
		if err != nil {
			return false, err
		}
		if ev := sourceEventOf(it); ev != "" {
			return eventAllowed(tx, pol, ev)
		}
		if byAgent, err := createdByAgent(tx, id); err != nil || byAgent {
			return false, err
		}
		parent, ok, err := items.ParentOf(tx, id)
		if err != nil {
			return false, err
		}
		if !ok {
			break
		}
		id = parent
	}
	return true, nil
}

// createdByAgent：事项的第一条事件（建立它的那条）由 agent 写下。
func createdByAgent(tx *store.Tx, id string) (bool, error) {
	es, err := eventlog.ReadObject(tx, id)
	if err != nil || len(es) == 0 {
		return false, err
	}
	return es[0].Event.Actor.Type == domain.ActorAgent, nil
}
