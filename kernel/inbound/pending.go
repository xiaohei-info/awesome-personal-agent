package inbound

import (
	"context"
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdReleasePending 把已取消的批量归类留下的"待归类"建议放回普通建议（§9.3、§16.5）：只去掉归类标记，别的不动。
const CmdReleasePending = "inbound.release_pending"

type releasePendingIn struct {
	Item string `json:"item"`
}

func pendingSweep(it domain.Item) bool {
	return it.Phase == domain.PhaseProposed && it.Proposal != nil && it.Proposal.Sweep != nil && it.Proposal.Sweep.State == "pending"
}

func registerReleasePending(x *command.Executor) error {
	parse := func(in json.RawMessage) (releasePendingIn, error) {
		var v releasePendingIn
		if err := json.Unmarshal(in, &v); err != nil || v.Item == "" {
			return v, invalid("release_pending needs item")
		}
		return v, nil
	}
	return x.Register(command.Spec{Name: CmdReleasePending, Action: authz.ActionUpdateOwn, Group: authz.GroupInternal, KeepVersion: true,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			return authz.Object{ID: v.Item, Assignee: systemPrinc}, err // 内核整理自己建的建议
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := parse(c.Input)
			if err != nil {
				return nil, err
			}
			it, err := items.Get(c.Tx, v.Item)
			if err != nil || !pendingSweep(it) {
				return nil, err
			}
			p := *it.Proposal
			p.Sweep = nil
			w, err := items.SetProposal(c.Tx, it.ID, p, nil, c.Now())
			if err != nil {
				return nil, err
			}
			return nil, emitWrite(c, w, "batch classification cancelled: back to a plain proposal")
		}})
}

// ReleasePending 在启动时把所有"待归类"建议放回普通建议，返回条数；做过之后没有可做的，不写任何事件。
func (s *Service) ReleasePending(ctx context.Context, x *command.Executor) (int, error) {
	var todo []string
	if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		all, err := items.List(tx, items.Filter{})
		for _, it := range all {
			if pendingSweep(it) {
				todo = append(todo, it.ID)
			}
		}
		return err
	}); err != nil {
		return 0, err
	}
	for i, id := range todo {
		raw, _ := json.Marshal(releasePendingIn{Item: id})
		if _, err := x.Do(ctx, command.Request{Identity: system, Command: CmdReleasePending, Input: raw}); err != nil {
			return i, err
		}
	}
	return len(todo), nil
}
