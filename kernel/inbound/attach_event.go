package inbound

import (
	"context"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdAttachEvent 把处于处理中、未判定或 digest 的来源事件挂到指定未关闭事项（§9.3、§9.4）。
// 命令实现登记在 kernel/followmsg（与 followmsg.attach 共用 AttachInTx 落账逻辑，不引入循环依赖）。
const CmdAttachEvent = "inbound.attach_event"

type attachEventIn struct {
	Event  string `json:"event"`
	Item   string `json:"item"`
	Reason string `json:"reason,omitempty"`
}

// AttachEvent 以 id 的身份把来源事件挂到未关闭事项 item，返回更新后的来源事件。
func (s *Service) AttachEvent(ctx context.Context, id command.Identity, event, item, reason string) (domain.SourceEvent, error) {
	if _, err := s.do(ctx, id, "", "", CmdAttachEvent, attachEventIn{Event: event, Item: item, Reason: reason}, command.Chain{}); err != nil {
		return domain.SourceEvent{}, err
	}
	var ev domain.SourceEvent
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, event); return })
	return ev, err
}
