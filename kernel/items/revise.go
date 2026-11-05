package items

import (
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Revise 是其他模块（决策）改事项投影的唯一入口：投影只由本包写。它在调用方事务内读出事项、
// 交给 f 修改、校验阶段不变量（§6.14 第 3 条）、落投影并对齐 wait_until Timer，
// 返回变更前后的事项与可直接作事件 payload 的快照（折叠按它重建投影）。
// 事件与乐观版本由调用方负责：命令内由管线追加版本，时钟处理器里用 command.Bump。
func Revise(tx *store.Tx, id, op string, f func(*domain.Item) error) (before, after domain.Item, snapshot json.RawMessage, err error) {
	if before, err = Get(tx, id); err != nil {
		return
	}
	after = before
	if err = f(&after); err != nil {
		return
	}
	if after.Phase != before.Phase {
		if e := domain.CheckEntry(after, after.Phase, "revise", true); e != nil {
			err = e
			return
		}
	}
	if err = save(tx, after); err != nil {
		return
	}
	if err = syncTimer(tx, after); err != nil {
		return
	}
	snapshot, err = marshalPayload(payload{Op: op, Before: &before, Item: after})
	return
}

// ChainDepth 返回事项最近一条事件的链路深度，供命令管线之外写事件的模块沿用（§14.9）。
func ChainDepth(tx *store.Tx, id string) (int, error) { return lastDepth(tx, id) }
