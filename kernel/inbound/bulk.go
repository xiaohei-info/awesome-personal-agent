package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 按选中批量接受或忽略、审计与按批撤销（§7.2） ----

// BatchSelected：对 Filter.IDs 里的建议一律做 Do（accept 或 dismiss）。接受时合并键相同的几条先合成一条再接受（只建一个事项），
// 本人拆开过、跨项目的不合；需要本人拍板的不进批量；已不是建议的跳过。
const BatchSelected = "selected"

// UndoWindow 是批量接受的撤销窗口：接受后这么久之内、事项此后没有别的改动，按批撤销才把它（连同合进来的几条）改回建议。
// 忽略的撤销不限时（建议页的"已忽略"按批列出，§7.2）。
const UndoWindow = 30 * time.Minute

// 跳过的原因码（BatchResult.Skip）：稳定取值，Web 按词表渲染。
const (
	SkipHandled     = "handled"      // 已不是建议：别处先处理了（含批量执行期间）
	SkipHandledHere = "handled_here" // 这一批已经处理过：同一批重复提交
	SkipNeedsYou    = "needs_you"    // 需要本人逐条拍板，不进批量
	SkipNotFound    = "not_found"
	SkipChanged     = "changed" // 撤销：之后在别处改过，不动
	SkipExpired     = "expired" // 撤销：过了撤销窗口
)

const (
	// CmdMergeProposal 把一条建议合进另一条建议（批量接受时按同一对象合并）：复用批量归类的折叠，可按批撤销，也可在建议上逐条撤销。
	CmdMergeProposal = "proposal.merge"
	// CmdBatchLog 记一次批量的审计事件；只由内核在批量结束时以内核身份写，载荷记谁发起的。
	CmdBatchLog = "proposal.batch_log"
)

type mergeIn struct {
	ID    string `json:"id"`
	Into  string `json:"into"`
	Batch string `json:"batch"`
}

func parseMerge(in json.RawMessage) (mergeIn, error) {
	var v mergeIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil || v.ID == "" || v.Into == "" || !batchRe.MatchString(v.Batch) {
		return v, domain.NewError(domain.CodeInvalidRequest, "invalid input", "send {id, into, batch}")
	}
	return v, nil
}

// mergeTarget 校验合并：两条都仍是建议、不是同一条、去处没被折叠，且不被 veto 拦下（跨项目、同仓库不同 PR、本人拆开过）。
func mergeTarget(tx *store.Tx, v mergeIn) (it, t domain.Item, err error) {
	if v.ID == v.Into {
		return it, t, domain.NewError(domain.CodeInvalidRequest, "a proposal cannot be merged into itself", "choose another proposal")
	}
	if it, err = items.Get(tx, v.ID); err != nil {
		return
	}
	if t, err = items.Get(tx, v.Into); err != nil {
		return
	}
	switch {
	case it.Phase != domain.PhaseProposed:
		return it, t, domain.NewError(domain.CodeConflict, "the item is not a pending proposal", "re-read the item; the first answer stands")
	case t.Phase != domain.PhaseProposed:
		return it, t, domain.NewError(domain.CodeConflict, "target "+v.Into+" is not a pending proposal", "re-read the target")
	}
	if code, _ := veto(it, t); code != "" {
		return it, t, domain.NewError(domain.CodeConflict, "these two proposals may not be merged: "+code, "accept them one by one")
	}
	return it, t, nil
}

// registerBulk 登记 proposal.merge（二档，同 proposal.accept）与 proposal.batch_log（系统内部）。
func registerBulk(x *command.Executor) error {
	err := x.Register(command.Spec{Name: CmdMergeProposal, Action: authz.ActionAnswerDecision, Tier: authz.TierDecide, Page: "/proposals", KeepVersion: true,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseMerge(in)
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
			v, _ := parseMerge(in)
			_, _, err := mergeTarget(tx, v)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseMerge(c.Input)
			it, t, err := mergeTarget(c.Tx, v)
			if err != nil {
				return nil, err
			}
			at := c.Now()
			sw := domain.ProposalSweep{State: "done", Batch: v.Batch, Action: domain.SweepMerge, Target: t.ID, By: domain.SweepByBulk, At: &at, Prev: it.Proposal.Sweep}
			p := domain.Proposal{}
			if it.Proposal != nil {
				p = *it.Proposal
			}
			p.Sweep = &sw
			it.Proposal = &p
			moved, err := moveHeld(c, it, t.ID, p.Sources, sw, 0, "batch "+v.Batch+": merged "+it.ID+" in")
			if err != nil {
				return nil, err
			}
			return map[string]string{"id": it.ID, "into": t.ID}, foldInto(c, it, t.ID, p, moved, at, v.Batch, "batch "+v.Batch+": merged into "+t.ID)
		}})
	if err != nil {
		return err
	}
	return x.Register(command.Spec{Name: CmdBatchLog, Action: authz.ActionUpdateOwn, Tier: authz.TierInternal,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v batchLog
			if err := json.Unmarshal(in, &v); err != nil || !batchRe.MatchString(v.Batch) {
				return authz.Object{}, invalid("batch log needs a batch")
			}
			return authz.Object{ID: v.Batch, Assignee: systemPrinc}, nil
		},
		Validate: func(tx *store.Tx, o authz.Object, in json.RawMessage) error {
			var v batchLog
			_ = json.Unmarshal(in, &v)
			if v.Do == "undo" {
				return nil
			}
			if l, err := readBatchLog(tx, o.ID); err != nil || l != nil {
				return cmpErr(err, domain.NewError(domain.CodeConflict, "batch "+o.ID+" is already recorded", "nothing to do"))
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			var v batchLog
			_ = json.Unmarshal(c.Input, &v)
			typ := domain.EvProposalBatchHandled
			if v.Do == "undo" {
				typ = domain.EvProposalBatchUndone
			}
			n := 0
			for _, r := range v.Results {
				if r.OK {
					n++
				}
			}
			_, err := c.Emit(domain.Event{Type: typ, Object: v.Batch, Payload: c.Input,
				Reason: fmt.Sprintf("batch %s %s: %d of %d handled", v.Mode, v.Do, n, len(v.Results))})
			return nil, err
		}})
}

func cmpErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

// batchLog 是审计事件的载荷：哪一批、方式、动作（selected 与 undo 才有单一动作，其余方式逐条见 Results）、谁发起、理由、
// 请求了几条、逐条结果、撤销窗口截止。
type batchLog struct {
	Batch     string        `json:"batch"`
	Mode      string        `json:"mode"`
	Do        string        `json:"do,omitempty"`
	By        domain.Actor  `json:"by"`
	Reason    string        `json:"reason,omitempty"`
	Requested int           `json:"requested"`
	Results   []BatchResult `json:"results"`
	UndoUntil *time.Time    `json:"undo_until,omitempty"`
	At        time.Time     `json:"-"`
}

// readBatchLog 读这一批的审计事件（proposal.batch_handled），没有为 nil。
func readBatchLog(tx *store.Tx, batch string) (*batchLog, error) {
	es, err := eventlog.ReadObject(tx, batch)
	if err != nil {
		return nil, err
	}
	for _, en := range es {
		if en.Event.Type != domain.EvProposalBatchHandled || en.Tombstone {
			continue
		}
		var l batchLog
		if err := json.Unmarshal(en.Event.Payload, &l); err != nil {
			return nil, err
		}
		l.At = en.Event.At
		return &l, nil
	}
	return nil, nil
}

// logBatch 以内核身份记一次批量的审计事件，载荷的 by 是发起者。同一批（同一幂等键重发）只记一次。
func logBatch(ctx context.Context, x *command.Executor, st *store.Store, req command.Request, l batchLog) error {
	if err := st.View(ctx, func(tx *store.Tx) error {
		c, err := req.Identity.Authenticate(tx)
		l.By = c.Actor
		return err
	}); err != nil {
		return err
	}
	in, _ := json.Marshal(l)
	_, err := x.Do(ctx, command.Request{Identity: system, Command: CmdBatchLog, Input: in, Owner: req.Owner})
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeConflict && l.Do != "undo" {
		return nil // 重发：这一批已经记过
	}
	return err
}

// stillProposed 读事项现在是否仍是建议。
func stillProposed(ctx context.Context, st *store.Store, id string) (bool, error) {
	var ok bool
	err := st.View(ctx, func(tx *store.Tx) error {
		it, err := items.Get(tx, id)
		ok = err == nil && it.Phase == domain.PhaseProposed
		var de *domain.Error
		if errors.As(err, &de) {
			return nil
		}
		return err
	})
	return ok, err
}

// selected 执行 BatchSelected。批量等于逐条：每一条（含合并）各是一条命令、按它自己的档位判定，没有整批的门。
func selected(ctx context.Context, x *command.Executor, st *store.Store, req command.Request, b BatchRequest) ([]BatchResult, error) {
	ids := b.Filter.IDs
	switch {
	case b.Do != "accept" && b.Do != "dismiss":
		return nil, usage("selected mode takes do of accept or dismiss")
	case len(ids) == 0 || len(ids) > MaxBatch:
		return nil, usage("selected mode takes 1 to 200 ids")
	case len(b.Actions) > 0 || b.Filter.Source != "" || b.Filter.Project != "" || b.Batch != "":
		return nil, usage("selected mode takes do, filter.ids and reason only")
	case b.Reason != "" && b.Do != "dismiss":
		return nil, usage("reason belongs to dismiss")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return nil, usage("filter.ids must be distinct, non-empty ids")
		}
		seen[id] = true
	}
	batch := newBatch(req, ids[0], clock.Now())
	var (
		out   []BatchResult
		parts [][]domain.Item // 每份接受后是一个事项：首条留下，其余先合进它；忽略时每份一条
	)
	if err := st.View(ctx, func(tx *store.Tx) error {
		var live []domain.Item
		for _, id := range ids {
			it, err := items.Get(tx, id)
			var de *domain.Error
			switch {
			case errors.As(err, &de) && de.Code == domain.CodeNotFound:
				out = append(out, BatchResult{ID: id, Do: b.Do, Skip: SkipNotFound, Batch: batch})
				continue
			case err != nil:
				return err
			case it.Phase != domain.PhaseProposed:
				_, last, err := items.LastChange(tx, id)
				if err != nil {
					return err
				}
				skip := SkipHandled
				if last == batch {
					skip = SkipHandledHere
				}
				out = append(out, BatchResult{ID: id, Do: b.Do, Skip: skip, Batch: batch})
				continue
			case NeedsYou(it):
				out = append(out, BatchResult{ID: id, Do: b.Do, Skip: SkipNeedsYou, Batch: batch})
				continue
			}
			live = append(live, it)
		}
		if b.Do == "dismiss" {
			for _, it := range live {
				parts = append(parts, []domain.Item{it})
			}
			return nil
		}
		for _, u := range units(live) {
			parts = append(parts, split(u)...)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	run := func(cmd, key string, in any) error {
		r := req
		r.Command = cmd
		r.Input, _ = json.Marshal(in)
		if req.IdemKey != "" {
			r.IdemKey = req.IdemKey + "#" + key
		}
		_, err := x.Do(ctx, r)
		return err
	}
	// failed 把一条命令的失败记成结果：这一条已不是建议的，按"别处先处理了"跳过；其余带错误码。非领域错误原样返回。
	failed := func(id, do string, err error) (BatchResult, error) {
		var de *domain.Error
		if !errors.As(err, &de) {
			return BatchResult{}, err
		}
		r := BatchResult{ID: id, Do: do, Code: de.Code, Why: de.Why, Batch: batch}
		if ok, err := stillProposed(ctx, st, id); err != nil {
			return r, err
		} else if !ok && de.Code == domain.CodeConflict {
			r.Skip = SkipHandled
		}
		return r, nil
	}
	var acceptPart func(part []domain.Item) error
	// acceptPart：其余几条先合进首条再接受首条。首条在执行中被别处处理了，已合进去的撤回，余下几条换下一条作首条重来。
	acceptPart = func(part []domain.Item) error {
		if len(part) == 0 {
			return nil
		}
		c := part[0]
		var folded []domain.Item
		undo := func() error {
			for _, m := range folded {
				if err := run(CmdUnmerge, m.ID+"#unfold#"+c.ID, unmergeIn{Item: m.ID, Batch: batch}); err != nil {
					return err
				}
			}
			return nil
		}
		for i, m := range part[1:] {
			err := run(CmdMergeProposal, m.ID+"#into#"+c.ID, mergeIn{ID: m.ID, Into: c.ID, Batch: batch})
			if err == nil {
				folded = append(folded, m)
				continue
			}
			if ok, e := stillProposed(ctx, st, c.ID); e != nil {
				return e
			} else if !ok { // 首条没了
				if err := undo(); err != nil {
					return err
				}
				out = append(out, BatchResult{ID: c.ID, Do: "accept", Skip: SkipHandled, Batch: batch})
				return acceptPart(append(folded, part[1+i:]...))
			}
			r, err := failed(m.ID, "merge", err)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		err := run("proposal.accept", c.ID, map[string]string{"id": c.ID, "batch": batch})
		if err == nil {
			until := x.Now().Add(UndoWindow)
			out = append(out, BatchResult{ID: c.ID, Do: "accept", OK: true, Batch: batch, UndoUntil: &until})
			for _, m := range folded {
				out = append(out, BatchResult{ID: m.ID, Do: "merge", OK: true, Into: c.ID, Batch: batch, UndoUntil: &until})
			}
			return nil
		}
		r, ferr := failed(c.ID, "accept", err)
		if ferr != nil {
			return ferr
		}
		if err := undo(); err != nil {
			return err
		}
		out = append(out, r)
		if r.Skip == SkipHandled {
			return acceptPart(folded)
		}
		for _, m := range folded { // 首条接受被拒（如档位）：合进去的已撤回，同样记为没处理
			out = append(out, BatchResult{ID: m.ID, Do: "merge", Code: r.Code, Why: r.Why, Batch: batch})
		}
		return nil
	}
	for _, part := range parts {
		if b.Do == "accept" {
			if err := acceptPart(part); err != nil {
				return out, err
			}
			continue
		}
		in := map[string]string{"id": part[0].ID, "batch": batch}
		if b.Reason != "" {
			in["reason"] = b.Reason
		}
		if err := run("proposal.dismiss", part[0].ID, in); err != nil {
			r, err := failed(part[0].ID, "dismiss", err)
			if err != nil {
				return out, err
			}
			out = append(out, r)
			continue
		}
		out = append(out, BatchResult{ID: part[0].ID, Do: "dismiss", OK: true, Batch: batch})
	}
	if allDenied(out) {
		return nil, domain.NewError(domain.CodePermissionDenied, out[0].Why, "nothing was handled")
	}
	l := batchLog{Batch: batch, Mode: BatchSelected, Do: b.Do, Reason: b.Reason, Requested: len(ids), Results: out}
	if until := undoUntil(out); until != nil {
		l.UndoUntil = until
	}
	return out, logBatch(ctx, x, st, req, l)
}

// split 把一个合并键单元拆成接受时各成一个事项的几份（含只有一条的）：每一条进第一份与其中每条都不被 veto 拦下
// （本人拆开过、跨项目、同仓库不同 PR）的，没有就另起一份。每份按建立先后，首条是留下的那条。
func split(unit []domain.Item) [][]domain.Item {
	var parts [][]domain.Item
	for _, it := range unit {
		i := slices.IndexFunc(parts, func(p []domain.Item) bool {
			return !slices.ContainsFunc(p, func(x domain.Item) bool { c, _ := veto(it, x); return c != "" })
		})
		if i < 0 {
			parts = append(parts, []domain.Item{it})
		} else {
			parts[i] = append(parts[i], it)
		}
	}
	return parts
}

func allDenied(out []BatchResult) bool {
	return len(out) > 0 && !slices.ContainsFunc(out, func(r BatchResult) bool { return r.Code != domain.CodePermissionDenied })
}

func undoUntil(out []BatchResult) *time.Time {
	for _, r := range out {
		if r.UndoUntil != nil {
			return r.UndoUntil
		}
	}
	return nil
}

// ---- 按批撤销 ----

// undoBatch 执行 BatchUndo：这一批里的忽略（最近一次阶段变更仍是这次忽略的）改回建议，不限时；
// 这一批里的接受（审计事件里记着的）在撤销窗口内、事项此后没有别的改动时改回建议，合进它的几条一并拆回、恢复原样；
// 窗口已过或之后改过的整份不动，逐条记跳过原因。再撤一次是 0 条。
func undoBatch(ctx context.Context, x *command.Executor, st *store.Store, req command.Request, b BatchRequest) ([]BatchResult, error) {
	if len(b.Actions) > 0 || b.Filter.Source != "" || b.Filter.Project != "" || len(b.Filter.IDs) > 0 || b.Do != "" || b.Reason != "" || !batchRe.MatchString(b.Batch) {
		return nil, usage("undo mode takes batch only (pb_ followed by 12 hex digits)")
	}
	type acc struct {
		carrier string
		members []string
	}
	var (
		dismissed []string
		accepted  []acc
		out       []BatchResult
	)
	now := x.Now()
	if err := st.View(ctx, func(tx *store.Tx) error {
		its, err := items.List(tx, items.Filter{Phase: domain.PhaseDropped})
		if err != nil {
			return err
		}
		ds, err := Dismissals(tx, its)
		if err != nil {
			return err
		}
		for _, it := range its { // 已改回的不在 dropped 里，重复撤销自然是 0 条
			if d, ok := ds[it.ID]; ok && d.Batch == b.Batch {
				dismissed = append(dismissed, it.ID)
			}
		}
		l, err := readBatchLog(tx, b.Batch)
		if err != nil || l == nil {
			return err
		}
		for _, r := range l.Results {
			if r.Do != "accept" || !r.OK {
				continue
			}
			a := acc{carrier: r.ID}
			for _, m := range l.Results {
				if m.Do == "merge" && m.OK && m.Into == r.ID {
					if it, err := items.Get(tx, m.ID); err == nil && it.Phase == domain.PhaseDropped && it.Proposal != nil &&
						it.Proposal.Sweep.Folded() && it.Proposal.Sweep.Batch == b.Batch {
						a.members = append(a.members, m.ID) // 已逐条撤过合并的不再碰
					}
				}
			}
			c, err := items.Get(tx, r.ID)
			var de *domain.Error
			if errors.As(err, &de) {
				continue // 已物理删除
			} else if err != nil {
				return err
			}
			if c.Phase == domain.PhaseProposed {
				continue // 已撤过
			}
			op, last, err := items.LastChange(tx, r.ID)
			if err != nil {
				return err
			}
			skip := ""
			switch {
			case op != "proposal.accept" || last != b.Batch:
				skip = SkipChanged
			case now.After(l.At.Add(UndoWindow)):
				skip = SkipExpired
			}
			if skip != "" {
				for _, id := range append([]string{r.ID}, a.members...) {
					out = append(out, BatchResult{ID: id, Do: "undo", Skip: skip, Batch: b.Batch})
				}
				continue
			}
			accepted = append(accepted, a)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	run := func(cmd, key string, in any) error {
		r := req
		r.Command = cmd
		r.Input, _ = json.Marshal(in)
		if req.IdemKey != "" {
			r.IdemKey = req.IdemKey + "#" + key
		}
		_, err := x.Do(ctx, r)
		return err
	}
	res := func(id string, err error) (BatchResult, error) {
		if err == nil {
			return BatchResult{ID: id, Do: "undo", OK: true, Batch: b.Batch}, nil
		}
		var de *domain.Error
		if !errors.As(err, &de) {
			return BatchResult{}, err
		}
		return BatchResult{ID: id, Do: "undo", Code: de.Code, Why: de.Why, Batch: b.Batch}, nil
	}
	for _, id := range dismissed {
		r, err := res(id, run("item.transition", id, map[string]string{"id": id, "to": string(domain.PhaseProposed), "reason": "undo batch ignore " + b.Batch}))
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	for _, a := range accepted {
		r, err := res(a.carrier, run("item.transition", a.carrier, map[string]string{"id": a.carrier, "to": string(domain.PhaseProposed), "reason": "undo batch accept " + b.Batch}))
		if err != nil {
			return out, err
		}
		out = append(out, r)
		if !r.OK {
			continue // 事项没改回，合进去的也不拆
		}
		for _, m := range a.members {
			r, err := res(m, run(CmdUnmerge, m+"#unfold", unmergeIn{Item: m, Batch: b.Batch}))
			if err != nil {
				return out, err
			}
			out = append(out, r)
		}
	}
	if allDenied(out) {
		return nil, domain.NewError(domain.CodePermissionDenied, out[0].Why, "nothing was undone")
	}
	if !slices.ContainsFunc(out, func(r BatchResult) bool { return r.OK }) {
		return out, nil // 什么都没改：不记审计
	}
	return out, logBatch(ctx, x, st, req, batchLog{Batch: b.Batch, Mode: BatchUndo, Do: "undo", Requested: len(out), Results: out})
}
