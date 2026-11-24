package sources

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// mergeDecision 是一批里一条新建议的归类：去处、理由、判断方，撤销过的标 undone；Sources 是它带着的消息条数（折叠时一起并进去处）。
type mergeDecision struct {
	Item        string       `json:"item"`
	Title       string       `json:"title"`
	Source      string       `json:"source,omitempty"`
	Sources     int          `json:"sources"`
	Action      string       `json:"action"`
	Target      string       `json:"target,omitempty"`
	TargetTitle string       `json:"target_title,omitempty"`
	TargetPhase domain.Phase `json:"target_phase,omitempty"`
	Why         string       `json:"why,omitempty"`
	By          string       `json:"by"`
	Hint        string       `json:"hint,omitempty"`
}

// mergeBatch 是一批归类：时刻、条数、判断方、那次模型调用与送出的字数（没调模型时 run 为空）、超预算留到下一轮的条数。
type mergeBatch struct {
	Batch     string          `json:"batch"`
	At        time.Time       `json:"at"`
	Size      int             `json:"size"`
	Run       string          `json:"run,omitempty"`
	Chars     int             `json:"chars"`
	Deferred  int             `json:"deferred,omitempty"`
	Decisions []mergeDecision `json:"decisions"`
}

// mergeActivity 是"最近自动归类"：days 天内（缺省 7）每批的去向与成本，加上还在待归类的条数。只读账本里的建议信息。
func (a *api) mergeActivity(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	days := 7
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 {
		days = min(d, 90)
	}
	var all []domain.Item
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { all, err = items.List(tx, items.Filter{}); return }); err != nil {
		return err
	}
	byID := map[string]domain.Item{}
	for _, it := range all {
		byID[it.ID] = it
	}
	since := clock.Now().Add(-time.Duration(days) * 24 * time.Hour)
	batches := map[string]*mergeBatch{}
	pending, calls, chars := 0, 0, 0
	for _, it := range all {
		if it.Proposal == nil || it.Proposal.Sweep == nil {
			continue
		}
		sw := it.Proposal.Sweep
		if sw.State == "pending" {
			if it.Phase == domain.PhaseProposed {
				pending++
			}
			continue
		}
		if sw.Batch == "" || sw.At == nil || sw.At.Before(since) || sw.By == domain.SweepByBulk { // 批量接受时的合并是本人做的，不是自动归类
			continue
		}
		b := batches[sw.Batch]
		if b == nil {
			b = &mergeBatch{Batch: sw.Batch, At: *sw.At, Size: sw.Size, Decisions: []mergeDecision{}}
			batches[sw.Batch] = b
		}
		if sw.Run != "" && b.Run == "" {
			b.Run, b.Chars, b.Deferred = sw.Run, sw.Chars, sw.Deferred
			calls, chars = calls+1, chars+sw.Chars
		}
		if sw.At.After(b.At) {
			b.At = *sw.At
		}
		d := mergeDecision{Item: it.ID, Title: it.Title, Sources: len(it.Proposal.Sources), Action: sw.Action, Target: sw.Target, Why: sw.Why, By: sw.By}
		if it.Origin != nil {
			d.Source = it.Origin.Source
		}
		if t, ok := byID[sw.Target]; ok {
			d.TargetTitle, d.TargetPhase = t.Title, t.Phase
			d.Sources = carried(it, t)
		}
		for _, h := range it.Proposal.Similar {
			d.Hint = h.Item
		}
		b.Decisions = append(b.Decisions, d)
	}
	out := make([]*mergeBatch, 0, len(batches))
	for _, b := range batches {
		slices.SortFunc(b.Decisions, func(x, y mergeDecision) int { return cmp.Compare(x.Item, y.Item) })
		out = append(out, b)
	}
	slices.SortFunc(out, func(x, y *mergeBatch) int { return y.At.Compare(x.At) })
	return core.WriteJSON(w, http.StatusOK, map[string]any{"days": days, "pending": pending, "model_calls": calls, "chars": chars, "batches": out})
}

// carried 是一条建议带着几条消息：合并进别处的，以去处上记着从它搬来的留痕为准（含不在它 sources 里的挂靠消息）；其余是它自己的 sources。
func carried(it, t domain.Item) int {
	if it.Proposal.Sweep.Action != domain.SweepMerge || t.Proposal == nil {
		return len(it.Proposal.Sources)
	}
	n := 0
	for _, m := range t.Proposal.Merges {
		if m.Item == it.ID {
			n++
		}
	}
	return n
}
