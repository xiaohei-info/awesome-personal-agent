package items

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// dateLayout 是 planned_for 与进展 on 的唯一格式：只到日，按本机时区解释"今天"（§6.3）。
const dateLayout = "2006-01-02"

// StaleAfter 是"久未推进"的默认天数（设计 §7.3，默认 14 天）。
const StaleAfter = 14 * 24 * time.Hour

// checkDate 要求 s 是真实存在的 YYYY-MM-DD（拒绝 2026-13-40、带时刻的写法与不规范的补零）。
func checkDate(field, s string) error {
	if d, err := time.Parse(dateLayout, s); err != nil || d.Format(dateLayout) != s {
		return invalid(field + " only accepts a date in YYYY-MM-DD form")
	}
	return nil
}

// Today 是 now 在本机时区的日期。
func Today(now time.Time) string { return now.In(time.Local).Format(dateLayout) }

// ---- 进展 ----

type progressIn struct {
	ID   string  `json:"id"`
	Text string  `json:"text"`
	On   *string `json:"on"`
}

func insertProgress(tx *store.Tx, item string, p domain.Progress) error {
	_, err := tx.Exec(`INSERT INTO item_progress (id, item, text, on_date, actor, at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.ID, item, p.Text, p.On, p.Actor, p.At.UnixNano())
	return err
}

// Progress 按记录先后返回事项的进展；没有时返回 nil。
func Progress(tx *store.Tx, item string) ([]domain.Progress, error) {
	r, err := tx.Query(`SELECT id, text, on_date, actor, at FROM item_progress WHERE item = ? ORDER BY at, id`, item)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []domain.Progress
	for r.Next() {
		var p domain.Progress
		var at int64
		if err := r.Scan(&p.ID, &p.Text, &p.On, &p.Actor, &at); err != nil {
			return nil, err
		}
		p.At = time.Unix(0, at).UTC()
		out = append(out, p)
	}
	return out, r.Err()
}

func (s *svc) progress() command.Spec {
	parse := func(in json.RawMessage) (progressIn, error) {
		var v progressIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "item.progress", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Bound: bound,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			cur, err := Get(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			return object(tx, cur, cur)
		},
		Actions: func(tx *store.Tx, c command.Caller, o authz.Object, _ json.RawMessage) []authz.Action {
			return editActions(c, o, boundOr(tx, c, o), false, false)
		},
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			if blank(v.Text) {
				return invalid("text is required")
			}
			if v.On != nil {
				return checkDate("on", *v.On)
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			if v.On != nil && *v.On > Today(c.Now()) { // 时钟只在命令里，所以"不晚于今天"在这里判
				return nil, invalid("on cannot be later than today")
			}
			cur, err := Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			prev, err := Progress(c.Tx, cur.ID)
			if err != nil {
				return nil, err
			}
			// id 随条数递增：同一时刻追加的几条仍按写入先后排序。
			p := domain.Progress{ID: fmt.Sprintf("%s.p%06d", cur.ID, len(prev)+1), Text: v.Text, Actor: c.Caller.Actor.Principal, At: c.Now().UTC()}
			if v.On != nil {
				p.On = *v.On
			}
			if err := insertProgress(c.Tx, cur.ID, p); err != nil {
				return nil, err
			}
			cur = stamped(cur, c.Now())
			if err := save(c.Tx, cur); err != nil {
				return nil, err
			}
			return ref{cur.ID}, emit(c, domain.EvItemProgressAdded, "", "", payload{Op: "progress", Item: cur, Progress: &p})
		}}
}

// ---- 今天 ----

// TodayList 是"今天"的四组（§4.1、§7.1）。每组都不为 nil。
type TodayList struct {
	Planned []domain.Item `json:"planned"`
	Due     []domain.Item `json:"due"`
	Pending []domain.Item `json:"pending"`
	Stale   []domain.Item `json:"stale"`
}

func active(it domain.Item) bool {
	return it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting || it.Phase == domain.PhaseNeedsInput
}

// TodayItems 取材：planned = 计划今天或更早且未完成（过期不自动改期）；due = 今天到期且未结束；
// pending = 等本人决定的事项（初筛中的不列）；stale = open 的任务与承诺超过 StaleAfter 无任何变更，且没有未来的计划日期。
func TodayItems(tx *store.Tx, now time.Time) (TodayList, error) {
	l := TodayList{Planned: []domain.Item{}, Due: []domain.Item{}, Pending: []domain.Item{}, Stale: []domain.Item{}}
	all, err := List(tx, Filter{})
	if err != nil {
		return l, err
	}
	today := Today(now)
	for _, it := range all {
		if it.PlannedFor != "" && it.PlannedFor <= today && active(it) {
			l.Planned = append(l.Planned, it)
		}
		if it.Deadline != nil && Today(*it.Deadline) == today && active(it) {
			l.Due = append(l.Due, it)
		}
		if it.Phase == domain.PhaseNeedsInput && (it.Ask == nil || !it.Ask.ScreeningPending) {
			l.Pending = append(l.Pending, it)
		}
		if it.Phase == domain.PhaseOpen && (it.Kind == domain.KindTask || it.Kind == domain.KindCommitment) &&
			now.Sub(it.UpdatedAt) > StaleAfter && it.PlannedFor <= today {
			l.Stale = append(l.Stale, it)
		}
	}
	slices.SortStableFunc(l.Planned, func(a, b domain.Item) int { return strings.Compare(a.PlannedFor, b.PlannedFor) })
	return l, nil
}
