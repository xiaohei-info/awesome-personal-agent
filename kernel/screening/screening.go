// Package screening 是决策请求的 web_only 判断型初筛（设计 §14.4、§7.2、§6.14 第 4 条）。
//
// 时序：decision.request 在同一事务里（经 decisions.Screening）置 screening_pending 并写下判断型 Run 的起点；
// 提交后本包调一次判断型（decision-card/web-only-screening），结果经 items.Revise 写回。
// 只升不降：返回"升级"才把 risk_class 升为 web_only 并清除超时默认值，其余一律保持规则的结果。
// 失败、超时、内核重启中断都按规则继续：清除 screening_pending，Event 记原因，进健康视图；不重试、不阻塞。
// 初筛结束前超时默认值不生效：由决策模块的超时处理器读 screening_pending 把关（本包不碰 Timer 的触发）。
//
// 本包不发模型请求（经 Judge）、不附带 event_stats、不起子进程，只写自己对事项的初筛结果（经 items.Revise）
// 和 Event 日志；判断型 Run 的状态沿用专属 agent 的 Event 留痕（kernel/agents/direct），没有独立的 Run 表行。
package screening

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 初筛用的判断型提示词，由 decision-card 内置包声明（附录 C.14）。
const (
	Pack     = "decision-card"
	Template = "web-only-screening"
)

// upgradeAt 是 yes 概率的升级线：判断型回答 yes 表示应升级（提示词约定拿不准倾向 yes）。
const upgradeAt = 0.5

// Judge 是判断型调用（*direct.Service 满足它）。
// 装配处必须给初筛一个 Attempts=1 的实例：初筛失败不重试（§14.4），退避重试会把"按规则继续"拖成阻塞。
type Judge interface {
	Do(ctx context.Context, c direct.Call) (direct.Outcome, error)
}

// Options 配置 Service。Now 为零值时取系统时钟。
type Options struct {
	Store *store.Store
	Judge Judge
	Now   func() time.Time
}

// Service 同时是 decisions.Screening（事务内接入点），并提供提交后执行与重启恢复。
type Service struct {
	o        Options
	wg       sync.WaitGroup
	mu       sync.Mutex
	inflight map[string]bool
}

var _ decisions.Screening = (*Service)(nil)

// New 返回 Service。Store 与 Judge 必须给出。
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Judge == nil {
		return nil, errors.New("screening: Store and Judge are required")
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	return &Service{o: o, inflight: map[string]bool{}}, nil
}

var systemActor = domain.Actor{Type: domain.ActorSystem, Principal: "system:screening", Via: domain.ViaClock}

func newRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(b[:]), nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // 只序列化本包构造的 map，不会失败
	}
	return b
}

// ---- 事务内：要不要初筛、写 Run 起点 ----

// Needed：规则已判为 web_only 或未配置模型端点时不做初筛（§14.4）；其余都要。
func (s *Service) Needed(tx *store.Tx, it domain.Item) (bool, error) {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return false, err
	}
	ep := pol.GenerateEndpoint
	if ep.URL == "" || ep.Model == "" {
		return false, nil
	}
	var cls string
	if it.Ask != nil {
		cls = it.Ask.RiskClass
	}
	return !authz.WebOnly(pol, authz.Object{Kind: string(it.Kind), Tags: it.Tags, Project: it.Project, Category: cls}), nil
}

// Begin 在 decision.request 的事务里写下判断型 Run 的起点（run.started）；事务回滚则一并消失。
func (s *Service) Begin(c *command.Ctx, it domain.Item) error {
	id, err := newRunID()
	if err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: domain.EvRunStarted, Object: it.ID, Reason: "web_only screening started",
		Payload: mustJSON(map[string]any{"run_id": id, "category": domain.RunJudge, "template": Template, "target": it.ID, "state": domain.RunRunning})})
	return err
}

// ---- 提交后：调用判断型 ----

// OnCommit 登记到 command.Executor.OnCommit：decision.request 提交后异步初筛。
// 重放、被拒绝或链路深度超限（没有置 screening_pending）的请求会在 run 里被识别并跳过。
func (s *Service) OnCommit(cm command.Commit) {
	if cm.Command != "decision.request" || cm.Object == "" {
		return
	}
	s.mu.Lock()
	if s.inflight[cm.Object] {
		s.mu.Unlock()
		return
	}
	s.inflight[cm.Object] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.run(cm.Object)
}

// Wait 等所有在途初筛结束（测试与优雅停机用）。
func (s *Service) Wait() { s.wg.Wait() }

type prepared struct {
	pending bool
	runID   string
	timeout time.Duration
	call    direct.Call
}

func (s *Service) prepare(ctx context.Context, id string) (p prepared, err error) {
	err = s.o.Store.View(ctx, func(tx *store.Tx) error {
		it, err := items.Get(tx, id)
		if err != nil {
			return err
		}
		if it.Ask == nil || !it.Ask.ScreeningPending {
			return nil
		}
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		depth, err := items.ChainDepth(tx, id)
		if err != nil {
			return err
		}
		if p.runID, err = startedRun(tx, id); err != nil {
			return err
		}
		input := mustJSON(map[string]any{"item": id, "kind": it.Kind, "title": it.Title, "tags": it.Tags, "project": it.Project,
			"question": it.Ask.Question, "options": it.Ask.Options, "free_text": it.Ask.FreeText})
		actor := systemActor
		actor.Run = p.runID
		p.pending = true
		p.timeout = time.Duration(pol.ScreeningTimeoutMs) * time.Millisecond
		// 事项正文是外部内容，只进资料段；初筛不附带 event_stats（NoStats）。
		p.call = direct.Call{Pack: Pack, Template: Template, Input: input, Material: it.Body, Target: id, Project: it.Project,
			NoStats: true, Actor: actor, ChainDepth: depth}
		return nil
	})
	return
}

// startedRun 取事项最近一条判断型 run.started 的 run id。
func startedRun(tx *store.Tx, id string) (string, error) {
	es, err := eventlog.ReadObject(tx, id)
	if err != nil {
		return "", err
	}
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Event.Type != domain.EvRunStarted {
			continue
		}
		var p struct {
			RunID    string `json:"run_id"`
			Template string `json:"template"`
		}
		if json.Unmarshal(es[i].Event.Payload, &p) == nil && p.Template == Template {
			return p.RunID, nil
		}
	}
	return "", nil
}

// result 是一次初筛的结论。
type result struct {
	escalate bool
	reason   string // 写入 Event 的 reason
	failure  string // 非空：失败、超时或中断；进健康视图
	runState domain.RunState
	// ownRunEvent 为真：direct 没有为这次调用留 run.finished（超时取消、未配置、调用前失败、中断），由本包补记。
	ownRunEvent bool
}

func (s *Service) run(id string) {
	defer func() {
		s.mu.Lock()
		delete(s.inflight, id)
		s.mu.Unlock()
		s.wg.Done()
	}()
	ctx := context.Background()
	p, err := s.prepare(ctx, id)
	if err == nil && !p.pending {
		return
	}
	var res result
	if err != nil {
		res = result{reason: "screening failed before the call: " + err.Error(), failure: err.Error(), runState: domain.RunFailed, ownRunEvent: true}
	} else {
		cctx, cancel := context.WithTimeout(ctx, p.timeout)
		out, derr := s.o.Judge.Do(cctx, p.call)
		timedOut := cctx.Err() != nil
		cancel()
		res = interpret(out, derr, timedOut)
	}
	// 写回失败（存储不可用）无处可报：flag 留着，下次启动 Recover 会按规则继续。
	_ = s.finish(ctx, id, p.runID, res)
}

func interpret(out direct.Outcome, err error, timedOut bool) result {
	switch {
	case timedOut || errors.Is(err, model.ErrTimeout):
		return result{reason: "screening timed out; continue by policy rules", failure: "timeout", runState: domain.RunTimeout, ownRunEvent: timedOut}
	case err != nil:
		return result{reason: "screening failed; continue by policy rules: " + err.Error(), failure: err.Error(), runState: domain.RunFailed,
			ownRunEvent: errors.Is(err, model.ErrNotConfigured)}
	case !out.Settled:
		return result{reason: "screening inconclusive (" + out.Reason + "); not escalated", runState: domain.RunSucceeded}
	}
	var j direct.Judgement
	if err := json.Unmarshal(out.Final().Result, &j); err != nil || j.Probability == nil {
		return result{reason: "screening returned no verdict; continue by policy rules", failure: "no verdict", runState: domain.RunFailed, ownRunEvent: true}
	}
	if *j.Probability >= upgradeAt {
		return result{escalate: true, reason: fmt.Sprintf("screening escalated to web_only (p=%.2f)", *j.Probability), runState: domain.RunSucceeded}
	}
	return result{reason: fmt.Sprintf("screening did not escalate (p=%.2f)", *j.Probability), runState: domain.RunSucceeded}
}

// finish 在一个事务里写回结论：清除 screening_pending；升级时升 risk_class、清默认值与超时 Timer。
// 事项已不在待初筛（已被恢复处理）时什么都不做；已被本人回答时只记结果，不改回答也不改类别。
func (s *Service) finish(ctx context.Context, id, runID string, res result) error {
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		it, err := items.Get(tx, id)
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.CodeNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if it.Ask == nil || !it.Ask.ScreeningPending {
			return nil
		}
		reason := res.reason
		answered := it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil
		escalate := res.escalate && !answered
		if answered {
			reason += "; the decision was already answered, result recorded only"
		}
		now := s.o.Now()
		_, _, snap, err := items.Revise(tx, id, "screening", now, func(it *domain.Item) error {
			a := *it.Ask
			a.ScreeningPending = false
			if escalate {
				a.RiskClass, a.DefaultOnTimeout = domain.RiskClassWebOnly, ""
			}
			it.Ask = &a
			return nil
		})
		if err != nil {
			return err
		}
		if escalate {
			if err := clock.Cancel(tx, decisions.TimerID(id)); err != nil && !(errors.As(err, &de) && de.Code == domain.CodeNotFound) {
				return err
			}
		}
		return s.record(tx, id, runID, reason, snap, res, now)
	})
}

func (s *Service) record(tx *store.Tx, id, runID, reason string, snap json.RawMessage, res result, now time.Time) error {
	if _, err := command.Bump(tx, id); err != nil {
		return err
	}
	depth, err := items.ChainDepth(tx, id)
	if err != nil {
		return err
	}
	ev := func(typ, why string, payload json.RawMessage) error {
		_, err := eventlog.Append(tx, domain.Event{Type: typ, Object: id, Actor: systemActor, Reason: why, Payload: payload, ChainDepth: depth, At: now})
		return err
	}
	if res.ownRunEvent {
		p := map[string]any{"run_id": runID, "category": domain.RunJudge, "template": Template, "state": res.runState}
		if res.failure != "" {
			p["error"] = res.failure
		}
		if err := ev(domain.EvRunFinished, "judge "+string(res.runState), mustJSON(p)); err != nil {
			return err
		}
	}
	if err := ev(domain.EvItemUpdated, reason, snap); err != nil {
		return err
	}
	if res.failure == "" {
		return nil
	}
	return ev(domain.EvHealthAlert, "web_only screening did not complete: "+res.failure,
		mustJSON(map[string]any{"kind": "screening", "template": Template, "run_id": runID, "reason": res.failure}))
}

// ---- 重启恢复 ----

// Recover 在内核启动时调用一次：上次进程在初筛返回前退出，screening_pending 就永远不会被清除。
// 对每个仍待初筛且不在本进程在途的事项按"中断"处理：按规则继续，不重试，Event 记原因，进健康视图。返回处理的事项数。
// shortcut: 全量扫描事项；事项量到十万级时给 screening_pending 加索引列。
func (s *Service) Recover(ctx context.Context) (int, error) {
	var ids []string
	if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		all, err := items.List(tx, items.Filter{})
		for _, it := range all {
			if it.Ask != nil && it.Ask.ScreeningPending {
				ids = append(ids, it.ID)
			}
		}
		return err
	}); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		s.mu.Lock()
		busy := s.inflight[id]
		s.mu.Unlock()
		if busy {
			continue
		}
		var runID string
		if err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) { runID, err = startedRun(tx, id); return }); err != nil {
			return n, err
		}
		res := result{reason: "screening interrupted by restart; continue by policy rules", failure: "interrupted", runState: domain.RunFailed, ownRunEvent: true}
		if err := s.finish(ctx, id, runID, res); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
