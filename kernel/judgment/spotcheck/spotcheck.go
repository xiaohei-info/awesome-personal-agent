// Package spotcheck 是抽检（设计 §8.10、§7.3）：从最近一个周期被初筛忽略的来源事件里取样，
// 本人逐条点"该看"或"确实不用"。
//
//   - 抽检池只含初筛定案为忽略（use=inbound_screen、answer=no、非影子、非把握低）、事件仍是 ignored、还没有结论的判断记录；
//     建议、规则忽略、其他用途的判断都不进池。池只读，CLI 与 API 列出的是同一批。
//   - "该看"：事件转为 proposed 事项（建议流可见），判断记录记 should_see 与抽检时间，这次判断计为误丢。
//   - "确实不用"：只在判断记录上记 correct 与抽检时间，事件仍是 ignored，不产生建议。
//   - 有简报实例时每份简报带 Policy.Judgment.SpotCheck.PerBrief 条抽检条目（Entries）；条目只带 id 与 Web 入口，结论仍只能在 Web 提交。
//
// 结论是策略类变更，命令按 change_settings 授权，只有 Web 本人会话通过（§14.8）。事项的起草由入站模块完成（Proposer），
// 本包不直写事项。
package spotcheck

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 抽检结论的取值（§6.16）。
const (
	ShouldSee = "should_see" // 该看：误丢
	Correct   = "correct"    // 确实不用
)

// CmdReview 填抽检结论。输入 {judgment, result, item?}：不带 item 的 should_see 只过授权与状态校验，不写入。
const CmdReview = "judgment.review"

// EvDisposed 是入站处置的审计事件类型（与 kernel/inbound 同值；本包不依赖入站包，由装配处的测试守住一致）。
const EvDisposed = "inbound.disposed"

// Window 是"最近一个周期"：判断时刻在此之内的初筛忽略才进池。
// shortcut: 固定 7 天，不随简报实例的周期变；实例周期差别大时再按实例参数取。
const Window = 7 * 24 * time.Hour

// UseInboundScreen 是入站初筛的判断用途。
const UseInboundScreen = "inbound_screen"

// Entry 是抽检池里的一条：判断记录加对应的来源事件。不含资料正文（摘要与详情）。
type Entry struct {
	Judgment string    `json:"judgment"`
	Event    string    `json:"event"`
	Source   string    `json:"source"`
	Subject  string    `json:"subject,omitempty"`
	Link     string    `json:"link,omitempty"`
	Template string    `json:"template"`
	JudgedAt time.Time `json:"judged_at"`
	WebPath  string    `json:"web_path"`
	Results  []string  `json:"results"`
}

// WebPath 是"判断与校准"页抽检区域里这一条的 Web 入口；API 路径同时是人点开的页面。
func webPath(judgment string) string { return "/judgments/spot-check?judgment=" + judgment }

// order 给记录一个稳定的伪随机次序：同一批记录在结论填入前，多次读取得到同一批（CLI、API、页面、简报一致）。
func order(id string) [32]byte { return sha256.Sum256([]byte(id)) }

// Pool 返回至多 limit 条待抽检条目；limit<=0 时返回空。now 决定窗口。
func Pool(tx *store.Tx, now time.Time, limit int) ([]Entry, error) {
	if limit <= 0 {
		return []Entry{}, nil
	}
	var cands []record.Record
	f := record.Filter{Use: UseInboundScreen, Since: now.Add(-Window), Limit: record.MaxLimit}
	for {
		page, more, err := record.List(tx, f)
		if err != nil {
			return nil, err
		}
		for _, r := range page {
			if r.Answer == "no" && !r.Shadow && !r.LowConfidence && r.Review == nil {
				cands = append(cands, r)
			}
		}
		if !more {
			break
		}
		f.After = page[len(page)-1].ID
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := order(cands[i].ID), order(cands[j].ID)
		return string(a[:]) < string(b[:])
	})
	out := []Entry{}
	for _, r := range cands {
		ev, err := runner.Event(tx, r.Subject)
		if err != nil {
			if e, ok := err.(*domain.Error); ok && e.Code == domain.CodeNotFound {
				continue
			}
			return nil, err
		}
		if ev.Disposition != domain.DispositionIgnored {
			continue
		}
		out = append(out, Entry{Judgment: r.ID, Event: ev.ID, Source: ev.SourceID, Subject: ev.Subject, Link: ev.Link, Template: r.Template,
			JudgedAt: r.CreatedAt, WebPath: webPath(r.ID), Results: []string{ShouldSee, Correct}})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ForBrief 返回一份简报该带的抽检条目：条数取策略的 spot_check.per_brief。
func ForBrief(tx *store.Tx, now time.Time) ([]Entry, error) {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return nil, err
	}
	return Pool(tx, now, pol.Judgment.SpotCheck.PerBrief)
}

// Proposer 为被忽略的来源事件起草建议事项（*inbound.Service 满足它）。
type Proposer interface {
	DraftProposal(ctx context.Context, id command.Identity, event string) (string, error)
}

type reviewIn struct {
	Judgment string `json:"judgment"`
	Result   string `json:"result"`
	Item     string `json:"item,omitempty"`
}

func parse(in json.RawMessage) (reviewIn, error) {
	var v reviewIn
	if err := json.Unmarshal(in, &v); err != nil || v.Judgment == "" {
		return v, domain.NewError(domain.CodeInvalidRequest, "review needs a judgment id", `send {"result": "should_see" | "correct"}`)
	}
	if v.Result != ShouldSee && v.Result != Correct {
		return v, domain.NewError(domain.CodeInvalidRequest, "result must be should_see or correct", `send {"result": "should_see" | "correct"}`)
	}
	return v, nil
}

// Register 登记 judgment.review。
func Register(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdReview, Action: authz.ActionChangeSettings,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			return authz.Object{ID: v.Judgment}, err
		},
		Validate: func(tx *store.Tx, o authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			r, err := record.Get(tx, o.ID)
			if err != nil {
				return err
			}
			if r.Use != UseInboundScreen || r.Answer != "no" || r.Shadow || r.LowConfidence {
				return domain.NewError(domain.CodeInvalidRequest, "judgment "+r.ID+" is not an ignored inbound screening", "only inbound screenings that ignored the event are spot-checked")
			}
			if r.Review != nil {
				return domain.NewError(domain.CodeConflict, "judgment "+r.ID+" is already reviewed", "a spot-check result is written once")
			}
			ev, err := runner.Event(tx, r.Subject)
			if err != nil {
				return err
			}
			if ev.Disposition != domain.DispositionIgnored {
				return domain.NewError(domain.CodeConflict, "source event "+ev.ID+" is "+string(ev.Disposition)+", not ignored", "re-read the spot-check pool")
			}
			if v.Result == Correct && v.Item != "" {
				return domain.NewError(domain.CodeInvalidRequest, "correct takes no item", "only should_see proposes an item")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			if v.Result == ShouldSee && v.Item == "" {
				return nil, nil // 授权与状态预检
			}
			r, err := record.Get(c.Tx, v.Judgment)
			if err != nil {
				return nil, err
			}
			now := c.Now()
			if v.Result == ShouldSee {
				if err := runner.ProposeIgnored(c.Tx, r.Subject, v.Item); err != nil {
					return nil, err
				}
				p, _ := json.Marshal(map[string]any{"event": r.Subject, "disposition": domain.DispositionProposed, "item": v.Item,
					"judgment": r.ID, "review": ShouldSee})
				if _, err := c.Emit(domain.Event{Type: EvDisposed, Object: r.Subject, Reason: "spot-check: the owner says this ignored event should have been seen", Payload: p}); err != nil {
					return nil, err
				}
			}
			return nil, record.SetReview(c.Tx, r.ID, v.Result, now)
		}})
}

// Service 把"该看"的两步（起草事项、落账）串起来。
type Service struct {
	Store    *store.Store
	Exec     *command.Executor
	Proposer Proposer
	Clock    func() time.Time // 空取 clock.Now
}

// Now 是池的"现在"。
func (s *Service) Now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return clock.Now()
}

// Review 以 id 的身份填抽检结论，返回更新后的判断记录。should_see 先过命令授权再起草事项
// （item.create 对任何凭据开放，授权放在后面会让被拒绝的调用者留下事项）；中途失败时事件仍是 ignored，重试接回已建事项。
func (s *Service) Review(ctx context.Context, id command.Identity, judgment, result, webURL string) (record.Record, error) {
	if s.Store == nil || s.Exec == nil || s.Proposer == nil {
		return record.Record{}, errors.New("spotcheck: Store, Exec and Proposer are required")
	}
	do := func(in reviewIn) error {
		b, _ := json.Marshal(in)
		_, err := s.Exec.Do(ctx, command.Request{Identity: id, Command: CmdReview, Input: b, Evidence: "spot-check", WebURL: webURL})
		return err
	}
	in := reviewIn{Judgment: judgment, Result: result}
	if err := do(in); err != nil { // should_see 不带 item 时是预检
		return record.Record{}, err
	}
	if result == ShouldSee {
		var event string
		if err := s.Store.View(ctx, func(tx *store.Tx) error {
			r, err := record.Get(tx, judgment)
			event = r.Subject
			return err
		}); err != nil {
			return record.Record{}, err
		}
		item, err := s.Proposer.DraftProposal(ctx, id, event)
		if err != nil {
			return record.Record{}, err
		}
		in.Item = item
		if err := do(in); err != nil {
			return record.Record{}, err
		}
	}
	var out record.Record
	err := s.Store.View(ctx, func(tx *store.Tx) (err error) { out, err = record.Get(tx, judgment); return })
	return out, err
}
