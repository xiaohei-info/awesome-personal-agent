package search

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ItemQuery 是事项列表在阶段/类型/项目/执行者筛选之外的检索条件（附录 B.2 `GET /items`）：
// 全文（标题、摘要、正文、进展）、标签、计划日期筛选与排序。各条件取交集。
type ItemQuery struct {
	Text       string
	Tag        string
	PlannedFor string // YYYY-MM-DD
	Sort       string // "" | updated | deadline | planned_for | priority
}

// ItemProgressText 返回每个事项全部进展文字（空格连接），供全文检索。
func ItemProgressText(tx *store.Tx) (map[string]string, error) {
	r, err := tx.Query("SELECT item, text FROM item_progress ORDER BY at, id")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]string{}
	for r.Next() {
		var id, text string
		if err := r.Scan(&id, &text); err != nil {
			return nil, err
		}
		out[id] += " " + text
	}
	return out, r.Err()
}

// FilterItems 按 q 筛选并排序 its（不改入参）。progress 是 ItemProgressText 的结果，缺项视为没有进展。
// 排序：updated 新的在前；deadline、planned_for 早的在前；priority 高的在前；未设该字段的排末尾，同值按 id。
// 不带 Sort 时保持入参顺序。全文沿用材料检索的切分：查询的每个词都要出现在标题、摘要、正文、进展的并集里。
func FilterItems(its []domain.Item, progress map[string]string, q ItemQuery) ([]domain.Item, error) {
	var terms []string
	if strings.TrimSpace(q.Text) != "" {
		if terms = tokens(q.Text, false); len(terms) == 0 {
			return nil, domain.NewError(domain.CodeInvalidRequest, "query has no searchable text", "use letters, digits or CJK characters")
		}
	}
	if q.PlannedFor != "" {
		if _, err := time.Parse("2006-01-02", q.PlannedFor); err != nil {
			return nil, domain.NewError(domain.CodeInvalidRequest, "planned_for must be YYYY-MM-DD", "e.g. planned_for=2026-05-01")
		}
	}
	var less func(a, b domain.Item) int
	switch q.Sort {
	case "":
	case "updated":
		less = func(a, b domain.Item) int { return b.UpdatedAt.Compare(a.UpdatedAt) }
	case "deadline":
		less = func(a, b domain.Item) int {
			if a.Deadline == nil || b.Deadline == nil {
				return cmp.Compare(b2i(a.Deadline == nil), b2i(b.Deadline == nil))
			}
			return a.Deadline.Compare(*b.Deadline)
		}
	case "planned_for":
		less = func(a, b domain.Item) int {
			if a.PlannedFor == "" || b.PlannedFor == "" {
				return cmp.Compare(b2i(a.PlannedFor == ""), b2i(b.PlannedFor == ""))
			}
			return cmp.Compare(a.PlannedFor, b.PlannedFor)
		}
	case "priority":
		less = func(a, b domain.Item) int { return cmp.Compare(prioRank(b.Priority), prioRank(a.Priority)) }
	default:
		return nil, domain.NewError(domain.CodeInvalidRequest, "unknown sort", "sort is one of updated, deadline, planned_for, priority")
	}
	out := make([]domain.Item, 0, len(its))
	for _, it := range its {
		if q.Tag != "" && !slices.Contains(it.Tags, q.Tag) || q.PlannedFor != "" && it.PlannedFor != q.PlannedFor {
			continue
		}
		if len(terms) > 0 {
			have := map[string]bool{}
			for _, t := range tokens(strings.Join([]string{it.Title, it.Summary, it.Body, progress[it.ID]}, "\n"), true) {
				have[t] = true
			}
			if slices.ContainsFunc(terms, func(t string) bool { return !have[t] }) {
				continue
			}
		}
		out = append(out, it)
	}
	if less != nil {
		slices.SortStableFunc(out, func(a, b domain.Item) int {
			return cmp.Or(less(a, b), cmp.Compare(a.ID, b.ID))
		})
	}
	return out, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func prioRank(p domain.Priority) int {
	return slices.Index([]domain.Priority{domain.PriorityLow, domain.PriorityNormal, domain.PriorityHigh, domain.PriorityUrgent}, p) + 1
}
