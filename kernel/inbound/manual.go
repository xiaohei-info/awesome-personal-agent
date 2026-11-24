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

// ManualRef 是输出里的一条记录：id、标题、来源、origin.ref、项目、阶段；候选另带相似度，送模型的带资料里的编号（N、E）。
type ManualRef struct {
	Item    string       `json:"item"`
	Num     string       `json:"num,omitempty"`
	Title   string       `json:"title"`
	Source  string       `json:"source,omitempty"`
	Ref     string       `json:"ref,omitempty"`
	Project string       `json:"project,omitempty"`
	Phase   domain.Phase `json:"phase"`
	Score   float64      `json:"score,omitempty"`
}

func refOf(r *sweepRec, score float64) ManualRef {
	it := r.it
	m := ManualRef{Item: it.ID, Num: r.ref, Title: it.Title, Project: it.Project, Phase: it.Phase, Score: score}
	if it.Origin != nil {
		m.Source, m.Ref = it.Origin.Source, it.Origin.Ref
	}
	return m
}

// ManualAsk 是会送给写作模型的一条：它自己、它的仓库 + PR 号、规则检索出的已有事项、被护栏或"不同 PR"拦下的。
type ManualAsk struct {
	ManualRef
	Change     string      `json:"change,omitempty"`
	Candidates []ManualRef `json:"candidates"`
	Blocked    []ManualRef `json:"blocked,omitempty"`
}

// ManualKept 是规则直接留着、不进模型的一条。
type ManualKept struct {
	ManualRef
	Why string `json:"why"`
}

// ManualGroup 是模型给的一组：主题与成员（只在调过模型时有）。
type ManualGroup struct {
	Group   string      `json:"group"`
	Topic   string      `json:"topic"`
	Members []ManualRef `json:"members"`
}

// ManualCombined 是模型给的一项批内整合与内核校验的结果：Missing 非空是关键事实校验没过，Why 非空即没有整合、成员都留着。
type ManualCombined struct {
	Topic   string      `json:"topic"`
	Carrier string      `json:"carrier,omitempty"`
	Members []ManualRef `json:"members"`
	Title   string      `json:"title"`
	Summary string      `json:"summary"`
	Missing []string    `json:"missing,omitempty"`
	Why     string      `json:"why,omitempty"`
}

// ManualDecision 是落账后从账本读回的归类结论。
type ManualDecision struct {
	Item       string `json:"item"`
	Title      string `json:"title"`
	Action     string `json:"action"`
	Target     string `json:"target,omitempty"`
	Why        string `json:"why"`
	By         string `json:"by"`
	Topic      string `json:"topic,omitempty"`
	Integrated bool   `json:"integrated,omitempty"` // 它是整合的载体：标题与摘要已改写
}

// ManualReport 是一轮手动合并的结果。演练与真跑的规则层部分（batch 到 chars）同一算法、同一输入下逐字段相同；
// 调过模型的（真跑、--with-model 演练）另带分组、整合、校验没过的条数与结论。
type ManualReport struct {
	DryRun      bool             `json:"dry_run"`
	WithModel   bool             `json:"with_model,omitempty"`
	Matched     int              `json:"matched"`   // 筛选命中的待归类
	Taken       int              `json:"taken"`     // 这一轮取的（不超过 --limit；进模型的一批不超过 batch_size）
	Remaining   int              `json:"remaining"` // 命中但留到下一轮的
	Batch       string           `json:"batch,omitempty"`
	RuleKept    []ManualKept     `json:"rule_kept"`
	Ask         []ManualAsk      `json:"ask"`
	Deferred    []ManualRef      `json:"deferred"` // 超出资料上限、留在待归类的
	Chars       int              `json:"chars"`    // 送给写作模型的资料字数
	Groups      []ManualGroup    `json:"groups,omitempty"`
	Combined    []ManualCombined `json:"combined,omitempty"`
	CheckFailed int              `json:"check_failed,omitempty"` // 关键事实校验没过、退回留着的整合项数
	Decisions   []ManualDecision `json:"decisions,omitempty"`
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
	rep := ManualReport{DryRun: true, Matched: len(sel), RuleKept: []ManualKept{}, Ask: []ManualAsk{}, Deferred: []ManualRef{}}
	if f.Limit > 0 && len(sel) > f.Limit {
		sel = sel[:f.Limit]
	}
	var keep []sweepIn
	var batch []domain.Item
	for _, it := range sel {
		if v, ok := ruleKeep(pol, it); ok {
			keep = append(keep, v)
			rep.RuleKept = append(rep.RuleKept, ManualKept{ManualRef: refOf(&sweepRec{it: it}, 0), Why: v.Why})
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
	p, err := s.planBatch(ctx, pol, all, srcs, batch)
	if err != nil {
		return pol, rep, nil, nil, err
	}
	rep.Batch, rep.Chars = p.id, len([]rune(p.text))
	byID := map[string]*sweepRec{}
	for _, m := range p.members {
		byID[m.it.ID] = m
	}
	for _, v := range p.settle {
		rep.RuleKept = append(rep.RuleKept, ManualKept{ManualRef: refOf(byID[v.Item], 0), Why: v.Why})
	}
	for _, m := range p.later {
		rep.Deferred = append(rep.Deferred, refOf(&sweepRec{it: m.it}, 0))
	}
	for _, m := range p.ask {
		a := ManualAsk{ManualRef: refOf(m, 0), Change: m.change, Candidates: []ManualRef{}}
		for _, c := range m.cands {
			a.Candidates = append(a.Candidates, refOf(c.rec, c.score))
		}
		for _, c := range p.blocked[m] {
			a.Blocked = append(a.Blocked, refOf(&sweepRec{it: c.rec.it}, c.score))
		}
		rep.Ask = append(rep.Ask, a)
	}
	return pol, rep, keep, p, nil
}

// modelPart 把模型的分组与整合（连同校验结果）写进报告。
func (rep *ManualReport) modelPart(p *batchPlan) {
	refs := func(rs []*sweepRec) []ManualRef {
		out := make([]ManualRef, len(rs))
		for i, r := range rs {
			out[i] = refOf(r, 0)
		}
		return out
	}
	for _, g := range p.groups {
		rep.Groups = append(rep.Groups, ManualGroup{Group: g.key, Topic: g.topic, Members: refs(g.members)})
	}
	for _, c := range p.combined {
		mc := ManualCombined{Topic: c.topic, Members: refs(c.members), Title: c.title, Summary: c.summary, Missing: c.missing, Why: c.why}
		if c.carrier != nil {
			mc.Carrier = c.carrier.it.ID
		}
		if len(c.missing) > 0 {
			rep.CheckFailed++
		}
		rep.Combined = append(rep.Combined, mc)
	}
}

// PlanManual 是手动合并的演练：只算规则层，不调模型、不写账本。
func (s *Service) PlanManual(ctx context.Context, f ManualFilter) (ManualReport, error) {
	_, rep, _, _, err := s.planManual(ctx, f)
	return rep, err
}

// RunManual 是手动合并的真跑：同一份规则层结果，调一次写作模型（有要问的时），以 id 的身份经 inbound.sweep 逐条落账，
// 结论从账本读回。重复执行时已归类的不再命中，第二遍零写入。`--dry-run --with-model` 在账本副本上调它，真账本不动。
// 在服务进程里跑时与维护循环共用 sweeping：规则层与模型调用在锁外，只有落账在锁内，一批的落账不和维护循环的一批交错；
// 两边算到同一条时，后落账的那次在 inbound.sweep 里见它已归类，什么都不改。
func (s *Service) RunManual(ctx context.Context, id command.Identity, f ManualFilter) (ManualReport, error) {
	pol, rep, keep, p, err := s.planManual(ctx, f)
	if err != nil {
		return rep, err
	}
	rep.DryRun = false
	settle := keep
	if p != nil {
		out, err := s.decide(ctx, pol, p)
		if err != nil {
			return rep, err // 模型没答上：什么都没落，下次再跑
		}
		settle = append(slices.Clone(keep), out...)
		rep.modelPart(p)
	}
	s.sweeping.Lock()
	err = s.land(ctx, id, settle)
	s.sweeping.Unlock()
	rerr := s.o.Store.View(ctx, func(tx *store.Tx) error {
		for _, v := range settle {
			it, err := items.Get(tx, v.Item)
			if err != nil {
				return err
			}
			if sw := it.Proposal.Sweep; sw != nil && sw.State == "done" && sw.Batch == v.Batch {
				rep.Decisions = append(rep.Decisions, ManualDecision{Item: it.ID, Title: it.Title, Action: sw.Action, Target: sw.Target, Why: sw.Why, By: sw.By,
					Topic: sw.Topic, Integrated: it.Proposal.Integrated != nil && it.Proposal.Integrated.Batch == v.Batch})
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
		rep.Marked = append(rep.Marked, refOf(&sweepRec{it: it}, 0))
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
