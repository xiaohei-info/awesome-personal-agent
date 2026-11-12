// Package eventfeed 是事件订阅读模型（设计 §10.1、附录 B.4）：
// 按序号游标读取已登记类型的 Event 信封，只带类型、对象 id、标题、actor、时间。
// 正文、reason、证据不出现在信封里，需要时由调用方按权限另取。
// 内核不推送；至少一次由订阅方保证——处理成功后才用 Page.Next 推进自己的游标。
package eventfeed

import (
	"encoding/json"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Envelope 是对外事件信封。
type Envelope struct {
	Seq    int64        `json:"seq"`
	Type   string       `json:"type"`
	Object string       `json:"object"`
	Title  string       `json:"title,omitempty"`
	Actor  domain.Actor `json:"actor"`
	At     time.Time    `json:"at"`
}

// Page 是一次读取的结果。Next 是订阅方处理完本页后应持有的游标：
// 它越过被过滤掉的未登记类型事件，保证游标不会卡在不可见事件上；无新事件时等于入参 after。
type Page struct {
	Envelopes []Envelope `json:"events"`
	Next      int64      `json:"next"`
}

// titleOf 按事件类型从载荷里取对象标题（载荷格式由各模块定，规则只收在这里）：
// 事项、决策、委派类取 item.title；材料与规范类取 material.title；项目更新取 project.name。
// 其余类型没有标题概念（含 item.deadline_passed：载荷只有截止与阶段），返回空；墓碑化后载荷为 null，也为空。
func titleOf(typ string, payload json.RawMessage) string {
	var p struct {
		Item     struct{ Title string } `json:"item"`
		Material struct{ Title string } `json:"material"`
		Project  struct{ Name string }  `json:"project"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	switch typ {
	case domain.EvItemCreated, domain.EvItemUpdated, domain.EvItemPhaseChanged, domain.EvItemProgressAdded, domain.EvProposalAttached,
		domain.EvDecisionRequested, domain.EvDecisionResolved,
		domain.EvDelegationCreated, domain.EvDelegationReported:
		return p.Item.Title
	case domain.EvMaterialRevised, domain.EvMaterialRevisionProposed, domain.EvGuidelineActivated:
		return p.Material.Title
	case domain.EvProjectUpdated:
		return p.Project.Name
	}
	return ""
}

// Read 返回序号大于 after 的至多 limit 个已登记类型信封，按序号升序。
func Read(tx *store.Tx, after int64, limit int) (Page, error) {
	page := Page{Next: after}
	for limit > 0 && len(page.Envelopes) < limit {
		batch, err := eventlog.Read(tx, page.Next, limit)
		if err != nil {
			return Page{}, err
		}
		if len(batch) == 0 {
			break
		}
		for _, en := range batch {
			e := en.Event
			page.Next = e.Seq
			if !domain.IsRegisteredEventType(e.Type) {
				continue
			}
			page.Envelopes = append(page.Envelopes, Envelope{
				Seq: e.Seq, Type: e.Type, Object: e.Object, Title: titleOf(e.Type, e.Payload), Actor: e.Actor, At: e.At,
			})
			if len(page.Envelopes) == limit {
				return page, nil
			}
		}
	}
	return page, nil
}
