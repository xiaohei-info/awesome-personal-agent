package inbound

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 跨来源合并的批量归类（§9.3） ----
// 新建议只标记 pending；维护循环里攒够条数或等够时间成一批：规则在账本全部已有记录里初筛候选，
// 整批一次写作模型调用给出 attach / merge / duplicate / keep，逐条经 inbound.sweep 落账，本人可经 inbound.unmerge 撤销。

const (
	// MergeUse 是合并调用的用途，计入既有的模型调用统计。
	MergeUse = "inbound_merge"
	// MergeTemplate 是内置的合并提示词（本人在设置里改的只换正文，输出结构仍取它的）。
	MergeTemplate = "merge-batch"
	CmdSweep      = "inbound.sweep"
	CmdUnmerge    = "inbound.unmerge"

	sweepMin     = 0.30             // 规则初筛的相似度下限
	sweepCands   = 5                // 每条新建议至多带几条候选给模型
	sweepRetry   = 15 * time.Minute // 模型调用失败后多久再试（只在内存）
	sweepTitle   = 120              // 送给模型的标题、摘要各自至多几个字
	sweepSummary = 200
	sweepActor   = "kernel:merge"
)

// sweepRec 是一批里的一条记录：新建议（N 编号）或已有记录（E 编号）。
type sweepRec struct {
	ref    string
	it     domain.Item
	member bool
	cands  []sweepCand // 只有新建议有
}

type sweepCand struct {
	rec   *sweepRec
	score float64
}

// sweepStatus 是记录在提示词里的状态，用本人的话。
func sweepStatus(it domain.Item) string {
	switch it.Phase {
	case domain.PhaseProposed:
		return "建议"
	case domain.PhaseOpen, domain.PhaseWaiting, domain.PhaseNeedsInput:
		return "进行中"
	case domain.PhaseDone:
		return "已完成"
	}
	return "本人忽略过"
}

func pending(it domain.Item) bool {
	return it.Phase == domain.PhaseProposed && it.Proposal != nil && it.Proposal.Sweep != nil && it.Proposal.Sweep.State == "pending"
}

func notSame(a, b domain.Item) bool {
	return (a.Proposal != nil && slices.Contains(a.Proposal.NotSame, b.ID)) || (b.Proposal != nil && slices.Contains(b.Proposal.NotSame, a.ID))
}

// markPending 在新建议的建议信息里记"待归类"：合并开启且来源参与时。
func markPending(tx *store.Tx, p *domain.Proposal, source string) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	if pol.MergeJudge != policy.MergeOff && pol.Merge.Includes(source) && p.Sweep == nil {
		p.Sweep = &domain.ProposalSweep{State: "pending"}
	}
	return nil
}

// sweepIn 是一条归类结论。
type sweepIn struct {
	Item     string  `json:"item"`
	Batch    string  `json:"batch"`
	Action   string  `json:"action"`
	Target   string  `json:"target,omitempty"`
	Why      string  `json:"why"`
	By       string  `json:"by"`
	Run      string  `json:"run,omitempty"`
	Size     int     `json:"size,omitempty"`
	Chars    int     `json:"chars,omitempty"`
	Deferred int     `json:"deferred,omitempty"` // 超预算砍半后留到下一轮的条数
	Hint     string  `json:"hint,omitempty"`     // 拿不准时可能相同的那条
	Score    float64 `json:"score,omitempty"`
}

// Sweep 是维护循环的一步：结清不该归类的待归类建议，攒够一批就跑一批。一次至多跑一批。
func (s *Service) Sweep(ctx context.Context) error {
	s.sweeping.Lock()
	defer s.sweeping.Unlock()
	now := s.o.Now()
	if now.Before(s.sweepAfter) {
		return nil
	}
	var (
		pol  policy.Policy
		all  []domain.Item
		srcs = map[string]domain.Source{}
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if _, pol, err = policy.Current(tx); err != nil {
			return err
		}
		if all, err = items.List(tx, items.Filter{}); err != nil {
			return err
		}
		rs, err := registry.List(tx)
		for _, x := range rs {
			srcs[x.ID] = x
		}
		return err
	})
	if err != nil {
		return err
	}
	var batch []domain.Item
	for _, it := range all {
		if !pending(it) {
			continue
		}
		switch {
		case pol.MergeJudge == policy.MergeOff:
			err = errors.Join(err, s.settle(ctx, sweepIn{Item: it.ID, Action: domain.SweepKeep, By: "rule", Why: "合并已关闭，只按来源编号与合并键去重"}))
		case !pol.Merge.Includes(originSource(it)):
			err = errors.Join(err, s.settle(ctx, sweepIn{Item: it.ID, Action: domain.SweepKeep, By: "rule", Why: "这个来源不参与合并"}))
		default:
			batch = append(batch, it)
		}
	}
	if err != nil || len(batch) == 0 {
		return err
	}
	slices.SortStableFunc(batch, func(a, b domain.Item) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if len(batch) < pol.Merge.Batch() && now.Sub(batch[0].CreatedAt) < pol.Merge.Wait() {
		return nil
	}
	if len(batch) > pol.Merge.Batch() {
		batch = batch[:pol.Merge.Batch()]
	}
	if err := s.sweepBatch(ctx, pol, all, srcs, batch); err != nil {
		s.sweepAfter = now.Add(sweepRetry)
		return err
	}
	return nil
}

// sweepBatch 跑一批：规则初筛 → 一次模型调用 → 逐条落账。
func (s *Service) sweepBatch(ctx context.Context, pol policy.Policy, all []domain.Item, srcs map[string]domain.Source, batch []domain.Item) error {
	locality := policy.EffectiveLocality(pol.GenerateEndpoint.Locality)
	canSend := func(it domain.Item) bool {
		id := originSource(it)
		if strings.HasPrefix(id, "capture:") {
			return false // 对话原文：范围在项目上，这里不读项目，保守不送
		}
		x, ok := srcs[id]
		return !ok || sendable(x.Outbound, locality) // 没登记的（本人建的、旧伪来源、例行任务）与模型调用的外发闸口同一取舍
	}
	h := sha256.New()
	for _, it := range batch {
		h.Write([]byte(it.ID + "|"))
	}
	id := "mb_" + hex.EncodeToString(h.Sum(nil))[:12]
	size := len(batch)

	// 比对对象：账本里已有的全部记录（§9.3 比对对象一行）。
	inBatch := map[string]bool{}
	for _, it := range batch {
		inBatch[it.ID] = true
	}
	var pool []*sweepRec
	for _, it := range all {
		switch {
		case inBatch[it.ID] || pending(it) || it.Kind == domain.KindBrief || it.Kind == "routine":
			continue
		case it.Proposal != nil && it.Proposal.Sweep.Folded():
			continue // 被折叠的由它的去处代表
		case !pol.Merge.Includes(originSource(it)) || !canSend(it):
			continue
		case it.Phase != domain.PhaseProposed && pol.Merge.SkipExisting:
			continue
		}
		pool = append(pool, &sweepRec{it: it})
	}
	members := make([]*sweepRec, len(batch))
	var settle []sweepIn
	for i, it := range batch {
		members[i] = &sweepRec{it: it, member: true}
	}
	for i, m := range members {
		if !canSend(m.it) {
			settle = append(settle, sweepIn{Item: m.it.ID, Action: domain.SweepKeep, By: "rule", Why: "这个来源的外发范围不允许送给写作模型，只按规则去重"})
			continue
		}
		others := slices.Clip(pool)     // 追加同批更早的那几条时不写进 pool 的底层数组
		for _, o := range members[:i] { // 只和同批更早的配对：合并的方向固定，不会成环
			if canSend(o.it) {
				others = append(others, o)
			}
		}
		for _, o := range others {
			if notSame(m.it, o.it) {
				continue
			}
			sc := similarScore(m.it.Title, m.it.Summary, o.it.Title, o.it.Summary)
			if k := mergeKeyOfItem(m.it); k != "" && k == mergeKeyOfItem(o.it) {
				sc = 1
			}
			if sc >= sweepMin {
				m.cands = append(m.cands, sweepCand{o, round2(sc)})
			}
		}
		slices.SortStableFunc(m.cands, func(a, b sweepCand) int { return cmp.Compare(b.score, a.score) })
		if len(m.cands) > sweepCands {
			m.cands = m.cands[:sweepCands]
		}
		if len(m.cands) == 0 {
			settle = append(settle, sweepIn{Item: m.it.ID, Action: domain.SweepKeep, By: "rule", Why: "和账本里已有的记录比过，没有相似的"})
		}
	}
	ask := slices.DeleteFunc(slices.Clone(members), func(m *sweepRec) bool { return len(m.cands) == 0 })
	for i := range settle {
		settle[i].Batch, settle[i].Size = id, size
	}
	if len(ask) > 0 {
		out, err := s.askBatch(ctx, pol, locality, srcs, id, size, ask)
		if err != nil {
			return err
		}
		settle = append(settle, out...)
	}
	// 留着的先落，再落折叠：并到同批建议的那条落账时，去处已经是归类过的建议。
	slices.SortStableFunc(settle, func(a, b sweepIn) int {
		return cmp.Compare(b2i(a.Action != domain.SweepKeep), b2i(b.Action != domain.SweepKeep))
	})
	var errs error
	for _, v := range settle {
		errs = errors.Join(errs, s.settle(ctx, v))
	}
	return errs
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mergeKeyOfItem(it domain.Item) string {
	if it.Proposal == nil {
		return ""
	}
	return it.Proposal.MergeKey
}

type sweepOut struct {
	Decisions []struct {
		New       string `json:"new"`
		Action    string `json:"action"`
		Target    any    `json:"target"` // 模板里不限类型：留着时模型常给 null；只认字符串
		Reason    string `json:"reason"`
		Confident bool   `json:"confident"`
	} `json:"decisions"`
}

// askBatch 组资料、调一次写作模型，把回答对回到每条新建议；不合规则的回答一律按"留着"。
func (s *Service) askBatch(ctx context.Context, pol policy.Policy, locality string, srcs map[string]domain.Source, id string, size int, ask []*sweepRec) ([]sweepIn, error) {
	name := func(src string) string {
		if x, ok := srcs[src]; ok && x.Name != "" {
			return x.Name
		}
		if src == "" {
			return "本人或 Agent 建的"
		}
		return src
	}
	clip := func(s string, n int) string {
		r := []rune(strings.Join(strings.Fields(s), " "))
		if len(r) > n {
			return string(r[:n]) + "…"
		}
		return string(r)
	}
	budget, total := pol.Merge.Budget(), len(ask)
	var text string
	var input map[string]any
	for {
		n, e := 0, 0
		refs := map[*sweepRec]string{}
		for _, m := range ask {
			n++
			refs[m] = fmt.Sprintf("N%d", n)
		}
		var b strings.Builder
		cands := map[string][]string{}
		var existing []*sweepRec
		for _, m := range ask {
			for _, c := range m.cands {
				if _, ok := refs[c.rec]; !ok {
					if c.rec.member { // 同批更早的、但自己没有候选的那条也要编号
						n++
						refs[c.rec] = fmt.Sprintf("N%d", n)
					} else {
						e++
						refs[c.rec] = fmt.Sprintf("E%d", e)
					}
					existing = append(existing, c.rec)
				}
				cands[refs[m]] = append(cands[refs[m]], refs[c.rec])
			}
		}
		line := func(r *sweepRec, label string) {
			fmt.Fprintf(&b, "%s［%s］来源：%s · %s\n标题：%s\n", refs[r], label, name(originSource(r.it)), r.it.CreatedAt.In(time.Local).Format("2006-01-02 15:04"), clip(r.it.Title, sweepTitle))
			if r.it.Summary != "" {
				fmt.Fprintf(&b, "摘要：%s\n", clip(r.it.Summary, sweepSummary))
			}
		}
		b.WriteString("# 这一批新到的建议\n\n")
		for _, m := range ask {
			line(m, "新到的建议")
			fmt.Fprintf(&b, "候选：%s\n\n", strings.Join(cands[refs[m]], "、"))
		}
		b.WriteString("# 候选记录\n\n")
		for _, r := range existing {
			label := sweepStatus(r.it)
			if r.member {
				label = "同批的新建议"
			}
			line(r, label)
			b.WriteString("\n")
		}
		news := make([]string, 0, len(ask))
		for _, m := range ask {
			news = append(news, refs[m])
		}
		text, input = b.String(), map[string]any{"new": news, "candidates": cands, "today": s.o.Now().In(time.Local).Format("2006-01-02")}
		if policy.MergeTokens(len([]rune(text))) <= budget || len(ask) == 1 {
			for r, ref := range refs {
				r.ref = ref
			}
			break
		}
		ask = ask[:len(ask)/2] // 超出资料预算：这批先归一半，其余下一轮
	}
	in, _ := json.Marshal(input)
	out, err := s.o.Judge.Do(ctx, direct.Call{Pack: DefaultPack, Template: MergeTemplate, Input: in, Material: text, Body: pol.Merge.Prompt,
		RequireLocality: locality, NoStats: true, Actor: domain.Actor{Type: domain.ActorAgent, Principal: systemPrinc, Via: domain.ViaSource}})
	if errors.Is(err, model.ErrNotConfigured) || errors.Is(err, model.ErrPaused) || errors.Is(err, direct.ErrUnavailable) || errors.Is(err, direct.ErrLocalityChanged) {
		return nil, fmt.Errorf("merge batch %s waits: %w", id, err) // 留在待归类里，过一会儿再试；建议本身已经在建议页上
	}
	if err != nil {
		return nil, err
	}
	var x sweepOut
	if !out.Settled || json.Unmarshal(out.Final().Result, &x) != nil {
		return nil, fmt.Errorf("merge batch %s: the model gave no usable answer", id)
	}
	run, chars := out.Final().RunID, len([]rune(text))
	byRef := map[string]*sweepRec{}
	for _, m := range ask {
		byRef[m.ref] = m
		for _, c := range m.cands {
			byRef[c.rec.ref] = c.rec
		}
	}
	folded := map[string]sweepIn{} // 同批里已折叠的：并到它的跟着它走
	var res []sweepIn
	for _, m := range ask {
		v := sweepIn{Item: m.it.ID, Batch: id, Action: domain.SweepKeep, By: "generate", Run: run, Size: size, Chars: chars, Deferred: total - len(ask), Why: "模型没有给这条结论，留着"}
		for _, d := range x.Decisions {
			if d.New != m.ref {
				continue
			}
			v.Why = plainRefs(strings.TrimSpace(d.Reason), byRef)
			ref, _ := d.Target.(string)
			t, ok := byRef[ref]
			inCands := ok && slices.ContainsFunc(m.cands, func(c sweepCand) bool { return c.rec == t })
			switch {
			case d.Action == domain.SweepKeep:
			case !inCands:
				v.Why = "模型给的去处不在候选里，按留着处理：" + v.Why
			case !d.Confident:
				v.Hint, v.Score = t.it.ID, scoreOf(m, t)
				v.Why = "模型拿不准，只提示：" + v.Why
			case !fits(d.Action, t):
				v.Why = "模型给的结论和去处的状态不相配，按留着处理：" + v.Why
			default:
				v.Action, v.Target, v.Score = d.Action, t.it.ID, scoreOf(m, t)
				if f, ok := folded[t.it.ID]; ok { // 去处本身也折叠了：跟到它的最终去处
					v.Action, v.Target = f.Action, f.Target
				}
			}
			break
		}
		if v.Action != domain.SweepKeep {
			folded[m.it.ID] = v
		}
		res = append(res, v)
	}
	return res, nil
}

// batchRef 是资料里给记录编的号（N1、E3）；理由里出现时换成那条的标题，页面上不出现编号。
var batchRef = regexp.MustCompile(`[NE]\d+`)

func plainRefs(why string, byRef map[string]*sweepRec) string {
	return batchRef.ReplaceAllStringFunc(why, func(ref string) string {
		r, ok := byRef[ref]
		if !ok {
			return ref
		}
		t := []rune(r.it.Title)
		if len(t) > 24 {
			t = append(t[:24], '…')
		}
		return "「" + string(t) + "」"
	})
}

func scoreOf(m, t *sweepRec) float64 {
	for _, c := range m.cands {
		if c.rec == t {
			return c.score
		}
	}
	return 0
}

// fits：并入只并到未关闭事项，合并只合到仍是建议的，判定重复对哪种记录都行。
func fits(action string, t *sweepRec) bool {
	switch action {
	case domain.SweepAttach:
		return !t.member && (t.it.Phase == domain.PhaseOpen || t.it.Phase == domain.PhaseWaiting || t.it.Phase == domain.PhaseNeedsInput)
	case domain.SweepMerge:
		return t.it.Phase == domain.PhaseProposed
	case domain.SweepDuplicate:
		return true
	}
	return false
}

func (s *Service) settle(ctx context.Context, v sweepIn) error {
	_, err := s.do(ctx, system, "", "inbound:sweep:"+v.Item, CmdSweep, v, command.Chain{})
	return err
}

func parseSweep(in json.RawMessage) (sweepIn, error) {
	var v sweepIn
	if err := json.Unmarshal(in, &v); err != nil || v.Item == "" {
		return v, invalid("sweep needs item")
	}
	return v, nil
}

// emitWrite 追加一条事项事件并推进该事项的版本（两条命令都 KeepVersion，版本只按事项事件走）。
func emitWrite(c *command.Ctx, w items.Write, reason string) error {
	if _, err := command.Bump(c.Tx, w.Object); err != nil {
		return err
	}
	_, err := c.Emit(domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, Reason: reason})
	return err
}

func registerSweep(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdSweep, Action: authz.ActionUpdateOwn, Tier: authz.TierInternal, KeepVersion: true,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseSweep(in)
			return authz.Object{ID: v.Item, Assignee: systemPrinc}, err // 内核整理自己建的建议，同 inbound.dispose
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := parseSweep(c.Input)
			if err != nil {
				return nil, err
			}
			it, err := items.Get(c.Tx, v.Item)
			if err != nil || !pending(it) {
				return nil, err // 已归类过或本人已处理：什么都不改
			}
			var t domain.Item
			if v.Action != domain.SweepKeep {
				if t, err = items.Get(c.Tx, v.Target); err != nil || !fits(v.Action, &sweepRec{it: t}) || (t.Proposal != nil && t.Proposal.Sweep.Folded()) {
					v.Action, v.Target, v.Why = domain.SweepKeep, "", "去处已经变了，留着："+v.Why
				}
			}
			at := c.Now()
			sw := domain.ProposalSweep{State: "done", Batch: v.Batch, Action: v.Action, Target: v.Target, Why: v.Why, By: v.By, Run: v.Run, Size: v.Size, Chars: v.Chars, Deferred: v.Deferred, At: &at}
			p := *it.Proposal
			p.Sweep = &sw
			if v.Action == domain.SweepKeep {
				if v.Hint != "" && !slices.ContainsFunc(p.Similar, func(x domain.ProposalSimilar) bool { return x.Item == v.Hint }) {
					p.Similar = append(slices.Clone(p.Similar), domain.ProposalSimilar{Item: v.Hint, Score: v.Score})
				}
				w, err := items.SetProposal(c.Tx, it.ID, p, at)
				if err != nil {
					return nil, err
				}
				return nil, emitWrite(c, w, "merge batch "+v.Batch+": keep")
			}
			switch v.Action {
			case domain.SweepMerge:
				tp := domain.Proposal{}
				if t.Proposal != nil {
					tp = *t.Proposal
				}
				for _, ev := range it.Proposal.Sources {
					sev, err := runner.Event(c.Tx, ev)
					if err != nil {
						return nil, err
					}
					if !slices.Contains(tp.Sources, ev) {
						tp.Sources = append(slices.Clone(tp.Sources), ev)
					}
					tp.Merges = append(slices.Clone(tp.Merges), domain.ProposalMerge{Event: ev, Source: sev.SourceID, Title: it.Title, Summary: it.Summary,
						Kind: it.Kind, Project: it.Project, By: v.By, Score: v.Score, Why: v.Why, Batch: v.Batch, Run: v.Run, Item: it.ID, At: at})
					if err := runner.Repoint(c.Tx, ev, it.ID, t.ID); err != nil {
						return nil, err
					}
				}
				w, err := items.SetProposal(c.Tx, t.ID, tp, at)
				if err != nil {
					return nil, err
				}
				if err := emitWrite(c, w, "merge batch "+v.Batch+": merged "+it.ID+" in"); err != nil {
					return nil, err
				}
			case domain.SweepAttach:
				text := fmt.Sprintf("补充（跨来源合并并入，原建议「%s」）：%s", it.Title, it.Title)
				if it.Summary != "" {
					text += "\n" + it.Summary
				}
				if v.Why != "" {
					text += "\n依据：" + v.Why
				}
				w, err := items.AppendProgress(c.Tx, t.ID, text, sweepActor, at)
				if err != nil {
					return nil, err
				}
				if err := emitWrite(c, w, "merge batch "+v.Batch+": attached "+it.ID); err != nil {
					return nil, err
				}
			}
			_, _, snap, err := items.Revise(c.Tx, it.ID, "merge.fold", at, func(x *domain.Item) error {
				x.Phase, x.Proposal = domain.PhaseDropped, &p
				return nil
			})
			if err != nil {
				return nil, err
			}
			return nil, emitWrite(c, items.Write{Type: domain.EvItemPhaseChanged, Object: it.ID, Payload: snap}, "merge batch "+v.Batch+": "+v.Action+" into "+v.Target)
		}})
}

// ---- 撤销 ----

type unmergeIn struct {
	Item string `json:"item"`
}

func parseUnmerge(in json.RawMessage) (unmergeIn, error) {
	var v unmergeIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil || v.Item == "" {
		return v, invalid("unmerge needs item")
	}
	return v, nil
}

func unmergeable(tx *store.Tx, id string) (domain.Item, error) {
	it, err := items.Get(tx, id)
	if err != nil {
		return it, err
	}
	if it.Phase != domain.PhaseDropped || it.Proposal == nil || !it.Proposal.Sweep.Folded() {
		return it, domain.NewError(domain.CodeConflict, "item "+id+" was not folded by a merge batch, or the fold was already undone", "re-read the item")
	}
	return it, nil
}

func registerUnmerge(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdUnmerge, Action: authz.ActionManageSource, Tier: authz.TierOrganize, KeepVersion: true, Page: "/proposals",
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseUnmerge(in)
			return authz.Object{ID: v.Item}, err
		},
		Validate: func(tx *store.Tx, o authz.Object, _ json.RawMessage) error {
			_, err := unmergeable(tx, o.ID)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseUnmerge(c.Input)
			it, err := unmergeable(c.Tx, v.Item)
			if err != nil {
				return nil, err
			}
			at, sw := c.Now(), *it.Proposal.Sweep
			t, err := items.Get(c.Tx, sw.Target)
			var de *domain.Error
			switch {
			case errors.As(err, &de) && de.Code == domain.CodeNotFound: // 去处已清除：只恢复这一条
			case err != nil:
				return nil, err
			case sw.Action == domain.SweepMerge && t.Proposal != nil:
				tp := *t.Proposal
				var moved []string
				tp.Merges = slices.DeleteFunc(slices.Clone(tp.Merges), func(m domain.ProposalMerge) bool {
					if m.Item == it.ID {
						moved = append(moved, m.Event)
					}
					return m.Item == it.ID
				})
				tp.Sources = slices.DeleteFunc(slices.Clone(tp.Sources), func(e string) bool { return slices.Contains(moved, e) })
				if !slices.Contains(tp.NotSame, it.ID) {
					tp.NotSame = append(slices.Clone(tp.NotSame), it.ID)
				}
				for _, ev := range moved {
					if err := runner.Repoint(c.Tx, ev, t.ID, it.ID); err != nil {
						return nil, err
					}
				}
				w, err := items.SetProposal(c.Tx, t.ID, tp, at)
				if err != nil {
					return nil, err
				}
				if err := emitWrite(c, w, "unmerge "+it.ID); err != nil {
					return nil, err
				}
			case sw.Action == domain.SweepAttach:
				w, err := items.AppendProgress(c.Tx, t.ID, "撤回上一条补充：「"+it.Title+"」经本人确认不是这件事，已退回建议。", sweepActor, at)
				if err != nil {
					return nil, err
				}
				if err := emitWrite(c, w, "unmerge "+it.ID); err != nil {
					return nil, err
				}
			}
			sw.Action = domain.SweepUndone
			p := *it.Proposal
			p.Sweep = &sw
			if !slices.Contains(p.NotSame, sw.Target) {
				p.NotSame = append(slices.Clone(p.NotSame), sw.Target)
			}
			_, _, snap, err := items.Revise(c.Tx, it.ID, "merge.unfold", at, func(x *domain.Item) error {
				x.Phase, x.Proposal = domain.PhaseProposed, &p
				return nil
			})
			if err != nil {
				return nil, err
			}
			return map[string]string{"item": it.ID}, emitWrite(c, items.Write{Type: domain.EvItemPhaseChanged, Object: it.ID, Payload: snap}, "unmerge: back to proposals")
		}})
}

// Unmerge 以 id 的身份撤销一条折叠：被折叠的建议回到建议页，去处上的合并留痕移回或追加撤回说明。
func (s *Service) Unmerge(ctx context.Context, id command.Identity, item string) error {
	_, err := s.do(ctx, id, "", "", CmdUnmerge, unmergeIn{Item: item}, command.Chain{})
	return err
}
