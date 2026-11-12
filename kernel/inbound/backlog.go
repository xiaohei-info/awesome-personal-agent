package inbound

// 未判定积压（§9.3）：因模型不可用而未判定的事件在模型恢复后自动重判；超出时限的转 expired，不再自动重判；
// 本人可把积压忽略，单条、按来源或全部。每一步都经命令写入，带操作者留痕；状态全在账本，退避只在内存。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// CmdRequeue 把一条因模型不可用而未判定的事件放回 pending，交给入站流水线重判（内核自己发起）。
	CmdRequeue = "inbound.requeue"
	// CmdExpire 把超出重判时限的未判定事件转为 expired（内核自己发起）。
	CmdExpire = "inbound.expire"
	// CmdIgnoreUnjudged 由本人把积压（unjudged、expired）记为 ignored：单条、按来源或全部。
	CmdIgnoreUnjudged = "inbound.ignore_unjudged"
	// CmdBacklogRules 是积压放开的第 1 步（§9.3）：只跑规则，不调模型。由本人显式触发（只接受本人原话）。
	// 输入不带 event 时只过授权与校验；带 event 时把这一条积压事件按规则出口结清，操作者是调用者。
	CmdBacklogRules = "inbound.backlog_rules"
	// EvRequeued 是重判入队的审计记录。
	EvRequeued = "inbound.requeued"
)

// RejudgeLimits 是自动重判的上限与节流。
type RejudgeLimits struct {
	Window  time.Duration // 只重判发生在这段时间内的事件；更早的转 expired。为 0（其余字段非零）表示关闭：既不重判也不转 expired
	Batch   int           // 每轮至多重判的条数
	Backoff time.Duration // 一轮里模型仍不可用时，至少等这么久再探；模型端点改了不等
}

// DefaultRejudge 是缺省的上限：最近 7 天、每轮 20 条、模型仍不可用时退避 10 分钟。
var DefaultRejudge = RejudgeLimits{Window: 7 * 24 * time.Hour, Batch: 20, Backoff: 10 * time.Minute}

type probeState struct {
	next     time.Time
	endpoint string
}

type backlogIn struct {
	Event    string `json:"event,omitempty"`
	Source   string `json:"source,omitempty"`
	All      bool   `json:"all,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`  // 重判第几次：同一次入队幂等，下一次是新的入队
	Outbound bool   `json:"outbound,omitempty"` // 入队对象是因外发范围未选择而未判定的事件
	Reason   string `json:"reason,omitempty"`
}

func parseBacklog(in json.RawMessage) (backlogIn, error) {
	var v backlogIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, invalid("bad input: " + err.Error())
	}
	return v, nil
}

func ownObject(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	v, err := parseBacklog(in)
	// 内核自己维护来源事件的处置：执行者记为内核身份，使写入属于"改自己的对象"（同 inbound.dispose）。
	return authz.Object{ID: v.Event, Assignee: systemPrinc}, err
}

func (s *Service) registerBacklog(x *command.Executor) error {
	return errors.Join(
		x.Register(command.Spec{Name: CmdRequeue, Action: authz.ActionUpdateOwn, Object: ownObject,
			Apply: func(c *command.Ctx) (any, error) {
				v, _ := parseBacklog(c.Input)
				requeue := runner.Requeue
				if v.Outbound {
					requeue = runner.RequeueOutbound
				}
				n, err := requeue(c.Tx, v.Event)
				if err != nil {
					return nil, err
				}
				p, _ := json.Marshal(map[string]any{"event": v.Event, "rejudges": n})
				_, err = c.Emit(domain.Event{Type: EvRequeued, Object: v.Event, Reason: orElse(v.Reason, "model endpoint is configured again; rejudging"), Payload: p})
				return nil, err
			}}),
		x.Register(rulesSpec()),
		x.Register(command.Spec{Name: CmdExpire, Action: authz.ActionUpdateOwn, Object: ownObject,
			Apply: func(c *command.Ctx) (any, error) {
				v, _ := parseBacklog(c.Input)
				return nil, settle(c, v.Event, domain.DispositionExpired, v.Reason)
			}}),
		x.Register(command.Spec{Name: CmdIgnoreUnjudged, Action: authz.ActionManageSource, Tier: authz.TierWords,
			Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
				v, err := parseBacklog(in)
				return authz.Object{ID: v.Event}, err
			},
			Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
				v, _ := parseBacklog(in)
				n := 0
				for _, set := range []bool{v.Event != "", v.Source != "", v.All} {
					if set {
						n++
					}
				}
				if n != 1 {
					return invalid("give exactly one of event, source or all")
				}
				if v.Event != "" {
					_, err := runner.Event(tx, v.Event)
					return err
				}
				return nil
			},
			Apply: func(c *command.Ctx) (any, error) {
				v, _ := parseBacklog(c.Input)
				ids := []string{v.Event}
				if v.Event == "" {
					var err error
					if ids, err = runner.Backlog(c.Tx, v.Source); err != nil {
						return nil, err
					}
				}
				for _, id := range ids {
					if err := settle(c, id, domain.DispositionIgnored, orElse(v.Reason, "ignored by the owner from the source page")); err != nil {
						return nil, err
					}
				}
				return map[string]any{"ignored": len(ids)}, nil
			}}),
	)
}

// settle 结清一条积压事件并记处置的审计事件；操作者是命令的调用方。
func settle(c *command.Ctx, event string, to domain.Disposition, reason string) error {
	if err := runner.Resolve(c.Tx, event, to, ""); err != nil {
		return err
	}
	p, _ := json.Marshal(map[string]any{"event": event, "disposition": to})
	_, err := c.Emit(domain.Event{Type: EvDisposed, Object: event, Reason: reason, Payload: p})
	return err
}

// requeueOutboundChosen 把因外发范围未选择而未判定、现在已可送的事件放回 pending 重判（§14.14、§9.3）：
// 来源（或对话捕获的项目）选定了外部范围，或端点位置改了。范围仍不可用的事件留在积压里。
func (s *Service) requeueOutboundChosen(ctx context.Context, since time.Time) []error {
	var ready []domain.SourceEvent
	err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		unset, err := runner.OutboundUnset(tx, since)
		if err != nil || len(unset) == 0 {
			return err
		}
		sn := snapshot{}
		if err := sn.loadFacts(tx); err != nil {
			return err
		}
		for _, ev := range unset {
			src, err := sn.source(ev.SourceID)
			if err == nil && sn.outboundBlock(src) == "" {
				ready = append(ready, ev)
			}
		}
		return nil
	})
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, ev := range ready {
		n := ev.Rejudges + 1
		if _, err := s.do(ctx, system, "", fmt.Sprintf("inbound:requeue:%s:%d", ev.ID, n), CmdRequeue,
			backlogIn{Event: ev.ID, Attempt: n, Outbound: true, Reason: "outbound scope is chosen; rejudging"}, command.Chain{}); err != nil {
			errs = append(errs, fmt.Errorf("inbound: requeue %s: %w", ev.ID, err))
		}
	}
	if len(ready) > 0 {
		if err := s.Process(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// Rejudge 跑一轮积压维护，由后台维护循环定期调用：
//
//  1. 发生在 Window 之前、仍未判定的事件转 expired，不再自动重判，也不再计入积压；
//  2. 模型端点已配置时，按重判次数少、落盘早的顺序取至多 Batch 条因模型不可用而未判定的事件，逐条放回 pending 交流水线重判；
//     一条重判后仍因模型不可用回到 unjudged，这一轮就停下，Backoff 之内不再探（端点改了立即再探）。
//
// 判断未定的事件不自动重判：同一输入会复用上次的判断结果（§8.5），重判没有意义。每步幂等，中途失败下一轮接着做。
func (s *Service) Rejudge(ctx context.Context) error {
	if !s.rejudging.TryLock() {
		return nil
	}
	defer s.rejudging.Unlock()
	now, lim := s.o.Now(), s.o.Rejudge
	if lim.Window <= 0 {
		return nil
	}
	since := now.Add(-lim.Window)
	var (
		stale    []string
		cands    []domain.SourceEvent
		endpoint string
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if stale, err = runner.StaleUnjudged(tx, since); err != nil {
			return err
		}
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		// 判断端点与生成端点任一可用就值得探：任一端点改了立即再探。
		if ep := pol.GenerateEndpoint; ep.URL != "" && ep.Model != "" {
			endpoint = ep.URL + "\n" + ep.Model
		}
		if ep := pol.JudgeEndpoint; ep != nil && ep.URL != "" {
			endpoint += "\n" + ep.URL + "\n" + ep.Model
		}
		cands, err = runner.RejudgeCandidates(tx, since, lim.Batch)
		return err
	})
	if err != nil {
		return err
	}
	var errs []error
	why := fmt.Sprintf("unjudged for more than %s; no longer rejudged automatically", lim.Window)
	for _, id := range stale {
		if _, err := s.do(ctx, system, "", "inbound:expire:"+id, CmdExpire, backlogIn{Event: id, Reason: why}, command.Chain{}); err != nil {
			errs = append(errs, fmt.Errorf("inbound: expire %s: %w", id, err))
		}
	}
	errs = append(errs, s.requeueOutboundChosen(ctx, since)...)
	if endpoint == "" || len(cands) == 0 || (endpoint == s.probe.endpoint && now.Before(s.probe.next)) {
		return errors.Join(errs...)
	}
	s.probe = probeState{}
	for _, ev := range cands {
		n := ev.Rejudges + 1
		if _, err := s.do(ctx, system, "", fmt.Sprintf("inbound:requeue:%s:%d", ev.ID, n), CmdRequeue,
			backlogIn{Event: ev.ID, Attempt: n}, command.Chain{}); err != nil {
			errs = append(errs, fmt.Errorf("inbound: requeue %s: %w", ev.ID, err))
			continue
		}
		if err := s.Process(ctx); err != nil {
			errs = append(errs, err)
		}
		var got domain.SourceEvent
		if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { got, err = runner.Event(tx, ev.ID); return }); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if got.Disposition == domain.DispositionPending { // 正在运行的另一轮接手了，或处理出错留在 pending：下一轮再看
			break
		}
		if got.Disposition == domain.DispositionUnjudged && got.UnjudgedReason == domain.UnjudgedModelUnavailable {
			s.probe = probeState{next: now.Add(lim.Backoff), endpoint: endpoint}
			break
		}
	}
	return errors.Join(errs...)
}

// ---- 积压放开第 1 步：只跑规则（§9.3） ----

type rulesIn struct {
	Source      string             `json:"source,omitempty"`
	Event       string             `json:"event,omitempty"`
	Disposition domain.Disposition `json:"disposition,omitempty"`
	Item        string             `json:"item,omitempty"`
	Rule        string             `json:"rule,omitempty"`
	Reason      string             `json:"reason,omitempty"`
}

func parseRules(in json.RawMessage) (rulesIn, error) {
	var v rulesIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, invalid("bad input: " + err.Error())
	}
	return v, nil
}

func rulesSpec() command.Spec {
	return command.Spec{Name: CmdBacklogRules, Action: authz.ActionManageSource, Tier: authz.TierWords,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseRules(in)
			return authz.Object{ID: v.Event}, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parseRules(in)
			if v.Event == "" {
				return nil
			}
			switch v.Disposition {
			case domain.DispositionIgnored, domain.DispositionDigest, domain.DispositionProposed:
			default:
				return invalid("rules can only settle the backlog as ignored, digest or proposed")
			}
			_, err := runner.Event(tx, v.Event)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseRules(c.Input)
			if v.Event == "" {
				return nil, nil
			}
			if err := runner.Resolve(c.Tx, v.Event, v.Disposition, v.Item); err != nil {
				return nil, err
			}
			p, _ := json.Marshal(map[string]any{"event": v.Event, "disposition": v.Disposition, "item": v.Item, "rule": v.Rule})
			_, err := c.Emit(domain.Event{Type: EvDisposed, Object: v.Event, Reason: v.Reason, Payload: p})
			return nil, err
		}}
}

// RulesResult 是只跑规则的结果：挂靠的条数、按规则出口处置的条数、仍留在积压里的条数。
type RulesResult struct {
	Attached  int `json:"attached"`
	Disposed  int `json:"disposed"`
	Remaining int `json:"remaining"`
}

// BacklogRules 让积压（unjudged、expired，含旧原因 undecided 与 restored）重走流水线的第 1–3 步，不调模型：
// 能挂靠的挂靠；规则出口是 proposed、ignore、digest 的按出口处置（转建议不自动接受）；其余原样留在积压里，原因不改写。
// req 带调用者的身份与本人原话，授权由命令管线判定；source 非空时只看该来源。
// 每个事件各自幂等、各自失败：错误合并返回，已完成的不回滚，重跑只处理还在积压里的。
func (s *Service) BacklogRules(ctx context.Context, req command.Request, source string) (RulesResult, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	call := func(in rulesIn) (command.Result, error) {
		b, _ := json.Marshal(in)
		r := req
		r.Command, r.Input, r.IdemKey = CmdBacklogRules, b, ""
		return s.o.Exec.Do(ctx, r)
	}
	// 先过授权：item.create 对任何凭据开放，放到后面会让被拒绝的调用者留下事项。
	if _, err := call(rulesIn{Source: source}); err != nil {
		return RulesResult{}, err
	}
	var (
		sn  snapshot
		evs []domain.SourceEvent
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		ids, err := runner.Backlog(tx, source)
		if err != nil {
			return err
		}
		for _, id := range ids {
			ev, err := runner.Event(tx, id)
			if err != nil {
				return err
			}
			evs = append(evs, ev)
		}
		return sn.loadFacts(tx)
	})
	if err != nil {
		return RulesResult{}, err
	}
	res := RulesResult{Remaining: len(evs)}
	var errs []error
	for _, ev := range evs {
		attached, settled, err := s.rulesOne(ctx, req.Identity, sn, ev, call)
		if err != nil {
			errs = append(errs, fmt.Errorf("inbound: backlog rules %s: %w", ev.ID, err))
			continue
		}
		if attached {
			res.Attached++
		}
		if settled {
			res.Disposed++
		}
	}
	res.Remaining -= res.Attached + res.Disposed
	return res, errors.Join(errs...)
}

func (s *Service) rulesOne(ctx context.Context, id command.Identity, sn snapshot, ev domain.SourceEvent, call func(rulesIn) (command.Result, error)) (attached, settled bool, err error) {
	src, err := sn.source(ev.SourceID)
	if err != nil {
		return false, false, err
	}
	m, merr := s.match(src, ev)
	if s.o.Intercept != nil {
		onUpdate := ""
		if m != nil {
			onUpdate = m.rule.OnUpdate
		}
		if attached, _, err = s.o.Intercept(ctx, ev, onUpdate); err != nil || attached {
			return attached, false, err
		}
	}
	if merr != nil || m == nil {
		return false, false, merr
	}
	d := rulesIn{Event: ev.ID, Rule: m.key(), Reason: "rule " + m.key() + " (backlog, rules only)"}
	switch rules.Exit(m.rule) {
	case rules.ExitIgnore:
		d.Disposition = domain.DispositionIgnored
	case rules.ExitDigest:
		d.Disposition = domain.DispositionDigest
	case rules.ExitProposed:
		d.Disposition = domain.DispositionProposed
		d.Item = sn.made[ev.ID] // 上次中途失败留下的事项：接上，不重建
		if d.Item == "" {
			project, err := s.project(sn, src, ev, m)
			if err != nil {
				return false, false, err
			}
			kind := defaultKind
			if m.rule.Kind != "" {
				kind = domain.Kind(m.rule.Kind)
			}
			p := proposal{kind: kind, title: orElse(ev.Subject, ev.Type), summary: ev.Summary, reason: d.Reason}
			if d.Item, err = s.createItem(ctx, id, m.key(), src, ev, p, project); err != nil {
				return false, false, err
			}
		}
	default: // screen、extract、alert：要模型或另有出口，留在积压里
		return false, false, nil
	}
	if _, err := call(d); err != nil {
		return false, false, err
	}
	return false, true, nil
}
