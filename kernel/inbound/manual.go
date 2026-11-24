package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 手动合并与补打待归类（apa merge，§9.3） ----
// 手动合并按维护循环同一套规则跑一批（规则初筛、合并护栏、资料上限、一次模型调用、inbound.sweep 落账），只是不等攒批条件、可按条件筛；
// 演练只算规则层，不调模型、不写账本。补打待归类只给还没有标记的建议记 pending 并标 backfill，维护循环不自动归类它们。

// CmdMarkPending 给一条还没有归类标记的建议补记待归类（apa merge mark-pending --apply）。
const CmdMarkPending = "inbound.mark_pending"

// ManualFilter 筛选这一轮处理的建议：来源（origin.source，结尾 * 为前缀）、项目（"-" 为不属于任何项目）、建立时刻 [Since, Until)、条数上限。
type ManualFilter struct {
	Source       string
	Project      string
	Since, Until time.Time
	Limit        int
}

func (f ManualFilter) match(it domain.Item) bool {
	src := originSource(it)
	switch {
	case f.Source != "" && !strings.HasSuffix(f.Source, "*") && src != f.Source:
		return false
	case strings.HasSuffix(f.Source, "*") && !strings.HasPrefix(src, strings.TrimSuffix(f.Source, "*")):
		return false
	case f.Project == "-" && it.Project != "", f.Project != "" && f.Project != "-" && it.Project != f.Project:
		return false
	case !f.Since.IsZero() && it.CreatedAt.Before(f.Since), !f.Until.IsZero() && !it.CreatedAt.Before(f.Until):
		return false
	}
	return true
}

// ManualRef 是输出里的一条记录：id、标题、来源、origin.ref、项目、阶段；候选另带相似度。
type ManualRef struct {
	Item    string       `json:"item"`
	Title   string       `json:"title"`
	Source  string       `json:"source,omitempty"`
	Ref     string       `json:"ref,omitempty"`
	Project string       `json:"project,omitempty"`
	Phase   domain.Phase `json:"phase"`
	Score   float64      `json:"score,omitempty"`
}

func refOf(it domain.Item, score float64) ManualRef {
	r := ManualRef{Item: it.ID, Title: it.Title, Project: it.Project, Phase: it.Phase, Score: score}
	if it.Origin != nil {
		r.Source, r.Ref = it.Origin.Source, it.Origin.Ref
	}
	return r
}

// ManualAsk 是会送给写作模型的一条：它自己、规则初筛出的候选、被合并护栏拦下的跨项目候选。
type ManualAsk struct {
	ManualRef
	Candidates []ManualRef `json:"candidates"`
	Blocked    []ManualRef `json:"blocked_cross_project,omitempty"`
}

// ManualKept 是规则直接留着、不进模型的一条。
type ManualKept struct {
	ManualRef
	Why     string      `json:"why"`
	Blocked []ManualRef `json:"blocked_cross_project,omitempty"`
}

// ManualDecision 是 --apply 落账后从账本读回的归类结论。
type ManualDecision struct {
	Item   string `json:"item"`
	Title  string `json:"title"`
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Why    string `json:"why"`
	By     string `json:"by"`
}

// ManualReport 是一轮手动合并的结果。演练与真跑的规则层部分（batch 到 groups）同一算法、同一输入下逐字段相同，真跑另带 decisions。
type ManualReport struct {
	DryRun    bool             `json:"dry_run"`
	Matched   int              `json:"matched"`   // 筛选命中的待归类
	Taken     int              `json:"taken"`     // 这一轮取的（不超过 --limit；进模型的一批不超过 batch_size）
	Remaining int              `json:"remaining"` // 命中但留到下一轮的
	Batch     string           `json:"batch,omitempty"`
	RuleKept  []ManualKept     `json:"rule_kept"`
	Ask       []ManualAsk      `json:"ask"`
	Deferred  []ManualRef      `json:"deferred"` // 超出资料上限、留在待归类的
	Groups    [][]ManualRef    `json:"groups"`   // ask 与候选按候选关系连成的组
	Chars     int              `json:"chars"`    // 送给写作模型的资料字数
	Decisions []ManualDecision `json:"decisions,omitempty"`
}

// planManual 选出这一轮的待归类（含本人补打的），算规则层结果。
func (s *Service) planManual(ctx context.Context, f ManualFilter) (policy.Policy, ManualReport, []sweepIn, *batchPlan, error) {
	pol, all, srcs, err := s.sweepView(ctx)
	if err != nil {
		return pol, ManualReport{}, nil, nil, err
	}
	var sel []domain.Item
	for _, it := range all {
		if pending(it) && f.match(it) {
			sel = append(sel, it)
		}
	}
	slices.SortStableFunc(sel, func(a, b domain.Item) int { return a.CreatedAt.Compare(b.CreatedAt) })
	rep := ManualReport{DryRun: true, Matched: len(sel), RuleKept: []ManualKept{}, Ask: []ManualAsk{}, Deferred: []ManualRef{}, Groups: [][]ManualRef{}}
	if f.Limit > 0 && len(sel) > f.Limit {
		sel = sel[:f.Limit]
	}
	var keep []sweepIn
	var batch []domain.Item
	for _, it := range sel {
		if v, ok := ruleKeep(pol, it); ok {
			keep = append(keep, v)
			rep.RuleKept = append(rep.RuleKept, ManualKept{ManualRef: refOf(it, 0), Why: v.Why})
			continue
		}
		if len(batch) < pol.Merge.Batch() { // 一批不超过 batch_size，同维护循环
			batch = append(batch, it)
		}
	}
	rep.Taken = len(keep) + len(batch)
	rep.Remaining = rep.Matched - rep.Taken
	if len(batch) == 0 {
		return pol, rep, keep, nil, nil
	}
	p := s.planBatch(pol, all, srcs, batch)
	rep.Batch, rep.Chars = p.id, len([]rune(p.text))
	blocked := func(m *sweepRec) (out []ManualRef) {
		for _, c := range p.blocked[m] {
			out = append(out, refOf(c.rec.it, c.score))
		}
		return
	}
	byID := map[string]*sweepRec{}
	for _, m := range p.members {
		byID[m.it.ID] = m
	}
	for _, v := range p.settle {
		m := byID[v.Item]
		rep.RuleKept = append(rep.RuleKept, ManualKept{ManualRef: refOf(m.it, 0), Why: v.Why, Blocked: blocked(m)})
	}
	for _, m := range p.later {
		rep.Deferred = append(rep.Deferred, refOf(m.it, 0))
	}
	// 按候选关系连组（并查集）；组按第一次出现的先后，组内同样。
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	var order []string
	refs := map[string]ManualRef{}
	add := func(r ManualRef) {
		if _, ok := parent[r.Item]; !ok {
			parent[r.Item] = r.Item
			order = append(order, r.Item)
			r.Score = 0
			refs[r.Item] = r
		}
	}
	for _, m := range p.ask {
		a := ManualAsk{ManualRef: refOf(m.it, 0), Candidates: []ManualRef{}, Blocked: blocked(m)}
		add(a.ManualRef)
		for _, c := range m.cands {
			r := refOf(c.rec.it, c.score)
			a.Candidates = append(a.Candidates, r)
			add(r)
			parent[find(c.rec.it.ID)] = find(m.it.ID)
		}
		rep.Ask = append(rep.Ask, a)
	}
	groups := map[string]int{}
	for _, id := range order {
		root := find(id)
		i, ok := groups[root]
		if !ok {
			i = len(rep.Groups)
			groups[root] = i
			rep.Groups = append(rep.Groups, nil)
		}
		rep.Groups[i] = append(rep.Groups[i], refs[id])
	}
	return pol, rep, keep, p, nil
}

// PlanManual 是手动合并的演练：只算规则层，不调模型、不写账本。
func (s *Service) PlanManual(ctx context.Context, f ManualFilter) (ManualReport, error) {
	_, rep, _, _, err := s.planManual(ctx, f)
	return rep, err
}

// RunManual 是手动合并的真跑：同一份规则层结果，调一次写作模型（有要问的时），以 id 的身份经 inbound.sweep 逐条落账，
// 结论从账本读回。重复执行时已归类的不再命中，第二遍零写入。
func (s *Service) RunManual(ctx context.Context, id command.Identity, f ManualFilter) (ManualReport, error) {
	s.sweeping.Lock() // 与维护循环互斥（同一进程里时）
	defer s.sweeping.Unlock()
	pol, rep, keep, p, err := s.planManual(ctx, f)
	if err != nil {
		return rep, err
	}
	rep.DryRun = false
	for _, v := range keep {
		err = errors.Join(err, s.settle(ctx, id, v))
	}
	var done []sweepIn
	if p != nil && err == nil {
		done, err = s.runPlan(ctx, id, pol, p)
	}
	ids := make([]string, 0, len(keep)+len(done))
	for _, v := range append(keep, done...) {
		ids = append(ids, v.Item)
	}
	rerr := s.o.Store.View(ctx, func(tx *store.Tx) error {
		for _, x := range ids {
			it, err := items.Get(tx, x)
			if err != nil {
				return err
			}
			if sw := it.Proposal.Sweep; sw != nil && sw.State == "done" {
				rep.Decisions = append(rep.Decisions, ManualDecision{Item: it.ID, Title: it.Title, Action: sw.Action, Target: sw.Target, Why: sw.Why, By: sw.By})
			}
		}
		return nil
	})
	return rep, errors.Join(err, rerr)
}

// ---- 补打待归类 ----

// MarkReport 是一轮补打的结果：补了（演练时为将补）哪些，跳过的按原因计数。
type MarkReport struct {
	DryRun  bool           `json:"dry_run"`
	By      string         `json:"by"`
	Marked  []ManualRef    `json:"marked"`
	Skipped map[string]int `json:"skipped"`
}

// markSkip 是一条建议不补打的原因；空串表示要补。
func markSkip(pol policy.Policy, it domain.Item) string {
	switch {
	case it.Phase != domain.PhaseProposed || it.Kind == domain.KindBrief || it.Kind == "routine":
		return "not_a_proposal"
	case it.Proposal != nil && it.Proposal.Sweep != nil:
		return "already_marked" // 待归类或已归类过：不重复补
	case pol.MergeJudge == policy.MergeOff:
		return "merge_off"
	case !pol.Merge.Includes(originSource(it)):
		return "source_not_included"
	}
	return ""
}

type markIn struct {
	Item string `json:"item"`
	By   string `json:"by"`
}

// MarkPending 给筛中的、还没有归类标记的建议补记待归类（apply 为假时只列出将补哪些）。每条一个 inbound.mark_pending 命令，
// 以 id 的身份落账，事件记操作者；补过的不再满足条件，第二遍零写入。补打不触发归类：维护循环跳过补打的，等本人跑 apa merge run --apply。
func (s *Service) MarkPending(ctx context.Context, id command.Identity, by string, f ManualFilter, apply bool) (MarkReport, error) {
	rep := MarkReport{DryRun: !apply, By: by, Marked: []ManualRef{}, Skipped: map[string]int{}}
	pol, all, _, err := s.sweepView(ctx)
	if err != nil {
		return rep, err
	}
	slices.SortStableFunc(all, func(a, b domain.Item) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, it := range all {
		if it.Phase != domain.PhaseProposed || !f.match(it) {
			continue
		}
		if why := markSkip(pol, it); why != "" {
			rep.Skipped[why]++
			continue
		}
		if f.Limit > 0 && len(rep.Marked) >= f.Limit {
			rep.Skipped["over_limit"]++
			continue
		}
		if apply {
			res, err := s.do(ctx, id, "", "", CmdMarkPending, markIn{Item: it.ID, By: by}, command.Chain{})
			if err != nil {
				return rep, err
			}
			var ok struct {
				Marked bool `json:"marked"`
			}
			if json.Unmarshal(res.Value, &ok) != nil || !ok.Marked {
				rep.Skipped["changed_meanwhile"]++
				continue
			}
		}
		rep.Marked = append(rep.Marked, refOf(it, 0))
	}
	return rep, nil
}

func registerMarkPending(x *command.Executor) error {
	parse := func(in json.RawMessage) (markIn, error) {
		var v markIn
		if err := json.Unmarshal(in, &v); err != nil || v.Item == "" || v.By == "" {
			return v, invalid("mark_pending needs item and by")
		}
		return v, nil
	}
	return x.Register(command.Spec{Name: CmdMarkPending, Action: authz.ActionUpdateOwn, Tier: authz.TierInternal, KeepVersion: true,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			return authz.Object{ID: v.Item, Assignee: systemPrinc}, err // 内核整理自己建的建议，同 inbound.sweep
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := parse(c.Input)
			if err != nil {
				return nil, err
			}
			it, err := items.Get(c.Tx, v.Item)
			if err != nil {
				return nil, err
			}
			_, pol, err := policy.Current(c.Tx)
			if err != nil {
				return nil, err
			}
			if markSkip(pol, it) != "" {
				return map[string]bool{"marked": false}, nil // 已有标记或不再是建议：什么都不改
			}
			at := c.Now()
			p := domain.Proposal{} // 迁移来的、本人或 Agent 建的建议可能没有建议信息
			if it.Proposal != nil {
				p = *it.Proposal
			}
			p.Sweep = &domain.ProposalSweep{State: "pending", Backfill: &domain.SweepBackfill{By: v.By, At: at}}
			w, err := items.SetProposal(c.Tx, it.ID, p, at)
			if err != nil {
				return nil, err
			}
			return map[string]bool{"marked": true}, emitWrite(c, w, "backfill: marked pending for a manual merge, by "+v.By)
		}})
}

// RegisterManual 只登记手动合并要用的两条命令（归类落账、补打待归类），给离线的 `apa merge` 用（不起入站流水线）。
func RegisterManual(x *command.Executor) error {
	if err := registerSweep(x); err != nil {
		return err
	}
	return registerMarkPending(x)
}
