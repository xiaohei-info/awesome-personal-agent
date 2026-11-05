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

// Create 是其他模块（任务委派）新建事项的入口：分配 id、校验阶段进入约束与事项自身、落投影，
// parent 非空时同时建 parent 关系（子 → 父）。返回的快照可直接作事件 payload（折叠按它重建投影，含关系）。
// 事件与乐观版本由调用方负责：对新对象用 command.Bump。
func Create(tx *store.Tx, it domain.Item, parent string) (domain.Item, json.RawMessage, error) {
	id, err := newID()
	if err != nil {
		return it, nil, err
	}
	it.ID = id
	if e := domain.CheckEntry(it, it.Phase, "created", true); e != nil {
		return it, nil, e
	}
	if err := (&svc{}).check(tx, it); err != nil {
		return it, nil, err
	}
	var rel *domain.Relation
	if parent != "" {
		if _, err := Get(tx, parent); err != nil {
			return it, nil, err
		}
		rel = &domain.Relation{Type: domain.RelParent, From: id, To: parent}
	}
	if err := save(tx, it); err != nil {
		return it, nil, err
	}
	if rel != nil {
		if err := setRelation(tx, *rel, false); err != nil {
			return it, nil, err
		}
	}
	snap, err := marshalPayload(payload{Op: "create", Item: it, Relation: rel})
	return it, snap, err
}

// Children 返回以 id 为父的事项（parent 关系的起点），按 id 排序。
func Children(tx *store.Tx, id string) ([]domain.Item, error) {
	rs, err := allRelations(tx, "WHERE type = ? AND to_id = ? ", domain.RelParent, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Item, 0, len(rs))
	for _, r := range rs {
		it, err := Get(tx, r.From)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// ParentOf 返回 id 的父事项 id；没有 parent 关系时 ok 为假。
func ParentOf(tx *store.Tx, id string) (parent string, ok bool, err error) {
	rs, err := allRelations(tx, "WHERE type = ? AND from_id = ? ", domain.RelParent, id)
	if err != nil || len(rs) == 0 {
		return "", false, err
	}
	return rs[0].To, true, nil
}
