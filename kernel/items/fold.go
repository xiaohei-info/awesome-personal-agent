package items

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// payload 是事项事件的 payload：变更后的完整快照（Version 恒为 0，由折叠按事件计数补上），
// 关系变更另带关系本身。Before 供撤销取反向状态。
type payload struct {
	Op       string           `json:"op"` // create | update | transition | relate | revert | timer
	Before   *domain.Item     `json:"before,omitempty"`
	Item     domain.Item      `json:"item"`
	Relation *domain.Relation `json:"relation,omitempty"`
	Progress *domain.Progress `json:"progress,omitempty"` // item.progress 追加的那一条
	Key      *Correlation     `json:"key,omitempty"`      // 关联键登记或移除（§9.4）
	Remove   bool             `json:"remove,omitempty"`   // 关系或关联键变更：为真是删
	Reverts  int64            `json:"reverts,omitempty"`
	To       string           `json:"to,omitempty"`       // proposal.attach：并入的目标事项
	Proposal string           `json:"proposal,omitempty"` // proposal.attached：被并入的建议
	Messages []string         `json:"messages,omitempty"` // proposal.attach：改挂到目标的来源事件
}

var itemEvents = []string{domain.EvItemCreated, domain.EvItemUpdated, domain.EvItemPhaseChanged, domain.EvDecisionRequested, domain.EvDecisionResolved,
	domain.EvDelegationCreated, domain.EvDelegationReported, domain.EvItemProgressAdded, domain.EvProposalAttached}

type folded struct {
	items map[string]domain.Item
	rels  map[domain.Relation]bool
	prog  map[string][]domain.Progress
	keys  map[string]map[Correlation]bool
}

// fold 按序号读完整个事件日志，折叠出事项与关系。已墓碑化的对象视为物理删除。
// 每条事项事件对应一次命令管线的版本加一，所以版本就是该对象事项事件的条数。
func fold(tx *store.Tx) (folded, error) {
	f := folded{items: map[string]domain.Item{}, rels: map[domain.Relation]bool{}, prog: map[string][]domain.Progress{}, keys: map[string]map[Correlation]bool{}}
	var after int64
	for {
		es, err := eventlog.Read(tx, after, 500)
		if err != nil {
			return f, err
		}
		if len(es) == 0 {
			return f, nil
		}
		for _, en := range es {
			after = en.Event.Seq
			if en.Tombstone {
				delete(f.items, en.Event.Object)
				delete(f.prog, en.Event.Object)
				delete(f.keys, en.Event.Object)
				for r := range f.rels {
					if r.From == en.Event.Object || r.To == en.Event.Object {
						delete(f.rels, r)
					}
				}
				continue
			}
			if !slices.Contains(itemEvents, en.Event.Type) {
				continue
			}
			var p payload
			if err := json.Unmarshal(en.Event.Payload, &p); err != nil || p.Item.ID != en.Event.Object {
				continue // 不是事项中心写的 payload
			}
			prev := f.items[p.Item.ID]
			p.Item.CreatedAt = prev.CreatedAt
			it := stamped(p.Item, en.Event.At)
			it.Version = prev.Version + 1
			f.items[it.ID] = it
			if p.Progress != nil {
				f.prog[it.ID] = append(f.prog[it.ID], *p.Progress)
			}
			if p.Key != nil {
				if f.keys[it.ID] == nil {
					f.keys[it.ID] = map[Correlation]bool{}
				}
				if p.Remove {
					delete(f.keys[it.ID], *p.Key)
				} else {
					f.keys[it.ID][*p.Key] = true
				}
			}
			if p.Relation != nil {
				if p.Remove {
					delete(f.rels, *p.Relation)
				} else {
					f.rels[*p.Relation] = true
				}
			}
		}
	}
}

// Verify 比较投影与事件折叠的结果，不一致返回描述第一处差异的错误。
func Verify(tx *store.Tx) error {
	want, err := fold(tx)
	if err != nil {
		return err
	}
	have, err := List(tx, Filter{})
	if err != nil {
		return err
	}
	if len(have) != len(want.items) {
		return fmt.Errorf("items: projection has %d items, events fold to %d", len(have), len(want.items))
	}
	for _, it := range have {
		w, ok := want.items[it.ID]
		if !ok {
			return fmt.Errorf("items: item %s is not in the event log", it.ID)
		}
		if !reflect.DeepEqual(canon(it), canon(w)) {
			return fmt.Errorf("items: item %s differs from its event fold", it.ID)
		}
	}
	for _, it := range have {
		got, err := Progress(tx, it.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(canon(got), canon(want.prog[it.ID])) && len(got)+len(want.prog[it.ID]) > 0 {
			return fmt.Errorf("items: progress of %s differs from its event fold", it.ID)
		}
	}
	rels, err := allRelations(tx, "")
	if err != nil {
		return err
	}
	if len(rels) != len(want.rels) {
		return fmt.Errorf("items: projection has %d relations, events fold to %d", len(rels), len(want.rels))
	}
	for _, r := range rels {
		if !want.rels[r] {
			return fmt.Errorf("items: relation %s %s→%s is not in the event log", r.Type, r.From, r.To)
		}
	}
	for _, it := range have {
		got, err := Correlations(tx, it.ID)
		if err != nil {
			return err
		}
		if len(got) != len(want.keys[it.ID]) {
			return fmt.Errorf("items: correlation keys of %s differ from its event fold", it.ID)
		}
		for _, k := range got {
			if !want.keys[it.ID][k] {
				return fmt.Errorf("items: correlation key %s of %s is not in the event log", k.Key, it.ID)
			}
		}
	}
	return nil
}

// canon 经 JSON 往返抹平 nil 与空切片等无语义差异。
func canon(it any) any {
	b, _ := json.Marshal(it)
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

// Rebuild 丢弃事项与关系投影，按事件折叠结果重建（一致性修复）。版本在命令管线里，不受影响；
// Timer 在时钟里，也不重建。
func Rebuild(tx *store.Tx) error {
	f, err := fold(tx)
	if err != nil {
		return err
	}
	for _, t := range []string{"items", "item_relations"} {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	for _, it := range f.items {
		if err := save(tx, it); err != nil {
			return err
		}
	}
	for r := range f.rels {
		if err := setRelation(tx, r, false); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM item_correlation"); err != nil {
		return err
	}
	for id, ks := range f.keys {
		for k := range ks {
			if err := setCorrelation(tx, id, k, false); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec("DELETE FROM item_progress"); err != nil {
		return err
	}
	for id, ps := range f.prog {
		for _, p := range ps {
			if err := insertProgress(tx, id, p); err != nil {
				return err
			}
		}
	}
	return nil
}
