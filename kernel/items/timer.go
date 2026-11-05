package items

import (
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// HandleTimer 是时钟处理器里归事项中心的部分：wait_until 到期，等时间的事项回 open（§6.4）。
// 它在时钟交来的事务里直接改自己的投影、加版本并追加事件，而不经 command.Do：Do 自己开写事务，
// 嵌套会死锁；版本仍走 command.Bump，乐观版本只有一个来源。事件时间取计划到期时刻（Due）。
//
// 目标已被删除、已离开 waiting 或不再等时间时，Timer 已过期：返回 nil 让时钟清掉它。
// 其他类别的 Timer 不归本模块，返回错误（不吞掉）；由组装处按类别分派给对应模块。
func HandleTimer(tx *store.Tx, c clock.Command) error {
	if c.Kind != domain.TimerWaitUntil {
		return invalid("timer kind " + string(c.Kind) + " is not handled by the items module")
	}
	cur, err := Get(tx, c.Target)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Phase != domain.PhaseWaiting || cur.WaitingOn == nil || cur.WaitingOn.Kind != domain.WaitingTime {
		return nil
	}
	next := cur
	next.Phase, next.WaitingOn = domain.PhaseOpen, nil
	if err := save(tx, next); err != nil {
		return err
	}
	if _, err := command.Bump(tx, next.ID); err != nil {
		return err
	}
	depth, err := lastDepth(tx, next.ID)
	if err != nil {
		return err
	}
	b, err := marshalPayload(payload{Op: "timer", Before: &cur, Item: next})
	if err != nil {
		return err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: domain.EvItemPhaseChanged, Object: next.ID, Actor: c.Actor,
		Reason: "wait_until elapsed", Payload: b, ChainDepth: depth, At: c.Due})
	return err
}

// lastDepth 沿用事项最近一条事件的链路深度（定时不清零，§14.9）；被拒绝的尝试不带深度。
func lastDepth(tx *store.Tx, id string) (int, error) {
	es, err := eventlog.ReadObject(tx, id)
	if err != nil {
		return 0, err
	}
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Event.Type != domain.EvCommandRejected {
			return es[i].Event.ChainDepth, nil
		}
	}
	return 0, nil
}
