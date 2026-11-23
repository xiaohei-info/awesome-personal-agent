package inbound

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdRepair 补齐一条事项的消息清单（`apa repair-merges`，2026-11-23 一次性修复，§9.3）：
//   - 归类折叠掉的（合并成一条、并入已有事项）：名下还留着的消息搬到去处，同折叠时一样记合并留痕（item 记它），撤销时一样搬回；
//   - 仍是建议的：挂在它名下、没记进 sources 的消息（旧版的来源对象更新）补记进它自己的 sources。
//
// 可重复执行：搬过、补过的不再满足条件，第二遍什么都不写。
const CmdRepair = "inbound.repair_held"

type repairIn struct {
	Item string `json:"item"`
}

// Repaired 是修复的一条：Item 名下的 Events 搬到了 Into（仍是建议的补记自己时 Into 等于 Item）。
type Repaired struct {
	Item   string   `json:"item"`
	Title  string   `json:"title"`
	Into   string   `json:"into"`
	Events []string `json:"events"`
}

// repairOf 算一条事项要修什么；不用修时 Events 为空。
func repairOf(tx *store.Tx, it domain.Item) (Repaired, error) {
	r := Repaired{Item: it.ID, Title: it.Title}
	p := it.Proposal
	switch {
	case p == nil:
		return r, nil
	case it.Phase == domain.PhaseDropped && p.Sweep.Folded() && (p.Sweep.Action == domain.SweepMerge || p.Sweep.Action == domain.SweepAttach):
		if _, err := items.Get(tx, p.Sweep.Target); err != nil {
			if de := (*domain.Error)(nil); errors.As(err, &de) && de.Code == domain.CodeNotFound {
				return r, nil // 去处已清除：留在原处
			}
			return r, err
		}
		r.Into = p.Sweep.Target
	case it.Phase == domain.PhaseProposed:
		r.Into = it.ID
	default:
		return r, nil
	}
	held, err := runner.Held(tx, it.ID, nil)
	for _, ev := range held {
		if r.Into != it.ID || !slices.Contains(p.Sources, ev.ID) {
			r.Events = append(r.Events, ev.ID)
		}
	}
	return r, err
}

func registerRepair(x *command.Executor) error {
	parse := func(in json.RawMessage) (repairIn, error) {
		var v repairIn
		if err := json.Unmarshal(in, &v); err != nil || v.Item == "" {
			return v, invalid("repair needs item")
		}
		return v, nil
	}
	return x.Register(command.Spec{Name: CmdRepair, Action: authz.ActionUpdateOwn, Tier: authz.TierInternal, KeepVersion: true,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			return authz.Object{ID: v.Item, Assignee: systemPrinc}, err // 内核整理自己建的建议，同 inbound.sweep
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := parse(c.Input)
			if err != nil {
				return nil, err
			}
			it, err := items.Get(c.Tx, v.Item)
			if err != nil {
				return nil, err
			}
			r, err := repairOf(c.Tx, it)
			if err != nil || len(r.Events) == 0 {
				return r, err
			}
			if r.Into != it.ID {
				score := 0.0 // 同一次折叠的分数，取去处上已有的留痕
				if t, err := items.Get(c.Tx, r.Into); err == nil && t.Proposal != nil {
					for _, m := range t.Proposal.Merges {
						if m.Item == it.ID {
							score = m.Score
							break
						}
					}
				}
				_, err := moveHeld(c, it, r.Into, nil, *it.Proposal.Sweep, score, "repair: moved messages left on "+it.ID)
				return r, err
			}
			p := *it.Proposal
			p.Sources = append(slices.Clone(p.Sources), r.Events...)
			w, err := items.SetProposal(c.Tx, it.ID, p, c.Now())
			if err != nil {
				return nil, err
			}
			return r, emitWrite(c, w, "repair: recorded attached messages in sources")
		}})
}

// RegisterRepair 只登记修复命令，给离线的 `apa repair-merges` 用（不起入站流水线、不调模型）。
func RegisterRepair(x *command.Executor) error { return registerRepair(x) }

// Repair 逐条补齐（见 CmdRepair），返回动过的。折叠掉的按折叠先后修，折进的去处后来又被折叠时，消息沿着折叠链一路搬到底。
func Repair(ctx context.Context, x *command.Executor) ([]Repaired, error) {
	var todo []domain.Item
	if err := x.Store().View(ctx, func(tx *store.Tx) error {
		all, err := items.List(tx, items.Filter{})
		for _, it := range all {
			if it.Proposal != nil && (it.Phase == domain.PhaseProposed || it.Phase == domain.PhaseDropped && it.Proposal.Sweep.Folded()) {
				todo = append(todo, it)
			}
		}
		return err
	}); err != nil {
		return nil, err
	}
	at := func(it domain.Item) int64 {
		if it.Phase == domain.PhaseProposed || it.Proposal.Sweep.At == nil {
			return 1 << 62 // 建议补记自己放最后：去处可能刚收到折叠链上搬来的
		}
		return it.Proposal.Sweep.At.UnixNano()
	}
	slices.SortStableFunc(todo, func(a, b domain.Item) int { return cmp.Or(cmp.Compare(at(a), at(b)), cmp.Compare(a.ID, b.ID)) })
	out := []Repaired{}
	for _, it := range todo {
		var r Repaired
		if err := x.Store().View(ctx, func(tx *store.Tx) (err error) { // 先看要不要修，不用修的不发命令，第二遍零写入
			cur, err := items.Get(tx, it.ID)
			if err == nil {
				r, err = repairOf(tx, cur)
			}
			return err
		}); err != nil {
			return out, err
		}
		if len(r.Events) == 0 {
			continue
		}
		in, _ := json.Marshal(repairIn{Item: it.ID})
		res, err := x.Do(ctx, command.Request{Identity: system, Command: CmdRepair, Input: in})
		if err != nil {
			return out, err
		}
		if err := json.Unmarshal(res.Value, &r); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}
