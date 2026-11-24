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
// 新建议只标记 pending；维护循环里攒够条数或等够时间成一批：成员互相全量可见，规则检索已有事项（进行中、已完成）作候选，
// 整批一次写作模型调用：先分组，再按优先级（并入事项 → 批内整合 → 保留）给出 attach / duplicate / merge / keep，逐条经 inbound.sweep 落账，本人可经 inbound.unmerge 撤销。

const (
	// MergeUse 是合并调用的用途，计入既有的模型调用统计。
	MergeUse = "inbound_merge"
	// MergeTemplate 是内置的合并提示词（本人在设置里改的只换正文，输出结构仍取它的）。
	MergeTemplate = "merge-batch"
	CmdSweep      = "inbound.sweep"
	CmdUnmerge    = "inbound.unmerge"

	sweepCands   = 5                // 每条成员至多带几条检索出的已有事项
	sweepRetry   = 15 * time.Minute // 模型调用失败后多久再试（只在内存）
	sweepTitle   = 120              // 送给模型的标题、摘要各自至多几个字
	sweepSummary = 200
	sweepActor   = "kernel:merge"
)

// sweepRec 是一批里的一条记录：待归类的建议（成员，N 编号）或检索出的已有事项（E 编号）。
type sweepRec struct {
	ref    string
	it     domain.Item
	member bool
	change string               // 仓库 + PR 号（changeRef）
	cands  []sweepCand          // 只有成员有：规则检索出的已有事项
	held   []domain.SourceEvent // 只有成员有：名下的来源事件（资料里的消息行、attach 进展里的出处）
}

type sweepCand struct {
	rec   *sweepRec
	score float64
}

// sweepStatus 是已有事项在提示词里的状态，用本人的话。
func sweepStatus(it domain.Item) string {
	if it.Phase == domain.PhaseDone {
		return "已完成"
	}
	return "进行中"
}

// sweepTarget 为真的事项能作第 1 步的去处：进行中的（attach）与已完成的（duplicate）。建议、被忽略的不是事项（§9.3 判定优先级）。
func sweepTarget(it domain.Item) bool {
	return it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting || it.Phase == domain.PhaseNeedsInput || it.Phase == domain.PhaseDone
}

// stepOne 是落到事项 t 上的结论：进行中并入，已完成判定重复。
func stepOne(t domain.Item) string {
	if t.Phase == domain.PhaseDone {
		return domain.SweepDuplicate
	}
	return domain.SweepAttach
}

func pending(it domain.Item) bool {
	return it.Phase == domain.PhaseProposed && it.Proposal != nil && it.Proposal.Sweep != nil && it.Proposal.Sweep.State == "pending"
}

func notSame(a, b domain.Item) bool {
	return (a.Proposal != nil && slices.Contains(a.Proposal.NotSame, b.ID)) || (b.Proposal != nil && slices.Contains(b.Proposal.NotSame, a.ID))
}

// apart 为真的两条无论谁判都不折叠在一起：本人拆开过、项目护栏、同一仓库的不同 PR。
func apart(a, b domain.Item) bool {
	return notSame(a, b) || !sameOwner(a, b) || otherChange(changeRef(a), changeRef(b))
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

// sweepText 是批内整合写到载体上的文本与成员（含载体自己）。
type sweepText struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Members []string `json:"members"`
}

// sweepIn 是一条归类结论。
type sweepIn struct {
	Item      string     `json:"item"`
	Batch     string     `json:"batch"`
	Action    string     `json:"action"`
	Target    string     `json:"target,omitempty"`
	Why       string     `json:"why"`
	By        string     `json:"by"`
	Run       string     `json:"run,omitempty"`
	Size      int        `json:"size,omitempty"`
	Chars     int        `json:"chars,omitempty"`
	Deferred  int        `json:"deferred,omitempty"` // 超预算砍半后留到下一轮的条数
	Hint      string     `json:"hint,omitempty"`     // 拿不准时可能相同的那条
	Score     float64    `json:"score,omitempty"`
	Group     string     `json:"group,omitempty"` // 批号/G<k>
	Topic     string     `json:"topic,omitempty"`
	Integrate *sweepText `json:"integrate,omitempty"` // 只在载体（keep）上：整合文本
	Into      bool       `json:"into,omitempty"`      // merge 到整合载体：落账时要求载体的整合已含这一条
	From      string     `json:"from,omitempty"`      // attach 进展里这条的出处（来源、建立时间、名下消息）
}

// Sweep 是维护循环的一步：结清不该归类的待归类建议，攒够一批就跑一批。一次至多跑一批。
// 本人补打的待归类（Backfill）不在这里归类，只由手动合并（RunManual）处理：补标记与执行合并是两个显式动作。
func (s *Service) Sweep(ctx context.Context) error {
	s.sweeping.Lock()
	defer s.sweeping.Unlock()
	now := s.o.Now()
	if now.Before(s.sweepAfter) {
		return nil
	}
	pol, all, srcs, err := s.sweepView(ctx)
	if err != nil {
		return err
	}
	var batch []domain.Item
	for _, it := range all {
		if !autoPending(it) {
			continue
		}
		if v, ok := ruleKeep(pol, it); ok {
			err = errors.Join(err, s.settle(ctx, system, v))
			continue
		}
		batch = append(batch, it)
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
	p, err := s.planBatch(ctx, pol, all, srcs, batch)
	var settle []sweepIn
	if err == nil {
		settle, err = s.decide(ctx, pol, p)
	}
	if err == nil {
		err = s.land(ctx, system, settle)
	}
	if err != nil {
		s.sweepAfter = now.Add(sweepRetry)
	}
	return err
}

// sweepView 读一批要用的策略、全部事项与来源。
func (s *Service) sweepView(ctx context.Context) (pol policy.Policy, all []domain.Item, srcs map[string]domain.Source, err error) {
	srcs = map[string]domain.Source{}
	err = s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
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
	return
}

// autoPending 是维护循环自己归类的待归类：本人补打的只由手动合并处理。
func autoPending(it domain.Item) bool { return pending(it) && it.Proposal.Sweep.Backfill == nil }

// ruleKeep：合并关闭或来源不参与时，待归类的建议不进批、只按规则留着。
func ruleKeep(pol policy.Policy, it domain.Item) (sweepIn, bool) {
	switch {
	case pol.MergeJudge == policy.MergeOff:
		return sweepIn{Item: it.ID, Action: domain.SweepKeep, By: "rule", Why: "合并已关闭，只按来源编号与合并键去重"}, true
	case !pol.Merge.Includes(originSource(it)):
		return sweepIn{Item: it.ID, Action: domain.SweepKeep, By: "rule", Why: "这个来源不参与合并"}, true
	}
	return sweepIn{}, false
}

// sameOwner 是合并护栏：只拦跨项目——两边都有项目且不是同一个。一边或两边没有项目放行（建议不带项目是常态，§9.3）。阶段相配另由 fits 判。
func sameOwner(a, b domain.Item) bool {
	return a.Project == "" || b.Project == "" || a.Project == b.Project
}

// batchGroup 是模型给的一个组：主题与成员。
type batchGroup struct {
	key, topic string
	members    []*sweepRec
}

// batchCombined 是模型给的一项整合，连同内核的校验结果。
type batchCombined struct {
	ref, topic     string
	members        []*sweepRec // 校验后真正整合的（不足两条时为空）
	carrier        *sweepRec
	title, summary string
	missing        []string // 关键事实校验没找到的片段；非空即整组留着
	why            string   // 没整合时的原因
}

// batchPlan 是一批的规则层结果，不调模型、不写账本：手动合并的演练输出它，真跑在同一份结果上调模型、落账。
// groups、combined 由 askBatch 在模型答完后填上。
type batchPlan struct {
	id       string
	size     int
	members  []*sweepRec
	settle   []sweepIn                 // 规则直接留着的（外发范围不许送）
	ask      []*sweepRec               // 送模型的（已按资料上限砍过）
	later    []*sweepRec               // 超出资料上限留到下一轮的，仍待归类
	blocked  map[*sweepRec][]sweepCand // 够得上检索条件、但被护栏或"不同 PR"拦下的已有事项
	text     string
	input    map[string]any
	srcs     map[string]domain.Source
	groups   []batchGroup
	combined []*batchCombined
}

// planBatch 跑一批的规则层：成员互相全量可见；每个成员按合并键、仓库 + PR 号、origin.ref、相似度与同项目检索已有事项；护栏；资料打包与超预算砍半。
func (s *Service) planBatch(ctx context.Context, pol policy.Policy, all []domain.Item, srcs map[string]domain.Source, batch []domain.Item) (*batchPlan, error) {
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
	p := &batchPlan{id: "mb_" + hex.EncodeToString(h.Sum(nil))[:12], size: len(batch), blocked: map[*sweepRec][]sweepCand{}, srcs: srcs}

	// 已有事项：进行中与已完成的（§9.3 判定优先级第 1 步）。建议（含已归类的）、被忽略的不作去处；批内合并只在成员之间。
	inBatch := map[string]bool{}
	for _, it := range batch {
		inBatch[it.ID] = true
	}
	var pool []*sweepRec
	for _, it := range all {
		switch {
		case pol.Merge.SkipExisting || inBatch[it.ID] || !sweepTarget(it) || it.Kind == domain.KindBrief || it.Kind == "routine":
			continue
		case !pol.Merge.Includes(originSource(it)) || !canSend(it):
			continue
		}
		pool = append(pool, &sweepRec{it: it, change: changeRef(it)})
	}
	p.members = make([]*sweepRec, len(batch))
	err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		for i, it := range batch {
			held, err := runner.Held(tx, it.ID, it.Proposal.Sources)
			if err != nil {
				return err
			}
			p.members[i] = &sweepRec{it: it, member: true, change: changeRef(it), held: held}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var ask []*sweepRec
	for _, m := range p.members {
		if !canSend(m.it) {
			p.settle = append(p.settle, sweepIn{Item: m.it.ID, Action: domain.SweepKeep, By: "rule", Why: "这个来源的外发范围不允许送给写作模型，只按规则去重"})
			continue
		}
		ask = append(ask, m)
		for _, o := range pool {
			if notSame(m.it, o.it) {
				continue
			}
			sc := similarScore(m.it.Title, m.it.Summary, o.it.Title, o.it.Summary)
			k := mergeKeyOfItem(m.it)
			if (k != "" && k == mergeKeyOfItem(o.it)) || (m.change != "" && m.change == o.change) || (m.it.Origin != nil && o.it.Origin != nil && m.it.Origin.Ref != "" && m.it.Origin.Ref == o.it.Origin.Ref) {
				sc = 1
			}
			project := m.it.Project != "" && m.it.Project == o.it.Project && o.it.Phase != domain.PhaseDone
			if sc < pol.Merge.MinScore() && !project { // 档位每批现读策略，改了下一批就生效
				continue
			}
			if !sameOwner(m.it, o.it) || otherChange(m.change, o.change) {
				p.blocked[m] = append(p.blocked[m], sweepCand{o, round2(sc)})
				continue
			}
			m.cands = append(m.cands, sweepCand{o, round2(sc)})
		}
		slices.SortStableFunc(m.cands, func(a, b sweepCand) int { return cmp.Compare(b.score, a.score) })
		if len(m.cands) > sweepCands {
			m.cands = m.cands[:sweepCands]
		}
	}
	if len(ask) == 1 && len(ask[0].cands) == 0 { // 只有它一条、也没有可选事项：分组与判定都无从谈起，不调模型
		p.settle = append(p.settle, sweepIn{Item: ask[0].it.ID, Action: domain.SweepKeep, By: "rule", Why: "这一批只有它一条，账本里也没有可能是同一件事的事项"})
		ask = nil
	}
	for i := range p.settle {
		p.settle[i].Batch, p.settle[i].Size = p.id, p.size
	}
	if len(ask) > 0 {
		p.ask, p.text, p.input = s.compose(pol, srcs, ask)
		p.later = ask[len(p.ask):]
	}
	return p, nil
}

// decide 按 plan 调一次写作模型（有要问的时），返回这一批全部结论；不写账本。
func (s *Service) decide(ctx context.Context, pol policy.Policy, p *batchPlan) ([]sweepIn, error) {
	settle := slices.Clone(p.settle)
	if len(p.ask) > 0 {
		out, err := s.askBatch(ctx, pol, p)
		if err != nil {
			return nil, err
		}
		settle = append(settle, out...)
	}
	return settle, nil
}

// land 以 id 的身份逐条经 inbound.sweep 落账。每条在落账时按账本现状重验（仍待归类、去处仍相配、护栏、整合已落成），
// 所以结论可以在锁外、按稍早的读视图算出。
func (s *Service) land(ctx context.Context, id command.Identity, settle []sweepIn) error {
	// 留着的先落（整合的载体在这一步改写文本），再落折叠：并到载体的那条落账时，载体已经整合好。
	slices.SortStableFunc(settle, func(a, b sweepIn) int {
		return cmp.Compare(b2i(a.Action != domain.SweepKeep), b2i(b.Action != domain.SweepKeep))
	})
	var errs error
	for _, v := range settle {
		errs = errors.Join(errs, s.settle(ctx, id, v))
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
	Groups []struct {
		Group   string   `json:"group"`
		Topic   string   `json:"topic"`
		Members []string `json:"members"`
	} `json:"groups"`
	Decisions []struct {
		New       string `json:"new"`
		Group     string `json:"group"`
		Step      int    `json:"step"`
		Action    string `json:"action"`
		Target    any    `json:"target"` // 模板里不限类型：留着时模型常给 null；只认字符串
		Reason    string `json:"reason"`
		Confident bool   `json:"confident"`
	} `json:"decisions"`
	Combined []struct {
		Merge   string   `json:"merge"`
		Group   string   `json:"group"`
		Members []string `json:"members"`
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
	} `json:"combined"`
}

// heldLine 是一条成员名下消息的概况：条数、各类型次数、最后一条的类型与时间。
func heldLine(held []domain.SourceEvent) string {
	if len(held) == 0 {
		return "0 条"
	}
	var types []string
	n := map[string]int{}
	last := held[0]
	for _, ev := range held {
		if n[ev.Type] == 0 {
			types = append(types, ev.Type)
		}
		n[ev.Type]++
		if ev.OccurredAt.After(last.OccurredAt) {
			last = ev
		}
	}
	parts := make([]string, len(types))
	for i, t := range types {
		parts[i] = fmt.Sprintf("%s×%d", t, n[t])
	}
	return fmt.Sprintf("%d 条：%s；最后 %s %s", len(held), strings.Join(parts, "、"), last.OccurredAt.In(time.Local).Format("2006-01-02 15:04"), last.Type)
}

// sourceName 是来源在提示词与进展里的名字。
func sourceName(srcs map[string]domain.Source, src string) string {
	if x, ok := srcs[src]; ok && x.Name != "" {
		return x.Name
	}
	if src == "" {
		return "本人或 Agent 建的"
	}
	return src
}

// compose 组送给写作模型的资料：成员编 N 号（全文标题与摘要，整合要照抄其中的数字与编号），检索出的已有事项编 E 号；
// 超出资料上限时砍半，返回这一轮实际要问的那些（ask 的前缀）。
func (s *Service) compose(pol policy.Policy, srcs map[string]domain.Source, ask []*sweepRec) ([]*sweepRec, string, map[string]any) {
	clip := func(s string, n int) string {
		r := []rune(strings.Join(strings.Fields(s), " "))
		if n > 0 && len(r) > n {
			return string(r[:n]) + "…"
		}
		return string(r)
	}
	budget := pol.Merge.Budget()
	for {
		refs := map[*sweepRec]string{}
		var existing []*sweepRec
		for i, m := range ask {
			refs[m] = fmt.Sprintf("N%d", i+1)
		}
		for _, m := range ask {
			for _, c := range m.cands {
				if _, ok := refs[c.rec]; !ok {
					existing = append(existing, c.rec)
					refs[c.rec] = fmt.Sprintf("E%d", len(existing))
				}
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# 这一批待归类的建议（共 %d 条）\n\n", len(ask))
		news := make([]string, 0, len(ask))
		cands := map[string][]string{}
		for _, m := range ask {
			news = append(news, refs[m])
			fmt.Fprintf(&b, "%s［待归类的建议］来源：%s · %s\n标题：%s\n", refs[m], sourceName(srcs, originSource(m.it)), m.it.CreatedAt.In(time.Local).Format("2006-01-02 15:04"), clip(m.it.Title, 0))
			if m.it.Summary != "" {
				fmt.Fprintf(&b, "摘要：%s\n", clip(m.it.Summary, 0))
			}
			if m.change != "" {
				fmt.Fprintf(&b, "强标识：%s\n", m.change)
			}
			fmt.Fprintf(&b, "消息：%s\n", heldLine(m.held))
			for _, c := range m.cands {
				cands[refs[m]] = append(cands[refs[m]], refs[c.rec])
			}
			if len(cands[refs[m]]) == 0 {
				b.WriteString("可选事项：无\n\n")
			} else {
				fmt.Fprintf(&b, "可选事项：%s\n\n", strings.Join(cands[refs[m]], "、"))
			}
		}
		b.WriteString("# 已有事项\n\n")
		if len(existing) == 0 {
			b.WriteString("（没有检索到）\n")
		}
		for _, r := range existing {
			fmt.Fprintf(&b, "%s［%s］来源：%s · %s\n标题：%s\n", refs[r], sweepStatus(r.it), sourceName(srcs, originSource(r.it)), r.it.CreatedAt.In(time.Local).Format("2006-01-02 15:04"), clip(r.it.Title, sweepTitle))
			if r.it.Summary != "" {
				fmt.Fprintf(&b, "摘要：%s\n", clip(r.it.Summary, sweepSummary))
			}
			if r.change != "" {
				fmt.Fprintf(&b, "强标识：%s\n", r.change)
			}
			b.WriteString("\n")
		}
		text := b.String()
		if policy.TokensOfChars(len([]rune(text))) <= budget || len(ask) == 1 {
			for r, ref := range refs {
				r.ref = ref
			}
			return ask, text, map[string]any{"new": news, "candidates": cands, "today": s.o.Now().In(time.Local).Format("2006-01-02")}
		}
		ask = ask[:len(ask)/2] // 超出资料预算：这批先归一半，其余下一轮
	}
}

// askBatch 按 plan 调一次写作模型，按组、按优先级把回答对回到每条成员；不合规则的回答一律按"留着"。
// 模型的分组与整合（连同校验结果）记在 p.groups、p.combined 上，给手动合并的报告用。
func (s *Service) askBatch(ctx context.Context, pol policy.Policy, p *batchPlan) ([]sweepIn, error) {
	locality := policy.EffectiveLocality(pol.GenerateEndpoint.Locality)
	id, size, ask, text, total := p.id, p.size, p.ask, p.text, len(p.ask)+len(p.later)
	in, _ := json.Marshal(p.input)
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

	// 第一段：分组。成员只认第一次出现的组；模型漏掉的单独成组（没有主题）。
	groupOf := map[*sweepRec]int{}
	for _, g := range x.Groups {
		bg := batchGroup{key: fmt.Sprintf("%s/G%d", id, len(p.groups)+1), topic: strings.TrimSpace(g.Topic)}
		for _, ref := range g.Members {
			if m, ok := byRef[ref]; ok && m.member {
				if _, seen := groupOf[m]; !seen {
					groupOf[m] = len(p.groups)
					bg.members = append(bg.members, m)
				}
			}
		}
		if len(bg.members) > 0 {
			p.groups = append(p.groups, bg)
		}
	}
	for _, m := range ask {
		if _, ok := groupOf[m]; !ok {
			groupOf[m] = len(p.groups)
			p.groups = append(p.groups, batchGroup{key: fmt.Sprintf("%s/G%d", id, len(p.groups)+1), members: []*sweepRec{m}})
		}
	}
	inGroup := func(g int, t *sweepRec) bool { // t 是不是本组某个成员的可选事项
		for _, m := range p.groups[g].members {
			if slices.ContainsFunc(m.cands, func(c sweepCand) bool { return c.rec == t }) {
				return true
			}
		}
		return false
	}

	// 第二段：逐条按优先级。merge 先收集，整组校验后再定。
	res := map[*sweepRec]*sweepIn{}
	merging := map[string][]*sweepRec{}
	combined := map[string]int{}
	for i, c := range x.Combined {
		if _, dup := combined[c.Merge]; !dup {
			combined[c.Merge] = i
		}
	}
	for _, m := range ask {
		g := p.groups[groupOf[m]]
		v := &sweepIn{Item: m.it.ID, Batch: id, Action: domain.SweepKeep, By: "generate", Run: run, Size: size, Chars: chars, Deferred: total - len(ask),
			Group: g.key, Topic: g.topic, Why: "模型没有给这条结论，留着"}
		res[m] = v
		for _, d := range x.Decisions {
			if d.New != m.ref {
				continue
			}
			v.Why = plainRefs(strings.TrimSpace(d.Reason), byRef)
			ref, _ := d.Target.(string)
			switch d.Action {
			case domain.SweepKeep:
			case domain.SweepAttach, domain.SweepDuplicate:
				t, ok := byRef[ref]
				switch {
				case !ok || t.member || !inGroup(groupOf[m], t):
					v.Why = "模型给的去处不在本组的可选事项里，按留着处理：" + v.Why
				case apart(m.it, t.it):
					v.Why = "去处与这条属于不同项目或不同 PR，按留着处理：" + v.Why
				case !d.Confident:
					v.Hint, v.Score = t.it.ID, scoreOf(m, t)
					v.Why = "模型拿不准，只提示：" + v.Why
				default:
					v.Action, v.Target, v.Score = stepOne(t.it), t.it.ID, scoreOf(m, t)
				}
			case domain.SweepMerge:
				i, ok := combined[ref]
				switch {
				case !ok || !slices.Contains(x.Combined[i].Members, m.ref):
					v.Why = "模型给的合并项不存在或没有列这一条，按留着处理：" + v.Why
				case !d.Confident:
					v.Why = "模型拿不准，留着：" + v.Why
				default:
					merging[ref] = append(merging[ref], m)
				}
			default:
				v.Why = "模型给的结论不认识，按留着处理：" + v.Why
			}
			break
		}
	}
	for _, c := range x.Combined {
		set, ok := merging[c.Merge]
		if !ok {
			continue
		}
		delete(merging, c.Merge) // 同号只认第一项
		bc := &batchCombined{ref: c.Merge, title: strings.TrimSpace(c.Title), summary: strings.TrimSpace(c.Summary), topic: p.groups[groupOf[set[0]]].topic}
		p.combined = append(p.combined, bc)
		bc.why = combineProblem(set, groupOf, bc)
		if bc.why == "" {
			for _, m := range set {
				bc.missing = append(bc.missing, missingFacts(bc.title+"\n"+bc.summary, keyFacts(m.it.Title, m.it.Summary))...)
			}
			if len(bc.missing) > 0 {
				bc.why = "整合后的文本缺少成员的关键数字或编号（" + strings.Join(bc.missing, "、") + "），不合并"
			}
		}
		if bc.why != "" {
			for _, m := range set {
				res[m].Why = bc.why + "，留着：" + res[m].Why
			}
			bc.members = set
			continue
		}
		bc.members, bc.carrier = set, carrierOf(set)
		ids := make([]string, len(set))
		for i, m := range set {
			ids[i] = m.it.ID
		}
		for _, m := range set {
			if m == bc.carrier {
				res[m].Integrate = &sweepText{Title: bc.title, Summary: bc.summary, Members: ids}
				continue
			}
			res[m].Action, res[m].Target, res[m].Into = domain.SweepMerge, bc.carrier.it.ID, true
		}
	}
	out2 := make([]sweepIn, 0, len(ask))
	for _, m := range ask {
		v := res[m]
		if v.Action == domain.SweepAttach {
			v.From = fmt.Sprintf("来源：%s · 建立于 %s · 消息 %s", sourceName(p.srcs, originSource(m.it)), m.it.CreatedAt.In(time.Local).Format("2006-01-02 15:04"), heldLine(m.held))
		}
		out2 = append(out2, *v)
	}
	return out2, nil
}

// combineProblem 是一项整合不能落成的原因（空为可以）：不足两条、跨组、文本为空、成员两两之间有护栏或不同 PR。
func combineProblem(set []*sweepRec, groupOf map[*sweepRec]int, bc *batchCombined) string {
	switch {
	case len(set) < 2:
		return "合并只剩一条"
	case bc.title == "" || bc.summary == "":
		return "整合后的标题或摘要为空"
	}
	for _, m := range set[1:] {
		if groupOf[m] != groupOf[set[0]] {
			return "合并的几条不在同一组"
		}
	}
	for i, a := range set {
		for _, b := range set[i+1:] {
			if apart(a.it, b.it) {
				return "合并的几条属于不同项目、不同 PR，或本人拆开过"
			}
		}
	}
	return ""
}

// carrierOf 是整合的载体：名下来源事件最多的一条，同数取建立最早的。
func carrierOf(set []*sweepRec) *sweepRec {
	best := set[0]
	for _, m := range set[1:] {
		if len(m.held) > len(best.held) || (len(m.held) == len(best.held) && m.it.CreatedAt.Before(best.it.CreatedAt)) {
			best = m
		}
	}
	return best
}

// batchRef 是资料里给记录编的号（N1、E3、M2）；理由里出现时换成那条的标题，页面上不出现编号。
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

// fits：并入只并到未关闭事项，合并只合到仍是建议的，判定重复落到已完成的事项（旧批次的结论另可落到任何记录）。
func fits(action string, t domain.Item) bool {
	switch action {
	case domain.SweepAttach:
		return t.Phase == domain.PhaseOpen || t.Phase == domain.PhaseWaiting || t.Phase == domain.PhaseNeedsInput
	case domain.SweepMerge:
		return t.Phase == domain.PhaseProposed
	case domain.SweepDuplicate:
		return true
	}
	return false
}

func (s *Service) settle(ctx context.Context, id command.Identity, v sweepIn) error {
	_, err := s.do(ctx, id, "", "inbound:sweep:"+v.Item, CmdSweep, v, command.Chain{})
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
				if t, err = items.Get(c.Tx, v.Target); err != nil || !fits(v.Action, t) || (t.Proposal != nil && t.Proposal.Sweep.Folded()) {
					v.Action, v.Target, v.Why = domain.SweepKeep, "", "去处已经变了，留着："+v.Why
				} else if !sameOwner(it, t) { // 护栏在落账处硬拦：不论谁给的结论，项目归属不同不折叠
					v.Action, v.Target, v.Why = domain.SweepKeep, "", "去处属于另一个项目，跨项目不合并，留着："+v.Why
				} else if otherChange(changeRef(it), changeRef(t)) {
					v.Action, v.Target, v.Why = domain.SweepKeep, "", "去处是同一仓库的另一个 PR，不合并，留着："+v.Why
				} else if v.Into && (t.Proposal == nil || t.Proposal.Integrated == nil || t.Proposal.Integrated.Batch != v.Batch || !slices.Contains(t.Proposal.Integrated.Members, it.ID)) {
					v.Action, v.Target, v.Why = domain.SweepKeep, "", "整合没有落成，留着："+v.Why
				}
			}
			at := c.Now()
			sw := domain.ProposalSweep{State: "done", Batch: v.Batch, Action: v.Action, Target: v.Target, Why: v.Why, By: v.By, Run: v.Run, Size: v.Size, Chars: v.Chars, Deferred: v.Deferred, At: &at,
				Group: v.Group, Topic: v.Topic, Backfill: it.Proposal.Sweep.Backfill}
			p := *it.Proposal
			p.Sweep = &sw
			if v.Action == domain.SweepKeep {
				if v.Hint != "" && !slices.ContainsFunc(p.Similar, func(x domain.ProposalSimilar) bool { return x.Item == v.Hint }) {
					p.Similar = append(slices.Clone(p.Similar), domain.ProposalSimilar{Item: v.Hint, Score: v.Score})
				}
				if v.Integrate != nil {
					if why := integrable(c.Tx, it, v.Integrate.Members); why != "" {
						sw.Why = "整合没落成（" + why + "），留着：" + v.Why
					} else {
						p.Integrated = &domain.ProposalIntegrated{Title: it.Title, Summary: it.Summary, ToTitle: v.Integrate.Title, ToSummary: v.Integrate.Summary,
							Members: v.Integrate.Members, Batch: v.Batch, Run: v.Run, At: at}
						_, _, snap, err := items.Revise(c.Tx, it.ID, "merge.integrate", at, func(x *domain.Item) error {
							x.Title, x.Summary, x.Proposal = v.Integrate.Title, v.Integrate.Summary, &p
							return nil
						})
						if err != nil {
							return nil, err
						}
						return nil, emitWrite(c, items.Write{Type: domain.EvItemUpdated, Object: it.ID, Payload: snap}, fmt.Sprintf("merge batch %s: integrated %d proposals", v.Batch, len(v.Integrate.Members)))
					}
				}
				w, err := items.SetProposal(c.Tx, it.ID, p, at)
				if err != nil {
					return nil, err
				}
				return nil, emitWrite(c, w, "merge batch "+v.Batch+": keep")
			}
			var moved []string
			switch v.Action {
			case domain.SweepMerge:
				if moved, err = moveHeld(c, it, t.ID, it.Proposal.Sources, sw, v.Score, "merge batch "+v.Batch+": merged "+it.ID+" in"); err != nil {
					return nil, err
				}
			case domain.SweepAttach:
				w, err := items.AppendProgress(c.Tx, t.ID, attachText(it, v), sweepActor, at)
				if err != nil {
					return nil, err
				}
				if err := emitWrite(c, w, "merge batch "+v.Batch+": attached "+it.ID); err != nil {
					return nil, err
				}
				if moved, err = moveHeld(c, it, t.ID, it.Proposal.Sources, sw, v.Score, "merge batch "+v.Batch+": moved messages of "+it.ID+" in"); err != nil {
					return nil, err
				}
			}
			if err := absorb(c, it, t.ID, sw, moved, at); err != nil {
				return nil, err
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

// attachText 是并入事项时追加的进展：逐字写出这一条的原标题、出处、原摘要与依据，不由模型改写。
func attachText(it domain.Item, v sweepIn) string {
	text := fmt.Sprintf("补充（跨来源合并并入，原建议「%s」）", it.Title)
	if v.From != "" {
		text += "\n" + v.From
	}
	if it.Summary != "" {
		text += "\n摘要：" + it.Summary
	}
	if v.Why != "" {
		text += "\n依据：" + v.Why
	}
	return text
}

// integrable 是载体落整合前的重验（空为可以）：其余成员仍待归类、与载体之间没有护栏、不同 PR 或本人拆开过。
func integrable(tx *store.Tx, carrier domain.Item, members []string) string {
	if !slices.Contains(members, carrier.ID) || len(members) < 2 {
		return "成员不全"
	}
	for _, id := range members {
		if id == carrier.ID {
			continue
		}
		m, err := items.Get(tx, id)
		switch {
		case err != nil || !pending(m):
			return "有成员已不在待归类"
		case apart(carrier, m):
			return "有成员与它属于不同项目或不同 PR"
		}
	}
	return ""
}

// absorb 在去处 to 上记下被折叠的 it 本身：原始标题与摘要、origin.source、origin.ref、项目、搬过去的消息与折叠结论，使溯源不靠它名下有没有消息；
// 原始正文仍在 it 自己身上（折叠只改阶段）。
func absorb(c *command.Ctx, it domain.Item, to string, sw domain.ProposalSweep, moved []string, at time.Time) error {
	t, err := items.Get(c.Tx, to)
	if err != nil {
		return err
	}
	tp := domain.Proposal{}
	if t.Proposal != nil {
		tp = *t.Proposal
	}
	a := domain.ProposalAbsorbed{Item: it.ID, Title: it.Title, Summary: it.Summary, Events: moved, Project: it.Project, Created: it.CreatedAt, Action: sw.Action, Batch: sw.Batch, At: at}
	if it.Origin != nil {
		a.Source, a.Ref = it.Origin.Source, it.Origin.Ref
	}
	tp.Absorbed = append(slices.DeleteFunc(slices.Clone(tp.Absorbed), func(x domain.ProposalAbsorbed) bool { return x.Item == it.ID }), a)
	w, err := items.SetProposal(c.Tx, to, tp, at)
	if err != nil {
		return err
	}
	return emitWrite(c, w, "merge batch "+sw.Batch+": recorded "+it.ID+" folded in")
}

// moveHeld 把被折叠的 it 名下的消息搬到去处 to（§9.3）：建出它的、合并进它的、挂在它名下的，按落盘先后；
// 每条记进 to 的 proposal.sources，并在 to 上记一条合并留痕（item 记 it，判断方、理由、批次取 sw），事件改指向 to。
// also 是 it 的 sources（可能含已不指向它的旧记录，折叠时一并记上）；修复时传 nil，只搬仍指向它的。没有可搬的什么都不写。返回搬过去的事件 id。
func moveHeld(c *command.Ctx, it domain.Item, to string, also []string, sw domain.ProposalSweep, score float64, reason string) ([]string, error) {
	held, err := runner.Held(c.Tx, it.ID, also)
	if err != nil || len(held) == 0 {
		return nil, err
	}
	t, err := items.Get(c.Tx, to)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(held))
	tp, at := domain.Proposal{}, c.Now()
	if t.Proposal != nil {
		tp = *t.Proposal
	}
	for _, sev := range held {
		if !slices.Contains(tp.Sources, sev.ID) {
			tp.Sources = append(slices.Clone(tp.Sources), sev.ID)
		}
		tp.Merges = append(slices.Clone(tp.Merges), domain.ProposalMerge{Event: sev.ID, Source: sev.SourceID, Title: it.Title, Summary: it.Summary,
			Kind: it.Kind, Project: it.Project, By: sw.By, Score: score, Why: sw.Why, Batch: sw.Batch, Run: sw.Run, Item: it.ID, At: at})
		if err := runner.Repoint(c.Tx, sev.ID, it.ID, to); err != nil {
			return nil, err
		}
		ids = append(ids, sev.ID)
	}
	w, err := items.SetProposal(c.Tx, to, tp, at)
	if err != nil {
		return nil, err
	}
	return ids, emitWrite(c, w, reason)
}

// moveBack 撤销 moveHeld：按去处 t 上记着从 it 搬来的留痕，把这些事件改指回 it，删掉留痕与 sources；notSame 时去处记下与 it 不是同一件。
func moveBack(c *command.Ctx, it, t domain.Item, notSame bool, at time.Time) error {
	if t.Proposal == nil {
		return nil
	}
	tp := *t.Proposal
	var moved []string
	tp.Merges = slices.DeleteFunc(slices.Clone(tp.Merges), func(m domain.ProposalMerge) bool {
		if m.Item == it.ID {
			moved = append(moved, m.Event)
		}
		return m.Item == it.ID
	})
	n := len(tp.Absorbed)
	tp.Absorbed = slices.DeleteFunc(slices.Clone(tp.Absorbed), func(a domain.ProposalAbsorbed) bool { return a.Item == it.ID })
	if len(moved) == 0 && !notSame && n == len(tp.Absorbed) {
		return nil
	}
	tp.Sources = slices.DeleteFunc(slices.Clone(tp.Sources), func(e string) bool { return slices.Contains(moved, e) })
	if notSame && !slices.Contains(tp.NotSame, it.ID) {
		tp.NotSame = append(slices.Clone(tp.NotSame), it.ID)
	}
	for _, ev := range moved {
		if err := runner.Repoint(c.Tx, ev, t.ID, it.ID); err != nil {
			return err
		}
	}
	w, err := items.SetProposal(c.Tx, t.ID, tp, at)
	if err != nil {
		return err
	}
	return emitWrite(c, w, "unmerge "+it.ID)
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
			case sw.Action == domain.SweepDuplicate: // 只有溯源记录要删
				if err := moveBack(c, it, t, false, at); err != nil {
					return nil, err
				}
			case sw.Action == domain.SweepMerge && t.Proposal != nil && t.Proposal.Integrated != nil && slices.Contains(t.Proposal.Integrated.Members, it.ID):
				if err := undoIntegration(c, it, t, at); err != nil { // 整合过的：整组还原
					return nil, err
				}
			case sw.Action == domain.SweepMerge:
				if err := moveBack(c, it, t, true, at); err != nil {
					return nil, err
				}
			case sw.Action == domain.SweepAttach:
				if err := moveBack(c, it, t, false, at); err != nil {
					return nil, err
				}
				w, err := items.AppendProgress(c.Tx, t.ID, "撤回上一条补充：「"+it.Title+"」经本人确认不是这件事，已退回建议。", sweepActor, at)
				if err != nil {
					return nil, err
				}
				if err := emitWrite(c, w, "unmerge "+it.ID); err != nil {
					return nil, err
				}
			}
			return map[string]string{"item": it.ID}, unfold(c, it, true, at)
		}})
}

// unfold 把折叠的 it 改回建议（sweep 记 undone）；notSame 时记下与去处不是同一件，以后不再与它合并。
func unfold(c *command.Ctx, it domain.Item, notSame bool, at time.Time) error {
	sw := *it.Proposal.Sweep
	sw.Action = domain.SweepUndone
	p := *it.Proposal
	p.Sweep = &sw
	if notSame && !slices.Contains(p.NotSame, sw.Target) {
		p.NotSame = append(slices.Clone(p.NotSame), sw.Target)
	}
	_, _, snap, err := items.Revise(c.Tx, it.ID, "merge.unfold", at, func(x *domain.Item) error {
		x.Phase, x.Proposal = domain.PhaseProposed, &p
		return nil
	})
	if err != nil {
		return err
	}
	return emitWrite(c, items.Write{Type: domain.EvItemPhaseChanged, Object: it.ID, Payload: snap}, "unmerge: back to proposals")
}

// undoIntegration 撤销一次批内整合（§9.3）：同一次整合的其他成员一并退回建议（不记 not_same），
// 本人点撤销的 it 的消息与溯源移回（记 not_same），载体的标题与摘要恢复成整合前的原文——本人事后改过的不覆盖。it 本身由调用方改回建议。
func undoIntegration(c *command.Ctx, it, t domain.Item, at time.Time) error {
	in := *t.Proposal.Integrated
	for _, id := range in.Members {
		if id == t.ID || id == it.ID {
			continue
		}
		m, err := items.Get(c.Tx, id)
		if err != nil {
			return err
		}
		if m.Phase != domain.PhaseDropped || m.Proposal == nil || m.Proposal.Sweep.Action != domain.SweepMerge || m.Proposal.Sweep.Target != t.ID {
			continue // 已单独撤过或另有去处
		}
		if t, err = items.Get(c.Tx, t.ID); err != nil {
			return err
		}
		if err := moveBack(c, m, t, false, at); err != nil {
			return err
		}
		if err := unfold(c, m, false, at); err != nil {
			return err
		}
	}
	t, err := items.Get(c.Tx, t.ID)
	if err != nil {
		return err
	}
	if err := moveBack(c, it, t, true, at); err != nil {
		return err
	}
	if t, err = items.Get(c.Tx, t.ID); err != nil {
		return err
	}
	p := *t.Proposal
	p.Integrated = nil
	_, _, snap, err := items.Revise(c.Tx, t.ID, "merge.unintegrate", at, func(x *domain.Item) error {
		if x.Title == in.ToTitle && x.Summary == in.ToSummary {
			x.Title, x.Summary = in.Title, in.Summary
		}
		x.Proposal = &p
		return nil
	})
	if err != nil {
		return err
	}
	return emitWrite(c, items.Write{Type: domain.EvItemUpdated, Object: t.ID, Payload: snap}, "unmerge "+it.ID+": integration undone")
}

// Unmerge 以 id 的身份撤销一条折叠：被折叠的建议回到建议页，去处上的合并留痕移回或追加撤回说明。
func (s *Service) Unmerge(ctx context.Context, id command.Identity, item string) error {
	_, err := s.do(ctx, id, "", "", CmdUnmerge, unmergeIn{Item: item}, command.Chain{})
	return err
}
