package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdAttach 是建议的第三种处置：补到已有事项（§7.2）。
const CmdAttach = "proposal.attach"

type attachIn struct {
	ID     string `json:"id"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	// Evidence 随 HTTP 体到达时是请求元数据（本人原话），命令管线另行读取；这里只接受不使用。
	Evidence string `json:"evidence,omitempty"`
}

func parseAttach(in json.RawMessage) (attachIn, error) {
	var v attachIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, domain.NewError(domain.CodeInvalidRequest, "invalid input: "+err.Error(), "send {id, to, reason?}")
	}
	return v, nil
}

// RegisterAttach 登记 proposal.attach：建议的来源事件改挂到目标事项并成为它的挂靠消息，建议的关联键登记到目标
// （此后同一线程的消息直接回到那里），标题与摘要作为一条进展记在目标上，建议转 dropped 并与目标建立 relates 关系。
// 全部写入在一个事务里：目标不合法时什么都不改。档位同 proposal.accept、proposal.dismiss（二档，§14.3）。
// 同时登记批量用到的 proposal.merge 与 proposal.batch_log（registerBulk）。
func RegisterAttach(x *command.Executor) error {
	if err := registerBulk(x); err != nil {
		return err
	}
	return x.Register(command.Spec{Name: CmdAttach, Action: authz.ActionAnswerDecision, Group: authz.GroupDecide, Page: "/proposals",
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseAttach(in)
			if err != nil {
				return authz.Object{}, err
			}
			it, err := items.Get(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			return authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Project: it.Project}, nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parseAttach(in)
			_, err := attachTarget(tx, v)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseAttach(c.Input)
			target, err := attachTarget(c.Tx, v)
			if err != nil {
				return nil, err
			}
			state := runner.AttachProcessed
			if target.Kind == domain.KindDelegation {
				state = runner.AttachPending // 委派事项的 agent 通过回执取走新消息（§9.4）
			}
			msgs, err := runner.Reattach(c.Tx, v.ID, v.To, state)
			if err != nil {
				return nil, err
			}
			var (
				keys []items.Correlation
				ids  []string
			)
			for _, m := range msgs {
				ids = append(ids, m.ID)
				for _, k := range m.CorrelationKeys {
					if x := (items.Correlation{Domain: m.DedupeKey.Domain, Key: k}); !slices.Contains(keys, x) {
						keys = append(keys, x)
					}
				}
			}
			ws, err := items.Absorb(c.Tx, v.ID, v.To, keys, ids, c.Caller.Actor.Principal, c.Now())
			if err != nil {
				return nil, err
			}
			reason := "attached to " + v.To
			if strings.TrimSpace(v.Reason) != "" {
				reason += ": " + v.Reason
			}
			for _, w := range ws {
				e := domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload}
				switch {
				case w.Object == v.ID:
					e.Reason, e.ResolvedVia = reason, attachVia(c)
				case w.Type == domain.EvProposalAttached:
					e.Reason, e.ResolvedVia = reason, attachVia(c)
					fallthrough
				default: // 目标的每条事项事件各推进一次版本
					if _, err := command.Bump(c.Tx, w.Object); err != nil {
						return nil, err
					}
				}
				if _, err := c.Emit(e); err != nil {
					return nil, err
				}
			}
			return struct {
				ID string `json:"id"`
			}{v.ID}, nil
		}})
}

// attachVia 同 proposal.accept 的 resolved_via：Web 上的本人记 web，其余（档位放开的 Agent）记 agent。
func attachVia(c *command.Ctx) string {
	if c.Caller.Level == authz.LevelWeb {
		return "web"
	}
	return "agent"
}

// attachTarget 校验补到的目标：建议仍是 proposed；目标存在（404）、未关闭、不在 proposed（409）。
func attachTarget(tx *store.Tx, v attachIn) (domain.Item, error) {
	var zero domain.Item
	if strings.TrimSpace(v.To) == "" {
		return zero, domain.NewError(domain.CodeInvalidRequest, "to is required", "give the item the proposal should be attached to")
	}
	prop, err := items.Get(tx, v.ID)
	if err != nil {
		return zero, err
	}
	if prop.Phase != domain.PhaseProposed {
		return zero, domain.NewError(domain.CodeConflict, "the item is not a pending proposal", "re-read the item; the first answer stands")
	}
	if v.To == v.ID {
		return zero, domain.NewError(domain.CodeInvalidRequest, "a proposal cannot be attached to itself", "choose another item")
	}
	target, err := items.Get(tx, v.To)
	if err != nil {
		return zero, err
	}
	switch {
	case target.Phase.Terminal():
		return zero, domain.NewError(domain.CodeConflict, "target item "+v.To+" is closed", "choose an open item, or accept the proposal as a new item")
	case target.Phase == domain.PhaseProposed:
		return zero, domain.NewError(domain.CodeConflict, "target item "+v.To+" is itself a pending proposal", "accept it first, or choose an open item")
	}
	return target, nil
}

// ---- 批量处理（§7.2 proposal.batch） ----

// MaxBatch 是一次批量至多处理的建议条数。
const MaxBatch = 200

// 批量的五种方式。ended：忽略 Filter.IDs 里上游对象可核为已结束的建议，依据（来源事件）逐条写进忽略理由；拿不准的不动。
// selected：对 Filter.IDs 一律接受或忽略（见 bulk.go）。undo：按批撤销（见 undoBatch）。
const (
	BatchRecommended = "recommended"
	BatchExplicit    = "explicit"
	BatchEnded       = "ended"
	BatchUndo        = "undo"
)

// BatchFilter 限定按推荐处理的范围；按推荐处理必须带范围（ids、source、project 至少一项），都空不等于“全部”，直接拒绝。
type BatchFilter struct {
	Source  string   `json:"source,omitempty"`
	Project string   `json:"project,omitempty"`
	IDs     []string `json:"ids,omitempty"`
}

// BatchAction 是处置清单里的一条。
type BatchAction struct {
	ID     string `json:"id"`
	Do     string `json:"do"` // accept | attach | dismiss
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// BatchRequest：Mode=recommended 配 Filter；Mode=explicit 配 Actions；Mode=ended 配 Filter.IDs；
// Mode=selected 配 Do、Filter.IDs 与可选的 Reason（只配 dismiss）；Mode=undo 配 Batch。
type BatchRequest struct {
	Mode    string        `json:"mode"`
	Filter  BatchFilter   `json:"filter,omitzero"`
	Actions []BatchAction `json:"actions,omitempty"`
	Batch   string        `json:"batch,omitempty"`
	Do      string        `json:"do,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

// BatchResult 是一条建议的处理结果。失败时 Code 与 Why 说明原因，建议不变；事先或执行中判定不处理的另有 Skip（原因码）。
// Batch 是这次批量的批次号（忽略、接受、合并与撤销的结果带）；Do 为 merge 的是合进了 Into 那一条；
// UndoUntil 是接受的撤销窗口截止（只在接受成功的结果上）。
type BatchResult struct {
	ID        string      `json:"id"`
	Do        string      `json:"do"`
	OK        bool        `json:"ok"`
	Code      domain.Code `json:"code,omitempty"`
	Why       string      `json:"why,omitempty"`
	Skip      string      `json:"skip,omitempty"`
	Into      string      `json:"into,omitempty"`
	Batch     string      `json:"batch,omitempty"`
	UndoUntil *time.Time  `json:"undo_until,omitempty"`
}

func usage(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "see the batch format in the API reference")
}

// Batch 逐条处理建议：批量等于逐条，没有整批的门（§14.3）。形态错误、超过 MaxBatch 时整批不生效；
// 其余按条各自成败，每条成功的处置各是一条命令、各记一条 Event、可逐条撤销。req 带调用者身份，
// Command、Input 由本函数按每条填写；req.IdemKey 非空时每条派生自己的幂等键。
//
// 每次批量（含按批撤销）另记一条审计事件（proposal.batch_handled / proposal.batch_undone，对象是批次号）。
func Batch(ctx context.Context, x *command.Executor, st *store.Store, req command.Request, b BatchRequest) ([]BatchResult, error) {
	switch b.Mode {
	case BatchSelected:
		return selected(ctx, x, st, req, b)
	case BatchUndo:
		return undoBatch(ctx, x, st, req, b)
	}
	if b.Do != "" || b.Reason != "" {
		return nil, usage("do and reason belong to selected mode")
	}
	acts, skipped, err := planBatch(ctx, st, b)
	if err != nil {
		return nil, err
	}
	out := make([]BatchResult, 0, len(acts)+len(skipped))
	batch := ""
	if len(acts) > 0 {
		batch = newBatch(req, acts[0].ID, clock.Now())
	}
	tag := func(a BatchAction) string {
		if a.Do == "dismiss" || a.Do == "accept" {
			return batch
		}
		return ""
	}
	var denied error
	for _, a := range acts {
		r := req
		r.Command, r.Input = batchCommand(a, tag(a))
		if req.IdemKey != "" {
			r.IdemKey = req.IdemKey + "#" + a.ID
		}
		_, err := x.Do(ctx, r)
		if err == nil {
			res := BatchResult{ID: a.ID, Do: a.Do, OK: true, Batch: tag(a)}
			if a.Do == "accept" {
				until := x.Now().Add(UndoWindow)
				res.UndoUntil = &until
			}
			out = append(out, res)
			continue
		}
		var de *domain.Error
		if !errors.As(err, &de) {
			return out, err
		}
		if de.Code == domain.CodePermissionDenied && denied == nil {
			denied = err
		}
		out = append(out, BatchResult{ID: a.ID, Do: a.Do, Code: de.Code, Why: de.Why, Batch: tag(a)})
	}
	// 钥匙范围不够：每一条都被拒绝说明整批都不会生效。
	if denied != nil && !slices.ContainsFunc(out, func(r BatchResult) bool { return r.Code != domain.CodePermissionDenied }) {
		return nil, denied
	}
	out = append(out, skipped...)
	if batch == "" {
		return out, nil // 一条都没执行：没有批次，不记审计
	}
	return out, logBatch(ctx, x, st, req, batchLog{Batch: batch, Mode: b.Mode, Requested: len(acts) + len(skipped), Results: out, UndoUntil: undoUntil(out)})
}

var batchRe = regexp.MustCompile(`^pb_[0-9a-f]{12}$`)

var batchVerbs = map[string]string{"accept": "proposal.accept", "dismiss": "proposal.dismiss", "attach": CmdAttach}

func batchCommand(a BatchAction, batch string) (string, json.RawMessage) {
	m := map[string]string{"id": a.ID}
	if batch != "" {
		m["batch"] = batch
	}
	if a.Reason != "" {
		m["reason"] = a.Reason
	}
	if a.Do == "attach" {
		m["to"] = a.To
	}
	in, _ := json.Marshal(m)
	return batchVerbs[a.Do], in
}

// planBatch 返回要执行的处置，以及事先就判定不处理的那几条（只有 ended 方式会有）。
func planBatch(ctx context.Context, st *store.Store, b BatchRequest) ([]BatchAction, []BatchResult, error) {
	switch b.Mode {
	case BatchExplicit:
		if len(b.Actions) == 0 || len(b.Actions) > MaxBatch {
			return nil, nil, usage("explicit mode takes 1 to 200 actions")
		}
		if b.Filter.Source != "" || b.Filter.Project != "" || len(b.Filter.IDs) > 0 {
			return nil, nil, usage("filter belongs to recommended mode")
		}
		for _, a := range b.Actions {
			if _, ok := batchVerbs[a.Do]; !ok || strings.TrimSpace(a.ID) == "" {
				return nil, nil, usage("every action needs an id and do of accept, attach or dismiss")
			}
		}
		return b.Actions, nil, nil
	case BatchRecommended:
		if len(b.Actions) > 0 {
			return nil, nil, usage("actions belong to explicit mode")
		}
		// 没带范围不能当成“处理全部”：调用方的可见集合为空时也会走到这里，必须拒绝而不是放大。
		if f := b.Filter; f.IDs != nil && len(f.IDs) == 0 {
			return nil, nil, usage("filter.ids is empty: nothing is in scope, so nothing was handled")
		} else if f.IDs == nil && f.Source == "" && f.Project == "" {
			return nil, nil, usage("recommended mode needs an explicit scope (filter.ids, source or project); it never means all proposals")
		}
		var acts []BatchAction
		err := st.View(ctx, func(tx *store.Tx) (err error) { acts, err = recommended(tx, b.Filter); return })
		if err == nil && len(acts) > MaxBatch {
			err = usage("at most 200 proposals per batch; narrow the filter")
		}
		return acts, nil, err
	case BatchEnded:
		if len(b.Actions) > 0 || b.Filter.Source != "" || b.Filter.Project != "" {
			return nil, nil, usage("ended mode takes filter.ids only")
		}
		if len(b.Filter.IDs) == 0 || len(b.Filter.IDs) > MaxBatch {
			return nil, nil, usage("ended mode takes 1 to 200 ids")
		}
		var (
			acts    []BatchAction
			skipped []BatchResult
		)
		err := st.View(ctx, func(tx *store.Tx) error {
			var its []domain.Item
			for _, id := range b.Filter.IDs {
				it, err := items.Get(tx, id)
				if err != nil {
					var de *domain.Error
					if !errors.As(err, &de) {
						return err
					}
					skipped = append(skipped, BatchResult{ID: id, Do: "dismiss", Code: de.Code, Why: de.Why})
					continue
				}
				its = append(its, it)
			}
			os, err := Origins(tx, its)
			for _, it := range its {
				// 依据只看上游对象，与建议现在的阶段无关：同一幂等键重放时照样得到同一条处置，由命令的幂等记录返回第一次的结果。
				if o := os[it.ID]; o.Ended != nil {
					acts = append(acts, BatchAction{ID: it.ID, Do: "dismiss", Reason: "upstream " + o.Object + " ended: " + o.Ended.Type + " " + o.Ended.Event + " at " + o.Ended.At.Format(time.RFC3339)})
				} else {
					skipped = append(skipped, BatchResult{ID: it.ID, Do: "dismiss", Code: domain.CodeConflict, Why: "the upstream object is not verifiably merged or closed; left as is"})
				}
			}
			return err
		})
		return acts, skipped, err
	}
	return nil, nil, usage("mode must be recommended, explicit, ended, selected or undo")
}

// recommended 把范围内带推荐的建议翻成处置：忽略取推荐理由，补到取推荐的目标。没有推荐的不动。
func recommended(tx *store.Tx, f BatchFilter) ([]BatchAction, error) {
	props, err := items.List(tx, items.Filter{Phase: domain.PhaseProposed, Project: f.Project})
	if err != nil {
		return nil, err
	}
	var out []BatchAction
	for _, p := range props {
		if len(f.IDs) > 0 && !slices.Contains(f.IDs, p.ID) {
			continue
		}
		rec, ok, err := items.RecommendationOf(tx, p.ID)
		if err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		if f.Source != "" {
			from, err := fromSource(tx, p.ID, f.Source)
			if err != nil {
				return nil, err
			}
			if !from {
				continue
			}
		}
		a := BatchAction{ID: p.ID, Do: rec.Recommend, To: rec.AttachTo}
		if rec.Recommend == "dismiss" {
			a.Reason = rec.Reason
		}
		out = append(out, a)
	}
	return out, nil
}

func fromSource(tx *store.Tx, item, source string) (bool, error) {
	r, err := tx.Query(`SELECT 1 FROM sources_runner_event WHERE item = ? AND source_id = ? AND disposition = ? LIMIT 1`,
		item, source, string(domain.DispositionProposed))
	if err != nil {
		return false, err
	}
	defer r.Close()
	return r.Next(), r.Err()
}
