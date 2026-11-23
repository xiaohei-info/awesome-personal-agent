package inbound

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 跨来源相似合并（§9.3 第 6 步）：合并键没命中时，同一件事从另一个来源来也只成一条建议 ----

const (
	similarWindow = 72 * time.Hour // 只和 72 小时内建的、仍在 proposed 的建议比
	similarMin    = 0.35           // 低于它不算相似，不问模型
	similarAsk    = 2              // 每条新建议至多问两个最像的候选
	// MergeUse 是合并判断的判断记录用途，计入既有的模型调用统计。
	MergeUse = "inbound_merge"
)

// mergeTemplates：本人在设置里选的判断方对应的内置是非题模板（answer_by 不同，题面相同）。
var mergeTemplates = map[string]string{
	policy.MergeSame: "merge-same", "": "merge-same", policy.MergeJudgeOnly: "merge-same-judge", policy.MergeGenerateOnly: "merge-same-generate",
}

// strongToken 是强标识：链接，或含数字、长度不少于 4 的编号（#226、B18630823、PR-1234）。
var strongToken = regexp.MustCompile(`https?://\S+|#\d{2,}|[A-Za-z]*\d[A-Za-z0-9_-]{2,}`)

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strongToken.FindAllString(strings.ToLower(s), -1) {
		if len([]rune(t)) >= 4 || strings.HasPrefix(t, "#") {
			out[t] = true
		}
	}
	return out
}

// similarScore = 标题的字符二元组 Dice 系数 + 共享强标识 0.3 + 摘要 Dice × 0.2，上限 1。
func similarScore(aTitle, aSummary, bTitle, bSummary string) float64 {
	s := similarity(normalizeText(aTitle), normalizeText(bTitle))
	ta, tb := tokens(aTitle+" "+aSummary), tokens(bTitle+" "+bSummary)
	for t := range ta {
		if tb[t] {
			s += 0.3
			break
		}
	}
	if aSummary != "" && bSummary != "" {
		s += 0.2 * similarity(normalizeText(aSummary), normalizeText(bSummary))
	}
	return min(s, 1)
}

// sourcesOf 是一条建议已经包含的来源：它自己的来源加并入过的来源。
func sourcesOf(it domain.Item) []string {
	out := []string{it.Origin.Source}
	if it.Proposal != nil {
		for _, m := range it.Proposal.Merges {
			out = append(out, m.Source)
		}
	}
	return out
}

type similarHit struct {
	item  domain.Item
	score float64
}

// findSimilar 在合并键没命中时找"说的是同一件事"的建议：先按相似度挑候选，再由本人选定的判断方答是非题。
// 判断方答"是"且有把握时返回 into 与合并留痕；拿不准、不可用或被外发范围排除时只返回提示；关闭合并时什么都不做。
// 合并是尽力而为的：模型调用出错只降为提示，不挡住建议的产生。
func (s *Service) findSimilar(ctx context.Context, src domain.Source, ev domain.SourceEvent, title, summary string) (into string, trace *domain.ProposalMerge, hints []domain.ProposalSimilar, err error) {
	var (
		pol   policy.Policy
		cands []domain.Item
		srcs  map[string]domain.Source
	)
	err = s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if _, pol, err = policy.Current(tx); err != nil {
			return err
		}
		if cands, err = items.List(tx, items.Filter{Phase: domain.PhaseProposed}); err != nil {
			return err
		}
		all, err := registry.List(tx)
		srcs = map[string]domain.Source{}
		for _, x := range all {
			srcs[x.ID] = x
		}
		return err
	})
	if err != nil || pol.MergeJudge == policy.MergeOff {
		return "", nil, nil, err
	}
	now := s.o.Now()
	var hits []similarHit
	for _, it := range cands {
		if now.Sub(it.CreatedAt) > similarWindow || slices.Contains(sourcesOf(it), ev.SourceID) {
			continue
		}
		if sc := similarScore(title, summary, it.Title, it.Summary); sc >= similarMin {
			hits = append(hits, similarHit{it, sc})
		}
	}
	slices.SortStableFunc(hits, func(a, b similarHit) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return b.item.CreatedAt.Compare(a.item.CreatedAt)
	})
	if len(hits) > similarAsk {
		hits = hits[:similarAsk]
	}
	for _, h := range hits {
		v, jerr := s.o.Judge.Judge(ctx, s.mergeCall(pol, src, srcs, ev, title, summary, h.item))
		judgment := ""
		if jerr == nil && v.Written {
			judgment = v.Record.ID
		}
		if jerr == nil && !v.Unavailable && !v.LowConfidence && v.Answer == "yes" {
			return h.item.ID, &domain.ProposalMerge{Title: title, Summary: summary, By: v.DecidedBy, Score: round2(h.score), Judgment: judgment}, nil, nil
		}
		if jerr != nil || v.Unavailable || v.LowConfidence {
			hints = append(hints, domain.ProposalSimilar{Item: h.item.ID, Score: round2(h.score), Judgment: judgment})
		}
	}
	return "", nil, hints, nil
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// mergeCall 组出合并判断的是非题：两条建议的标题与摘要；某一边的来源不许送给某个位置的端点时，那一级跳过（§14.14）。
func (s *Service) mergeCall(pol policy.Policy, src domain.Source, srcs map[string]domain.Source, ev domain.SourceEvent, title, summary string, it domain.Item) direct.JudgeCall {
	other, registered := srcs[it.Origin.Source]
	name := func(x domain.Source, id string) string {
		if x.Name != "" {
			return x.Name
		}
		return id
	}
	in, _ := json.Marshal(map[string]any{
		"new":      map[string]any{"title": title, "summary": summary, "source": name(src, src.ID), "occurred_at": ev.OccurredAt},
		"existing": map[string]any{"title": it.Title, "summary": it.Summary, "source": name(other, it.Origin.Source), "created_at": it.CreatedAt},
	})
	text := "新到的：" + title + "\n" + summary + "\n\n已有的建议：" + it.Title + "\n" + it.Summary
	return direct.JudgeCall{Pack: DefaultPack, Template: mergeTemplates[pol.MergeJudge], Use: MergeUse, Input: in,
		Subject: ev.ID, Source: src.ID, NoStats: true, UnjudgedOnUnavailable: true,
		Material: func(locality string) (string, bool) {
			if !sendable(src.Outbound, locality) || (registered && !sendable(other.Outbound, locality)) {
				return "", false
			}
			return text, true
		}}
}

// sendable：标题与摘要属于来源内容，外发范围未选择或为 none 的位置不送。
func sendable(o domain.Outbound, locality string) bool {
	sc := registry.ScopeFor(o, locality)
	return sc != "" && sc != registry.ScopeNone
}
