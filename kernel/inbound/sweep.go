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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 批量归类已取消（2026-11-26，§9.3）：本文件只保留对已有折叠建议的撤销合并（inbound.unmerge）以及批量接受折叠（bulk.go）共用的落账助手。

const (
	CmdUnmerge = "inbound.unmerge"
	sweepActor = "kernel:merge"
)

var (
	changeLink = regexp.MustCompile(`(?i)https?://[^/\s]+/([\w.-]+/[\w.-]+)/(?:pulls?|merge_requests)/(\d+)`)
	repoTag    = regexp.MustCompile(`\[([\w.-]+/[\w.-]+)\]`)
	repoBefore = regexp.MustCompile(`(?i)([\w.-]+/[\w.-]+)\s*(?:的\s*)?(?:PR|MR)\s*[#!]\d+`)
	changeNum  = regexp.MustCompile(`(?i)\b(?:PR|MR)\s*[#!](\d+)`)
)

func mergeKeyOfItem(it domain.Item) string {
	if it.Proposal == nil {
		return ""
	}
	return it.Proposal.MergeKey
}

func sameOwner(inbox string, a, b domain.Item) bool {
	unowned := func(project string) bool { return project == "" || (inbox != "" && project == inbox) }
	return unowned(a.Project) || unowned(b.Project) || a.Project == b.Project
}

func notSame(a, b domain.Item) bool {
	return (a.Proposal != nil && slices.Contains(a.Proposal.NotSame, b.ID)) || (b.Proposal != nil && slices.Contains(b.Proposal.NotSame, a.ID))
}

func changeRef(it domain.Item) string {
	ref := ""
	if it.Origin != nil {
		ref = it.Origin.Ref
	}
	texts := []string{ref, mergeKeyOfItem(it), it.Title, it.Summary}
	for _, s := range texts {
		if m := changeLink.FindStringSubmatch(s); m != nil {
			return strings.ToLower(m[1] + "#" + m[2])
		}
	}
	repo := ""
	for _, s := range texts[1:] {
		if m := repoTag.FindStringSubmatch(s); m != nil {
			repo = m[1]
			break
		}
		if m := repoBefore.FindStringSubmatch(s); m != nil {
			repo = m[1]
			break
		}
	}
	for _, s := range texts[1:] {
		if m := changeNum.FindStringSubmatch(s); m != nil && repo != "" {
			return strings.ToLower(repo + "#" + m[1])
		}
	}
	return ""
}

func otherChange(a, b string) bool {
	ra, _, oka := strings.Cut(a, "#")
	rb, _, okb := strings.Cut(b, "#")
	return oka && okb && ra == rb && a != b
}

// veto 检查批量接受折叠时的禁止性规则：跨项目、同仓不同 PR、本人拆开过的都不合并。
func veto(inbox string, it, t domain.Item) (code, why string) {
	switch {
	case !sameOwner(inbox, it, t):
		return domain.RejectCrossProject, "去处属于另一个项目，跨项目不合并"
	case otherChange(changeRef(it), changeRef(t)):
		return domain.RejectOtherChange, "去处是同一仓库的另一个 PR，不合并"
	case notSame(it, t):
		return domain.RejectNotSame, "本人拆开过这两条"
	}
	return "", ""
}

// emitWrite 追加一条事项事件并推进该事项的版本（相关命令 KeepVersion，版本只按事项事件走）。
func emitWrite(c *command.Ctx, w items.Write, reason string) error {
	if _, err := command.Bump(c.Tx, w.Object); err != nil {
		return err
	}
	_, err := c.Emit(domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, Reason: reason})
	return err
}

// foldInto 是一次折叠的收尾：在去处 to 上记下被折的 it 本身，再把 it 转 dropped、建议信息换成 p（p.Sweep 是这次折叠的记录）。
// 批量接受时按同一对象合并（proposal.merge）使用；batch 非空时记进阶段变更的载荷。
func foldInto(c *command.Ctx, it domain.Item, to string, p domain.Proposal, moved []string, at time.Time, batch, reason string) error {
	if err := absorb(c, it, to, *p.Sweep, moved, at); err != nil {
		return err
	}
	_, _, snap, err := items.Revise(c.Tx, it.ID, "merge.fold", at, func(x *domain.Item) error {
		x.Phase, x.Proposal = domain.PhaseDropped, &p
		return nil
	})
	if err == nil && batch != "" {
		snap, err = items.TagBatch(snap, batch)
	}
	if err != nil {
		return err
	}
	return emitWrite(c, items.Write{Type: domain.EvItemPhaseChanged, Object: it.ID, Payload: snap}, reason)
}

// absorb 在去处 to 上记下被折叠的 it 本身：原始标题、摘要与正文（全文）、origin.source、origin.ref、项目、搬过去的消息与折叠结论，
// 使溯源不靠它名下有没有消息；它自己身上也还留着原文（折叠只改阶段），撤销时它原样回到建议页。
func absorb(c *command.Ctx, it domain.Item, to string, sw domain.ProposalSweep, moved []string, at time.Time) error {
	t, err := items.Get(c.Tx, to)
	if err != nil {
		return err
	}
	tp := domain.Proposal{}
	if t.Proposal != nil {
		tp = *t.Proposal
	}
	a := domain.ProposalAbsorbed{Item: it.ID, Title: it.Title, Summary: it.Summary, Body: it.Body, Events: moved, Project: it.Project, Created: it.CreatedAt, Action: sw.Action, Batch: sw.Batch, At: at}
	if it.Origin != nil {
		a.Source, a.Ref = it.Origin.Source, it.Origin.Ref
	}
	tp.Absorbed = append(slices.DeleteFunc(slices.Clone(tp.Absorbed), func(x domain.ProposalAbsorbed) bool { return x.Item == it.ID }), a)
	w, err := items.SetProposal(c.Tx, to, tp, nil, at)
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
	w, err := items.SetProposal(c.Tx, to, tp, nil, at)
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
	w, err := items.SetProposal(c.Tx, t.ID, tp, nil, at)
	if err != nil {
		return err
	}
	return emitWrite(c, w, "unmerge "+it.ID)
}

// ---- 撤销 ----

// unmergeIn：Batch 只在按批撤销批量接受时带（§7.2），须等于那次折叠的批次：撤的是这一次操作，不是"这两条不是同一件"，
// 所以不记 not_same，归类记录恢复成折叠前的那一份。
type unmergeIn struct {
	Item  string `json:"item"`
	Batch string `json:"batch,omitempty"`
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

func unmergeable(tx *store.Tx, v unmergeIn) (domain.Item, error) {
	it, err := items.Get(tx, v.Item)
	if err != nil {
		return it, err
	}
	if it.Phase != domain.PhaseDropped || it.Proposal == nil || !it.Proposal.Sweep.Folded() {
		return it, domain.NewError(domain.CodeConflict, "item "+v.Item+" was not folded by a merge batch, or the fold was already undone", "re-read the item")
	}
	if sw := it.Proposal.Sweep; v.Batch != "" && (sw.By != domain.SweepByBulk || sw.Batch != v.Batch) {
		return it, domain.NewError(domain.CodeConflict, "item "+v.Item+" was not folded by batch "+v.Batch, "re-read the item")
	}
	return it, nil
}

func registerUnmerge(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdUnmerge, Action: authz.ActionManageSource, Group: authz.GroupSourceEvents, KeepVersion: true, Page: "/proposals",
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseUnmerge(in)
			return authz.Object{ID: v.Item}, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parseUnmerge(in)
			_, err := unmergeable(tx, v)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseUnmerge(c.Input)
			it, err := unmergeable(c.Tx, v)
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
			case v.Batch != "": // 按批撤销：只搬回消息与留痕，不记 not_same
				if err := moveBack(c, it, t, false, at); err != nil {
					return nil, err
				}
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
			if v.Batch != "" {
				return map[string]string{"item": it.ID}, restoreFold(c, it, at)
			}
			return map[string]string{"item": it.ID}, unfold(c, it, true, at)
		}})
}

// restoreFold 把批量接受时折叠的 it 改回建议，归类记录恢复成折叠前的那一份（没有就去掉）。
func restoreFold(c *command.Ctx, it domain.Item, at time.Time) error {
	p := *it.Proposal
	p.Sweep = it.Proposal.Sweep.Prev
	_, _, snap, err := items.Revise(c.Tx, it.ID, "merge.unfold", at, func(x *domain.Item) error {
		x.Phase, x.Proposal = domain.PhaseProposed, &p
		return nil
	})
	if err != nil {
		return err
	}
	return emitWrite(c, items.Write{Type: domain.EvItemPhaseChanged, Object: it.ID, Payload: snap}, "undo batch "+it.Proposal.Sweep.Batch+": back to proposals")
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
// 本人点撤销的 it 的消息与溯源移回（记 not_same），载体的标题、摘要与正文恢复成整合前的原文——本人事后改过的不覆盖。it 本身由调用方改回建议。
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
		if in.ToBody != "" && x.Body == in.ToBody { // 整合改过正文的才还原；本人事后改过的不覆盖
			x.Body = in.Body
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
