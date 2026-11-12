package inbound

import (
	"context"
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdProposeUnjudged 把积压的来源事件（unjudged、expired）记为 proposed（§9.3）。输入不带 item 时只过授权与状态校验，不写入。
const CmdProposeUnjudged = "inbound.propose_unjudged"

func registerPropose(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdProposeUnjudged, Action: authz.ActionManageSource,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseDispose(in)
			return authz.Object{ID: v.Event}, err
		},
		Validate: func(tx *store.Tx, o authz.Object, _ json.RawMessage) error {
			ev, err := runner.Event(tx, o.ID)
			if err != nil {
				return err
			}
			if ev.Disposition != domain.DispositionUnjudged && ev.Disposition != domain.DispositionExpired {
				return domain.NewError(domain.CodeConflict, "source event "+o.ID+" is "+string(ev.Disposition)+", not unjudged", "re-read the source event")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseDispose(c.Input)
			if v.Item == "" {
				return nil, nil
			}
			if err := runner.Resolve(c.Tx, v.Event, domain.DispositionProposed, v.Item); err != nil {
				return nil, err
			}
			p, _ := json.Marshal(map[string]any{"event": v.Event, "disposition": domain.DispositionProposed, "item": v.Item})
			_, err := c.Emit(domain.Event{Type: EvDisposed, Object: v.Event, Reason: v.Reason, Payload: p})
			return nil, err
		}})
}

// ProposeUnjudged 以 id 的身份把未判定事件转为 proposed 事项，返回更新后的来源事件。kind 与项目沿用命中的规则，
// 缺省 task、不归项目。事项与处置是两条命令：中途失败时事件仍在积压里，重试接回已建事项。
func (s *Service) ProposeUnjudged(ctx context.Context, id command.Identity, event string) (domain.SourceEvent, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	// 先过授权：item.create 对任何凭据开放，放到后面会让被拒绝的调用者留下事项。
	if _, err := s.do(ctx, id, "", "", CmdProposeUnjudged, disposeIn{Event: event}, command.Chain{}); err != nil {
		return domain.SourceEvent{}, err
	}
	item, err := s.draftProposal(ctx, id, event)
	var ev domain.SourceEvent
	if err != nil {
		return ev, err
	}
	d := disposeIn{Event: event, Item: item, Reason: "manually proposed from the source page"}
	if _, err := s.do(ctx, id, "", "", CmdProposeUnjudged, d, command.Chain{}); err != nil {
		return ev, err
	}
	err = s.o.Store.View(ctx, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, event); return })
	return ev, err
}

// DraftProposal 为来源事件建 proposed 事项并返回它的 id；这个事件已经建过就沿用，重试不会重复建。
// 只建事项，不改事件的处置：处置由调用方的命令落账（积压转建议、抽检"该看"）。
func (s *Service) DraftProposal(ctx context.Context, id command.Identity, event string) (string, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	return s.draftProposal(ctx, id, event)
}

func (s *Service) draftProposal(ctx context.Context, id command.Identity, event string) (string, error) {
	var (
		sn snapshot
		ev domain.SourceEvent
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if ev, err = runner.Event(tx, event); err != nil {
			return err
		}
		return sn.loadFacts(tx)
	})
	if err != nil {
		return "", err
	}
	if item, made := sn.made[event]; made {
		return item, nil
	}
	return s.proposeItem(ctx, id, sn, ev)
}

func (s *Service) proposeItem(ctx context.Context, id command.Identity, sn snapshot, ev domain.SourceEvent) (string, error) {
	src, err := sn.source(ev.SourceID)
	if err != nil {
		return "", err
	}
	m, err := s.match(src, ev)
	if err != nil {
		return "", err
	}
	project, err := s.project(sn, src, ev, m)
	if err != nil {
		return "", err
	}
	kind := defaultKind
	if m != nil && m.rule.Kind != "" {
		kind = domain.Kind(m.rule.Kind)
	}
	p := proposal{kind: kind, title: orElse(ev.Subject, ev.Type), summary: ev.Summary, reason: "manually proposed from the source page"}
	return s.createItem(ctx, id, "", src, ev, p, project)
}
