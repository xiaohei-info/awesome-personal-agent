package kcontext

import (
	"encoding/json"
	"sort"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 委派历史（设计 §6.17）：同一件事之前是怎么派的，只按账本里已有的显式关联认定，不判断"像不像"：
//
//	item       记录挂在目标事项本身（同一子事项的再次派出、续接；经关联键挂回的跟进消息落在原子事项上）
//	parent     目标是委派子事项时，同一父事项下的其他委派子事项；目标是父事项时，它的委派子事项
//	continues  经 continues 接上或被接续的记录（最多追 5 跳）
//
// 句柄是给宿主的资料：own 只说明记录是不是本次调用方报告的 harness 写的，不是边界（harness 名是自报的）。

const (
	historyMax  = 5
	continueMax = 5
)

// HistoryExecutor 是委派记录里执行者在名录里的登记；不在名录里时只有 id。
type HistoryExecutor struct {
	ID      string          `json:"id"`
	Harness string          `json:"harness,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Host    string          `json:"host,omitempty"`
	Label   string          `json:"label,omitempty"`
	Handle  json.RawMessage `json:"handle,omitempty"`
}

// HistoryEntry 是一条委派记录，带命中原因与执行者登记。
type HistoryEntry struct {
	delegation.Dispatch
	Executor HistoryExecutor `json:"executor"`
	Via      string          `json:"via"`
	Own      bool            `json:"own"`
}

// ItemHistory 是一个目标事项所在序列最近的委派记录，按开始时间倒序。
type ItemHistory struct {
	Item       string         `json:"item"`
	Dispatches []HistoryEntry `json:"dispatches"`
}

// sequenceItems 返回与 it 同序列的事项及命中原因（不含经 continues 的）。
func sequenceItems(tx *store.Tx, it domain.Item) (map[string]string, error) {
	via := map[string]string{it.ID: "item"}
	add := func(list []domain.Item, why string) {
		for _, c := range list {
			if c.Kind == domain.KindDelegation {
				if _, ok := via[c.ID]; !ok {
					via[c.ID] = why
				}
			}
		}
	}
	kids, err := items.Children(tx, it.ID)
	if err != nil {
		return nil, err
	}
	add(kids, "parent")
	if parent, ok, err := items.ParentOf(tx, it.ID); err != nil {
		return nil, err
	} else if ok {
		sibs, err := items.Children(tx, parent)
		if err != nil {
			return nil, err
		}
		add(sibs, "parent")
	}
	return via, nil
}

// History 返回 it 所在序列最近的委派记录（至多 historyMax 条）。harness 是本次调用方报告的 harness。
func History(tx *store.Tx, it domain.Item, harness string) ([]HistoryEntry, error) {
	via, err := sequenceItems(tx, it)
	if err != nil {
		return nil, err
	}
	got := map[string]HistoryEntry{}
	for id, why := range via {
		ds, err := delegation.Dispatches(tx, id)
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			got[d.ID] = HistoryEntry{Dispatch: d, Via: why}
		}
	}
	// 经 continues 两个方向各追几跳：接上的旧记录，以及接续它们的新记录。
	frontier := make([]delegation.Dispatch, 0, len(got))
	for _, h := range got {
		frontier = append(frontier, h.Dispatch)
	}
	for hop := 0; hop < continueMax && len(frontier) > 0; hop++ {
		var next []delegation.Dispatch
		for _, d := range frontier {
			var linked []delegation.Dispatch
			if d.Continues != "" {
				prev, err := delegation.GetDispatch(tx, d.Continues)
				if err == nil {
					linked = append(linked, prev)
				} else if !isNotFound(err) {
					return nil, err
				}
			}
			after, err := delegation.Continuing(tx, d.ID)
			if err != nil {
				return nil, err
			}
			for _, l := range append(linked, after...) {
				if _, seen := got[l.ID]; !seen {
					got[l.ID] = HistoryEntry{Dispatch: l, Via: "continues"}
					next = append(next, l)
				}
			}
		}
		frontier = next
	}
	out := make([]HistoryEntry, 0, len(got))
	for _, h := range got {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > historyMax {
		out = out[:historyMax]
	}
	for i := range out {
		out[i].Own = harness != "" && out[i].Harness == harness
		out[i].Executor = HistoryExecutor{ID: out[i].Dispatch.Executor}
		e, ok, err := executors.Get(tx, out[i].Dispatch.Executor)
		if err != nil {
			return nil, err
		}
		if ok {
			out[i].Executor = HistoryExecutor{ID: e.ID, Harness: e.Harness, Kind: e.Kind, Host: e.Host, Label: e.Label, Handle: e.Handle}
		}
	}
	return out, nil
}
