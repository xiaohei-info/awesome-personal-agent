package search

import (
	"cmp"
	"slices"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// ItemQuery 是事项列表在阶段/类型/项目/执行者筛选之外的条件（附录 B.2 `GET /items`）：
// 标签、计划日期筛选与排序。各条件取交集。全文筛选（q）走统一索引（Rank），不在这里。
type ItemQuery struct {
	Tag        string
	PlannedFor string // YYYY-MM-DD
	// From、To 是日期范围（含首尾，至多 MaxRangeDays 天）：计划日期或截止所在的本机日落在范围内的事项（§7.1）。
	From, To string
	Sort     string // "" | updated | deadline | planned_for | priority
}

// MaxRangeDays 是按日期范围读的最大跨度：月视图的 6 周加上前后余量。
const MaxRangeDays = 62

// CheckRange 校验一对日期范围参数（名为 fromName、toName）：两个都是 YYYY-MM-DD，to 不早于 from，跨度至多 MaxRangeDays 天。
func CheckRange(fromName, toName, from, to string) error {
	f, err1 := time.Parse("2006-01-02", from)
	t, err2 := time.Parse("2006-01-02", to)
	switch {
	case err1 != nil || err2 != nil:
		return domain.NewError(domain.CodeInvalidRequest, fromName+" and "+toName+" must both be YYYY-MM-DD", "e.g. "+fromName+"=2026-05-01&"+toName+"=2026-05-07")
	case t.Before(f) || t.Sub(f) >= MaxRangeDays*24*time.Hour:
		return domain.NewError(domain.CodeInvalidRequest, toName+" must not be before "+fromName+", and the range is at most 62 days", "narrow the range")
	}
	return nil
}

// FilterItems 按 q 筛选并排序 its（不改入参）。
// 排序：updated 新的在前；deadline、planned_for 早的在前；priority 高的在前；未设该字段的排末尾，同值按 id。
// 不带 Sort 时保持入参顺序（带全文时入参已按相关度排好）。
func FilterItems(its []domain.Item, q ItemQuery) ([]domain.Item, error) {
	if q.PlannedFor != "" {
		if _, err := time.Parse("2006-01-02", q.PlannedFor); err != nil {
			return nil, domain.NewError(domain.CodeInvalidRequest, "planned_for must be YYYY-MM-DD", "e.g. planned_for=2026-05-01")
		}
	}
	if q.From != "" || q.To != "" {
		if err := CheckRange("from", "to", q.From, q.To); err != nil {
			return nil, err
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
		if q.From != "" && !inRange(it, q.From, q.To) {
			continue
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

// inRange：计划日期在 [from, to] 内，或截止换到本机时区后的日期在内。日期都是 YYYY-MM-DD，按字符串比较即按日期比较。
func inRange(it domain.Item, from, to string) bool {
	in := func(d string) bool { return d != "" && from <= d && d <= to }
	return in(it.PlannedFor) || it.Deadline != nil && in(it.Deadline.In(time.Local).Format("2006-01-02"))
}
