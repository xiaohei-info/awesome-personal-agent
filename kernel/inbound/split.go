package inbound

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdSplit 把误并入一条建议的来源事件拆出来，另成一条建议（§9.3 跨来源合并的纠正）。
// 输入不带 to 时只过授权与状态校验，不写入；带 to（已为它建好的新事项）时在同一事务里改两边的建议信息与事件指向。
const CmdSplit = "inbound.split"

type splitIn struct {
	Event string `json:"event"`
	From  string `json:"from"`
	To    string `json:"to,omitempty"`
}

func parseSplit(in json.RawMessage) (splitIn, error) {
	var v splitIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil || v.Event == "" || v.From == "" {
		return v, invalid("split needs event and from")
	}
	return v, nil
}

// splittable 校验 from 仍是建议、包含这个事件，且这个事件不是它自己的那一个（只有并入的才能拆出）。
func splittable(tx *store.Tx, v splitIn) (domain.Item, error) {
	it, err := items.Get(tx, v.From)
	if err != nil {
		return it, err
	}
	if it.Phase != domain.PhaseProposed || it.Proposal == nil {
		return it, domain.NewError(domain.CodeConflict, "item "+v.From+" is "+string(it.Phase)+", only proposals can be split", "re-read the item")
	}
	var ext struct {
		SourceEvent string `json:"source_event"`
	}
	_ = json.Unmarshal(it.Ext, &ext) // 不是入站建的事项没有 ext：只按 proposal.sources 判
	if !slices.Contains(it.Proposal.Sources, v.Event) || ext.SourceEvent == v.Event || len(it.Proposal.Sources) < 2 {
		return it, domain.NewError(domain.CodeConflict, "source event "+v.Event+" was not merged into "+v.From, "split only a source that was merged in")
	}
	return it, nil
}

func registerSplit(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdSplit, Action: authz.ActionManageSource, Tier: authz.TierOrganize,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseSplit(in)
			return authz.Object{ID: v.Event}, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parseSplit(in)
			if v.To != "" { // 第二步：第一步已校验过；重放时事件已不在原建议里，不再校验
				return nil
			}
			_, err := splittable(tx, v)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseSplit(c.Input)
			if v.To == "" {
				return nil, nil
			}
			from, err := items.Get(c.Tx, v.From)
			if err != nil {
				return nil, err
			}
			p := *from.Proposal
			p.Sources = slices.DeleteFunc(slices.Clone(p.Sources), func(s string) bool { return s == v.Event })
			p.Merges = slices.DeleteFunc(slices.Clone(p.Merges), func(m domain.ProposalMerge) bool { return m.Event == v.Event })
			if !slices.Contains(p.NotSame, v.To) {
				p.NotSame = append(slices.Clone(p.NotSame), v.To)
			}
			w1, err := items.SetProposal(c.Tx, v.From, p, c.Now())
			if err != nil {
				return nil, err
			}
			w2, err := items.SetProposal(c.Tx, v.To, domain.Proposal{Sources: []string{v.Event}, SplitFrom: v.From, NotSame: []string{v.From}}, c.Now())
			if err != nil {
				return nil, err
			}
			if err := runner.Repoint(c.Tx, v.Event, v.From, v.To); err != nil {
				return nil, err
			}
			for _, w := range []items.Write{w1, w2} {
				if _, err := command.Bump(c.Tx, w.Object); err != nil {
					return nil, err
				}
				if _, err := c.Emit(domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, Reason: "split source event " + v.Event + " out of " + v.From}); err != nil {
					return nil, err
				}
			}
			pl, _ := json.Marshal(map[string]any{"event": v.Event, "disposition": domain.DispositionProposed, "item": v.To, "split_from": v.From})
			_, err = c.Emit(domain.Event{Type: EvDisposed, Object: v.Event, Reason: "split out of " + v.From, Payload: pl})
			return map[string]string{"item": v.To}, err
		}})
}

// Split 以 id 的身份把并入建议 from 的来源事件 event 拆成一条新建议，返回新建议的 id。
// 新建议用并入时留下的候选内容（标题、摘要、kind、项目），没有留痕的旧合并用来源事件的标题与摘要。
// 两步各有幂等键：重试不会多建一条，也不会重复改写。
func (s *Service) Split(ctx context.Context, id command.Identity, event, from string) (string, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	in := splitIn{Event: event, From: from}
	if _, err := s.do(ctx, id, "", "", CmdSplit, in, command.Chain{}); err != nil {
		return "", err
	}
	var (
		it  domain.Item
		ev  domain.SourceEvent
		src domain.Source
		sn  snapshot
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if it, err = splittable(tx, in); err != nil {
			return err
		}
		if ev, err = runner.Event(tx, event); err != nil {
			return err
		}
		return sn.loadFacts(tx)
	})
	if err != nil {
		return "", err
	}
	if src, err = sn.source(ev.SourceID); err != nil {
		return "", err
	}
	p := proposal{kind: defaultKind, title: orElse(ev.Subject, ev.Type), summary: ev.Summary, reason: "split out of " + from}
	project := ""
	for _, m := range it.Proposal.Merges {
		if m.Event == event {
			p.title, p.summary, project = orElse(m.Title, p.title), m.Summary, m.Project
			if m.Kind != "" {
				p.kind = m.Kind
			}
		}
	}
	to, err := s.createItemKeyed(ctx, system, "", "inbound:split:"+event+":"+from, src, ev, p, project)
	if err != nil {
		return "", err
	}
	in.To = to
	if _, err := s.do(ctx, id, "", "inbound:split:"+event+":"+from+":apply", CmdSplit, in, command.Chain{}); err != nil {
		return "", err
	}
	return to, nil
}
