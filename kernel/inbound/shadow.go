package inbound

// 积压放开第 2 步：影子打分（§9.3、§8.10）。用判断端点给剩余积压按批打分，只写带 shadow 标记的判断记录，
// 不改处置、不建事项、不问对话端点；来源页与 CLI 按这个分数排序。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// CmdBacklogScore 是积压放开的第 2 步：影子打分。由本人显式触发（只接受本人原话）；本身只过授权与校验，
	// 判断记录由判断入口写，不经 Event 日志（§6.16）。
	CmdBacklogScore = "inbound.backlog_score"

	// scoreTemplate 是影子打分问的初筛题（§8.9）。
	// shortcut: 不跟随来源规则的 screen 覆盖；规则覆盖了初筛模板的来源，其积压分数仍按默认初筛题给出，需要时再按规则取模板。
	scoreTemplate = "triage-screen"
)

// Scorer 是判断入口的批量打分能力（*direct.Service 满足它）。
type Scorer interface {
	JudgeBatch(ctx context.Context, calls []direct.JudgeCall) ([]direct.Verdict, []error)
}

// ScoreResult 是影子打分的结果：写入记录的条数，以及没打上分的条数（被外发范围排除，或分类端点没给出概率）。
type ScoreResult struct {
	Scored  int `json:"scored"`
	Skipped int `json:"skipped"`
}

func noJudgeEndpoint() error {
	return domain.NewError(domain.CodeConflict, "no judge endpoint is configured, so the backlog cannot be scored",
		"set a judge endpoint in Settings first, or release the backlog with the chat endpoint (apa source backlog rejudge)")
}

func scoreSpec(s *Service) command.Spec {
	return command.Spec{Name: CmdBacklogScore, Action: authz.ActionManageSource, Tier: authz.TierWords,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			_, err := parseRules(in)
			return authz.Object{}, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, _ json.RawMessage) error {
			if _, ok := s.o.Judge.(Scorer); !ok {
				return errors.New("inbound: the judge cannot score in batches")
			}
			_, pol, err := policy.Current(tx)
			if err == nil && (pol.JudgeEndpoint == nil || pol.JudgeEndpoint.URL == "") {
				err = noJudgeEndpoint()
			}
			return err
		},
		Apply: func(*command.Ctx) (any, error) { return nil, nil }}
}

// BacklogScore 给积压（unjudged、expired，含旧原因与 restored）逐条影子打分。source 非空时只看该来源。
// 外发范围照常裁剪：本事件的内容不可送给判断端点的位置时跳过（记入 Skipped），不送出。
// 重复执行时输入没变的题复用上次的分数（§8.5），不再外呼，但每次仍记一条新记录。
func (s *Service) BacklogScore(ctx context.Context, req command.Request, source string) (ScoreResult, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	in, _ := json.Marshal(rulesIn{Source: source})
	r := req
	r.Command, r.Input, r.IdemKey = CmdBacklogScore, in, ""
	if _, err := s.o.Exec.Do(ctx, r); err != nil {
		return ScoreResult{}, err
	}
	sc, _ := s.o.Judge.(Scorer) // Validate 已确认

	var (
		sn  snapshot
		evs []domain.SourceEvent
		ep  policy.JudgeEndpoint
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		if pol.JudgeEndpoint == nil || pol.JudgeEndpoint.URL == "" { // 校验之后端点被撤掉
			return noJudgeEndpoint()
		}
		ep = *pol.JudgeEndpoint
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
		return ScoreResult{}, err
	}

	var (
		res   ScoreResult
		errs  []error
		calls []direct.JudgeCall
	)
	loc := policy.EffectiveLocality(ep.Locality)
	for _, ev := range evs {
		src, err := sn.source(ev.SourceID)
		if err != nil {
			errs = append(errs, fmt.Errorf("inbound: backlog score %s: %w", ev.ID, err))
			res.Skipped++
			continue
		}
		dec := registry.Decide(src.Outbound, loc)
		if dec.Block != "" {
			res.Skipped++
			continue
		}
		t, _ := registry.Trim(ev, dec.Scopes[0])
		pack := DefaultPack
		if m, _ := s.match(src, ev); m != nil {
			pack = m.pack
		}
		// 输入与资料的形状同入站初筛一致，使放开重判时输入指纹相同、复用这里的分数。
		input, _ := json.Marshal(map[string]any{"source": src.ID, "source_type": src.SourceKind, "event_type": t.Type,
			"subject": t.Subject, "summary": t.Summary, "link": t.Link, "sender": t.Sender, "sender_verified": ev.SenderVerified,
			"occurred_at": t.OccurredAt, "correlation_hint": ""})
		calls = append(calls, direct.JudgeCall{Pack: pack, Template: scoreTemplate, Input: input, MaterialText: t.Detail,
			Subject: ev.ID, Source: src.ID, Scope: direct.Scope{Source: src.ID}, ScoreOnly: true})
	}
	if len(calls) == 0 {
		return res, errors.Join(errs...)
	}
	vs, jerrs := sc.JudgeBatch(ctx, calls)
	failed := map[string]int{} // 分类端点的错误码 → 没打上分的题数
	for i, v := range vs {
		switch {
		case jerrs[i] != nil:
			errs = append(errs, fmt.Errorf("inbound: backlog score %s: %w", calls[i].Subject, jerrs[i]))
			res.Skipped++
		case v.Written:
			res.Scored++
		default:
			res.Skipped++
			if ls := v.Record.Levels; len(ls) > 0 && ls[0].Outcome == record.OutcomeError {
				failed[ls[0].ErrorCode]++
			}
		}
	}
	for code, n := range failed {
		errs = append(errs, fmt.Errorf("inbound: judge endpoint could not score %d events (%s)", n, code))
	}
	return res, errors.Join(errs...)
}

// Scored 是带影子分数的积压事件；没有分数时 Score 为空。
type Scored struct {
	domain.SourceEvent
	Score *float64 `json:"score,omitempty"`
}

// SortByScore 给事件附上最近一次的初筛分数（分类端点的 p_yes），并按分数从高到低排；同分与没有分数的保持原顺序，没有分数的排最后。
// shortcut: 每个事件读一次它的判断记录；积压到数万条拖慢列表时，给判断记录加按（用途，对象）取最新一条的索引。
func SortByScore(tx *store.Tx, evs []domain.SourceEvent) ([]Scored, error) {
	out := make([]Scored, len(evs))
	for i, ev := range evs {
		out[i].SourceEvent = ev
		rs, _, err := record.List(tx, record.Filter{Subject: ev.ID, Use: "inbound_screen", Limit: record.MaxLimit})
		if err != nil {
			return nil, err
		}
		for _, r := range rs { // 升序：最后一条有分数的为准
			for _, l := range r.Levels {
				if l.Backend == record.BackendClassifier && l.PYes != nil {
					p := *l.PYes
					out[i].Score = &p
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Score, out[j].Score
		return a != nil && (b == nil || *a > *b)
	})
	return out, nil
}
