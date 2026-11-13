package inbound

// 积压放开第 4 步：放开重判（§9.3）。剩余积压按分数从高到低（没有判断端点时按落盘顺序）走完整流水线，
// 进入建议流的数量受每日上限约束（运行配置 backlog_release_per_day），被忽略的不占额度，当天用完的留到次日。
//
// 按批提交：每批的条数不超过今天剩余的额度，所以一批里即使全部进入建议流也不会超额；批内每条先各记一条 inbound.requeued
// （操作者是调用者），再交流水线，分类端点的初筛题合并成批先问掉（不超过 max_batch）。状态全在账本：中途崩溃时已放回的事件停在
// pending，下一次调用先把它们处理完再算额度；还在积压里的原样等下一次，不重复、不遗漏。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdBacklogRejudge 是积压放开的第 4 步。由本人显式触发（只接受本人原话）。输入不带 event 时只过授权；
// 带 event 时把这一条积压事件放回 pending 并记 inbound.requeued，操作者是调用者。
const CmdBacklogRejudge = "inbound.backlog_rejudge"

// Prefetcher 是判断入口把分类端点一级合并成批先问掉的能力（*direct.Service 满足它）；没有它时初筛逐条提交。
type Prefetcher interface {
	Prefetch(ctx context.Context, calls []direct.JudgeCall)
}

// ReleaseResult 是放开重判的结果：放回重判的条数，以及因当天额度用完而留到次日的条数。
type ReleaseResult struct {
	Requeued int `json:"requeued"`
	Deferred int `json:"deferred"`
}

type releaseIn struct {
	Source  string `json:"source,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Event   string `json:"event,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

func parseRelease(in json.RawMessage) (releaseIn, error) {
	var v releaseIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, invalid("bad input: " + err.Error())
	}
	if v.Limit < 0 {
		return v, invalid("limit must not be negative")
	}
	return v, nil
}

func rejudgeSpec() command.Spec {
	return command.Spec{Name: CmdBacklogRejudge, Action: authz.ActionManageSource, Tier: authz.TierWords,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseRelease(in)
			return authz.Object{ID: v.Event}, err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseRelease(c.Input)
			if v.Event == "" {
				return nil, nil
			}
			n, err := runner.RequeueBacklog(c.Tx, v.Event)
			if err != nil {
				return nil, err
			}
			// release 标记这次放回属于积压放开：当天额度按它统计。
			p, _ := json.Marshal(map[string]any{"event": v.Event, "rejudges": n, "release": true})
			_, err = c.Emit(domain.Event{Type: EvRequeued, Object: v.Event, Reason: "backlog release (rejudge)", Payload: p})
			return nil, err
		}}
}

// dayWindow 是 now 所在的日历日 [起, 止)。
func dayWindow(now time.Time) (time.Time, time.Time) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start, start.AddDate(0, 0, 1)
}

// releasedToday 是今天从积压放开进入建议流的来源事件数：今天被放开重判、现在处置为 proposed 的。
func releasedToday(tx *store.Tx, now time.Time) (int, error) {
	from, to := dayWindow(now)
	r, err := tx.Query(`SELECT COUNT(DISTINCT e.object) FROM eventlog_event e
		JOIN sources_runner_event r ON r.id = e.object
		WHERE e.type = ? AND e.tombstone = 0 AND e.at >= ? AND e.at < ? AND e.payload LIKE '%"release":true%' AND r.disposition = 'proposed'`,
		EvRequeued, from.UnixNano(), to.UnixNano())
	if err != nil {
		return 0, err
	}
	defer r.Close()
	var n int64
	if r.Next() {
		err = r.Scan(&n)
	}
	return int(n), errors.Join(err, r.Err())
}

// BacklogRejudge 放开重判积压（unjudged 任何原因、expired）。source 非空时只看该来源；limit 大于 0 时本次至多让这么多条进入建议流
// （仍受当天额度约束）。req 带调用者的身份与本人原话，授权由命令管线判定。
// 每批各自成败：错误合并返回，已放回的不回滚，再次调用接着做。
func (s *Service) BacklogRejudge(ctx context.Context, req command.Request, source string, limit int) (ReleaseResult, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	call := func(in releaseIn, idem string) error {
		b, _ := json.Marshal(in)
		r := req
		r.Command, r.Input, r.IdemKey = CmdBacklogRejudge, b, idem
		_, err := s.o.Exec.Do(ctx, r)
		return err
	}
	// 先过授权和校验，再碰账本。
	if err := call(releaseIn{Source: source, Limit: limit}, ""); err != nil {
		return ReleaseResult{}, err
	}
	// 上一次中途崩溃留在 pending 的事件先处理完：它们进入建议流的部分要算进今天的额度。
	if err := s.Process(ctx); err != nil {
		return ReleaseResult{}, err
	}

	var (
		ordered  []domain.SourceEvent
		maxBatch int
		hasJudge bool
		used     int
	)
	now := s.o.Now()
	err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		if ep := pol.JudgeEndpoint; ep != nil && ep.URL != "" {
			hasJudge, maxBatch = true, ep.MaxBatch
		}
		if maxBatch <= 0 {
			maxBatch = policy.DefaultJudgeMaxBatch
		}
		ids, err := runner.Backlog(tx, source)
		if err != nil {
			return err
		}
		evs := make([]domain.SourceEvent, len(ids))
		for i, id := range ids {
			if evs[i], err = runner.Event(tx, id); err != nil {
				return err
			}
		}
		if hasJudge { // 没有判断端点时没有分数，按落盘顺序
			scored, err := SortByScore(tx, evs)
			if err != nil {
				return err
			}
			for i := range scored {
				evs[i] = scored[i].SourceEvent
			}
		}
		ordered = evs
		used, err = releasedToday(tx, now)
		return err
	})
	if err != nil {
		return ReleaseResult{}, err
	}

	// allow 是本次还能让多少条进入建议流；unlimited 为真时不限（backlog_release_per_day: 0 且没给 limit）。
	unlimited := s.o.BacklogReleasePerDay <= 0 && limit <= 0
	allow := s.o.BacklogReleasePerDay - used
	if s.o.BacklogReleasePerDay <= 0 {
		allow = limit
	} else if limit > 0 && limit < allow {
		allow = limit
	}
	var (
		res  ReleaseResult
		errs []error
	)
	rest := ordered
	for len(rest) > 0 && (unlimited || allow > 0) {
		k := len(rest)
		if !unlimited && allow < k {
			k = allow
		}
		if hasJudge && k > maxBatch {
			k = maxBatch
		}
		if hasJudge {
			s.prefetchScreen(ctx, rest[:min(len(rest), maxBatch)])
		}
		batch := rest[:k]
		rest = rest[k:]
		proposed, stop, berrs := s.releaseBatch(ctx, call, batch, &res)
		errs = append(errs, berrs...)
		if !unlimited {
			allow -= proposed
		}
		if stop {
			break
		}
	}
	res.Deferred = len(rest)
	return res, errors.Join(errs...)
}

// releaseBatch 放回一批、交流水线处理，返回其中进入建议流的条数。模型不可用或处理出错时 stop 为真：不再往下放，余下的原样留在积压或 pending。
func (s *Service) releaseBatch(ctx context.Context, call func(releaseIn, string) error, batch []domain.SourceEvent, res *ReleaseResult) (proposed int, stop bool, errs []error) {
	var sent []string
	for _, ev := range batch {
		// 幂等键带重判次数：同一次放回只记一条，下一次是新的放回。
		if err := call(releaseIn{Event: ev.ID, Attempt: ev.Rejudges + 1}, fmt.Sprintf("inbound:rejudge:%s:%d", ev.ID, ev.Rejudges+1)); err != nil {
			return proposed, true, append(errs, fmt.Errorf("inbound: rejudge %s: %w", ev.ID, err))
		}
		res.Requeued++
		sent = append(sent, ev.ID)
	}
	if err := s.Process(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, id := range sent {
		var got domain.SourceEvent
		if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { got, err = runner.Event(tx, id); return }); err != nil {
			return proposed, true, append(errs, err)
		}
		switch {
		case got.Disposition == domain.DispositionProposed:
			proposed++
		case got.Disposition == domain.DispositionPending: // 处理出错留在 pending：按进入建议流计，保证不超额，下一次调用接手
			proposed++
			stop = true
		case got.Disposition == domain.DispositionUnjudged && got.UnjudgedReason == domain.UnjudgedModelUnavailable:
			stop = true
		}
	}
	if stop && len(errs) == 0 {
		errs = append(errs, domain.NewError(domain.CodeConflict, "the model is unavailable, so the backlog release stopped",
			"check the model endpoints in Settings, then release again"))
	}
	return proposed, stop, errs
}

// prefetchScreen 把这一窗口里要走初筛的事件的分类端点一级合并成批先问掉；题与入站初筛一致，之后流水线逐条作答时复用。
// 归属会挂靠的事件由 Options.Attaches 预判后跳过；预判只读，同一窗口里前一条事件新建的事项它看不到（多问一次，不影响结果）。
func (s *Service) prefetchScreen(ctx context.Context, window []domain.SourceEvent) {
	pf, ok := s.o.Judge.(Prefetcher)
	if !ok {
		return
	}
	var sn snapshot
	if err := s.o.Store.View(ctx, sn.loadFacts); err != nil {
		return
	}
	var calls []direct.JudgeCall
	for _, ev := range window {
		src, err := sn.source(ev.SourceID)
		if err != nil || IsCaptureSource(src.ID) {
			continue
		}
		m, err := s.match(src, ev)
		if err != nil {
			continue
		}
		var custom string
		if m != nil {
			if rules.Exit(m.rule) != rules.ExitScreen {
				continue
			}
			custom = m.rule.Screen
		}
		if s.o.Attaches != nil {
			onUpdate := ""
			if m != nil {
				onUpdate = m.rule.OnUpdate
			}
			if s.o.Attaches(ctx, ev, onUpdate) { // 归属在规则之前：会挂靠原事项的事件不进初筛
				continue
			}
		}
		pack, screen := m.tmpl(custom, Template)
		o, pol, err := s.freshOutbound(ctx, src)
		if err != nil || outboundGate(o, pol, src) != "" {
			continue
		}
		calls = append(calls, direct.JudgeCall{Pack: pack, Template: screen, Input: eventInput(src, ev, "", nil),
			Material: material(ev, o), Subject: ev.ID, Source: src.ID, Scope: direct.Scope{Source: src.ID}, UnjudgedOnUnavailable: true})
	}
	if len(calls) > 0 {
		pf.Prefetch(ctx, calls)
	}
}
