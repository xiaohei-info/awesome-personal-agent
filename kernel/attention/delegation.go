package attention

// 委派结果通知（§7.4 投递矩阵 delegation_result 行、§13.6）：由 APA 拉起的委派（委派记录带运行 id）收到"完成"或"卡住"回执时，
// 告诉发起这次委派的一方。Agent 的会话发起的走矩阵的 Agent 层（发起的是主 Agent 才唤醒主会话，否则按该行的 Else），
// 本人发起的走本人层，后台运行发起的不推（回执已作为进展写回它所在的父事项）。外部 harness 自己派出的（委派记录没有运行 id）
// 由 harness 自己通知，这里不重复推。一次回执一条：id 由子事项与回执事件序号决定，重启或重复追赶都不会再入队。

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// DelegationResultID 是一次委派回执的通知 id。
func DelegationResultID(child string, seq int64) string {
	return fmt.Sprintf("%s%s:%d", delivery.ResultPrefix, child, seq)
}

// 发起方的一层（row.from）。
const (
	fromUser  = "user"
	fromAgent = "agent"
)

// summaryRunes 是通知正文里回执摘要的上限；全文在事项页。
const summaryRunes = 500

// delegationResult 处理一条 delegation.reported 事件。
func (a *Attention) delegationResult(tx *store.Tx, pol policy.Policy, e domain.Event) error {
	var p struct {
		Report delegation.Report `json:"report"`
	}
	if json.Unmarshal(e.Payload, &p) != nil || p.Report.Outcome != delegation.OutcomeDone && p.Report.Outcome != delegation.OutcomeBlocked {
		return nil
	}
	ds, err := delegation.Dispatches(tx, e.Object)
	if err != nil {
		return err
	}
	started := false
	for _, d := range ds {
		started = started || d.Run != ""
	}
	if !started {
		return nil
	}
	from, _, err := originOf(tx, e.Object)
	if err != nil || from == "" {
		return err
	}
	id := DelegationResultID(e.Object, e.Seq)
	if known, err := a.known(tx, id); err != nil || known {
		return err
	}
	it, err := items.Get(tx, e.Object)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	title := "委派完成：" + it.Title
	if p.Report.Outcome == delegation.OutcomeBlocked {
		title = "委派卡住了：" + it.Title
	}
	summary := strings.TrimSpace(p.Report.Summary)
	if r := []rune(summary); len(r) > summaryRunes {
		summary = string(r[:summaryRunes]) + "…"
	}
	return a.offer(tx, pol, held{id: id, item: it.ID, class: domain.DeliveryReminder, priority: it.Priority, version: 1,
		content: domain.DeliveryContent{Title: title, Summary: summary, WebURL: a.o.WebURL(it.ID)}})
}

// originOf 认出委派子事项的发起方：建它的那条 delegation.created 的操作者。后台运行发起的返回空（不推）。
func originOf(tx *store.Tx, child string) (from, agent string, err error) {
	es, err := eventlog.Query(tx, eventlog.Filter{Object: child, Type: domain.EvDelegationCreated}, 0, 1)
	if err != nil || len(es) == 0 {
		return "", "", err
	}
	ev := es[0].Event
	switch {
	case ev.Actor.Run != "":
		return "", "", nil
	case ev.Actor.Type == domain.ActorHuman:
		return fromUser, "", nil
	}
	if ev.Basis != nil {
		agent = ev.Basis.Agent
	}
	return fromAgent, agent, nil
}

// delegationRow 是委派结果所在的行：只用发起方那一层，那一层不推时留 Web 未读。
func delegationRow(tx *store.Tx, child string) (row, error) {
	from, agent, err := originOf(tx, child)
	return row{keys: []string{policy.RowDelegationResult}, floor: true, from: from, fromAgent: agent}, err
}
