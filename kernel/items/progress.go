package items

import (
	"cmp"
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
	ID      string  `json:"id"`
	Text    string  `json:"text"`
	On      *string `json:"on"`
	ReplyTo string  `json:"reply_to"`
}

func insertProgress(tx *store.Tx, item string, p domain.Progress) error {
	_, err := tx.Exec(`INSERT INTO item_progress (id, item, text, on_date, actor, at, precision, reply_to, mentions) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, item, p.Text, p.On, p.Actor, p.At.UnixNano(), p.Precision, p.ReplyTo, strings.Join(p.Mentions, ","))
	return err
}

func splitMentions(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// CheckComment 是一条发言的输入校验（item.progress 与带 @ 的发言共用）：正文非空、所指日期合法、回复的是同一事项的一条进展。
// "不晚于今天"要用命令时刻，由 AppendComment 判。
func CheckComment(tx *store.Tx, item, text string, on *string, reply string) error {
	if blank(text) {
		return invalid("text is required")
	}
	if on != nil {
		if err := checkDate("on", *on); err != nil {
			return err
		}
	}
	return CheckReplyTo(tx, item, reply)
}

// CheckReplyTo 要求 reply 是事项 item 上已有的一条进展（讨论区的回复只挂在同一事项内，§6.3）；空串表示不是回复。
func CheckReplyTo(tx *store.Tx, item, reply string) error {
	if reply == "" {
		return nil
	}
	r, err := tx.Query(`SELECT 1 FROM item_progress WHERE id = ? AND item = ?`, reply, item)
	if err != nil {
		return err
	}
	found := r.Next()
	r.Close()
	if err := r.Err(); err != nil {
		return err
	}
	if !found {
		return domain.NewError(domain.CodeInvalidRequest, "reply_to "+reply+" is not a comment on item "+item, "read the thread with apa item thread "+item+" and reply to one of its comments")
	}
	return nil
}

// Progress 按记录先后返回事项的进展；没有时返回 nil。
func Progress(tx *store.Tx, item string) ([]domain.Progress, error) {
	r, err := tx.Query(`SELECT id, text, on_date, actor, at, precision, reply_to, mentions FROM item_progress WHERE item = ? ORDER BY at, id`, item)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []domain.Progress
	for r.Next() {
		var p domain.Progress
		var at int64
		var mentions string
		if err := r.Scan(&p.ID, &p.Text, &p.On, &p.Actor, &at, &p.Precision, &p.ReplyTo, &mentions); err != nil {
			return nil, err
		}
		p.At, p.Mentions = time.Unix(0, at).UTC(), splitMentions(mentions)
		out = append(out, p)
	}
	return out, r.Err()
}

// ProgressIn 返回进展日落在 [from, to]（YYYY-MM-DD，含首尾）里的进展，按事项分组、各自按记录先后。
// 进展日是条目的所指日期 on；没写 on 的取记录时刻所在的本机日（与 Today 同一日界）。「已完成」页按周数进展用它（附录 B.2）。
func ProgressIn(tx *store.Tx, from, to string) (map[string][]domain.Progress, error) {
	f, err := time.ParseInLocation(dateLayout, from, time.Local)
	if err != nil {
		return nil, err
	}
	t, err := time.ParseInLocation(dateLayout, to, time.Local)
	if err != nil {
		return nil, err
	}
	r, err := tx.Query(`SELECT item, id, text, on_date, actor, at, precision, reply_to, mentions FROM item_progress
		WHERE (on_date <> '' AND on_date BETWEEN ? AND ?) OR (on_date = '' AND at >= ? AND at < ?) ORDER BY at, id`,
		from, to, f.UnixNano(), t.AddDate(0, 0, 1).UnixNano())
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string][]domain.Progress{}
	for r.Next() {
		var item, mentions string
		var p domain.Progress
		var at int64
		if err := r.Scan(&item, &p.ID, &p.Text, &p.On, &p.Actor, &at, &p.Precision, &p.ReplyTo, &mentions); err != nil {
			return nil, err
		}
		p.At, p.Mentions = time.Unix(0, at).UTC(), splitMentions(mentions)
		out[item] = append(out[item], p)
	}
	return out, r.Err()
}

func (s *svc) progress() command.Spec {
	parse := func(in json.RawMessage) (progressIn, error) {
		var v progressIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "item.progress", Action: authz.ActionUpdateOwn, Group: authz.GroupOrganize,
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
			return editActions(boundOr(tx, c, o), false, false)
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			return CheckComment(tx, v.ID, v.Text, v.On, v.ReplyTo)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			p := domain.Progress{Text: v.Text, ReplyTo: v.ReplyTo, Actor: c.Caller.Actor.Principal}
			if v.On != nil {
				p.On = *v.On
			}
			w, err := AppendComment(c.Tx, v.ID, p, c.Now())
			if err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload})
			return ref{v.ID}, err
		}}
}

// ---- 今天 ----

// TodayList 是"今天"的六组（§4.1、§7.1）。每组都不为 nil；一件事可以同时在几组里（如计划今天又高优先级），由读者去重。
type TodayList struct {
	Planned []domain.Item `json:"planned"`
	Due     []domain.Item `json:"due"`
	Pending []domain.Item `json:"pending"`
	Stale   []domain.Item `json:"stale"`
	// Overdue：截止所在的本机日早于今天、还没结束的，按截止先后。
	Overdue []domain.Item `json:"overdue"`
	// HighPriority：优先级为紧急或高、还没结束的，紧急在前。
	HighPriority []domain.Item `json:"high_priority"`
}

func active(it domain.Item) bool {
	return it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting || it.Phase == domain.PhaseNeedsInput
}

// TodayItems 取材：planned = 计划今天或更早且未完成（过期不自动改期）；due = 今天到期且未结束；
// pending = 等本人决定的事项（初筛中的不列）；stale = open 的任务与承诺超过 StaleAfter 无任何变更，且没有未来的计划日期。
func TodayItems(tx *store.Tx, now time.Time) (TodayList, error) {
	return TodayItemsAfter(tx, now, StaleAfter)
}

// TodayItemsAfter 同 TodayItems，久未推进的阈值由调用方给（能力包 stale 规则的 stale_after）。
func TodayItemsAfter(tx *store.Tx, now time.Time, staleAfter time.Duration) (TodayList, error) {
	l := TodayList{Planned: []domain.Item{}, Due: []domain.Item{}, Pending: []domain.Item{}, Stale: []domain.Item{}, Overdue: []domain.Item{}, HighPriority: []domain.Item{}}
	all, err := List(tx, Filter{})
	if err != nil {
		return l, err
	}
	today := Today(now)
	imported, err := importedAt(tx)
	if err != nil {
		return l, err
	}
	for _, it := range all {
		if it.PlannedFor != "" && it.PlannedFor <= today && active(it) {
			l.Planned = append(l.Planned, it)
		}
		if it.Deadline != nil && Today(*it.Deadline) == today && active(it) {
			l.Due = append(l.Due, it)
		}
		if it.Deadline != nil && Today(*it.Deadline) < today && active(it) {
			l.Overdue = append(l.Overdue, it)
		}
		if (it.Priority == domain.PriorityUrgent || it.Priority == domain.PriorityHigh) && active(it) {
			l.HighPriority = append(l.HighPriority, it)
		}
		if it.Phase == domain.PhaseNeedsInput {
			l.Pending = append(l.Pending, it)
		}
		if it.Phase == domain.PhaseOpen && (it.Kind == domain.KindTask || it.Kind == domain.KindCommitment) &&
			now.Sub(laterOf(it.UpdatedAt, imported[it.ID])) > staleAfter && it.PlannedFor <= today {
			l.Stale = append(l.Stale, it)
		}
	}
	// 同一天里有时段的在前、按开始时间排，没时段的在后（§7.1）；HH:MM 补零，字符串序即时间序。
	slices.SortStableFunc(l.Planned, func(a, b domain.Item) int {
		return cmp.Or(strings.Compare(a.PlannedFor, b.PlannedFor), strings.Compare(slotKey(a), slotKey(b)))
	})
	slices.SortStableFunc(l.Overdue, func(a, b domain.Item) int { return a.Deadline.Compare(*b.Deadline) })
	slices.SortStableFunc(l.HighPriority, func(a, b domain.Item) int {
		return cmp.Compare(b2i(a.Priority != domain.PriorityUrgent), b2i(b.Priority != domain.PriorityUrgent))
	})
	return l, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// slotKey 让没有时段的排在有时段的之后（"~" 大于任何数字）。
func slotKey(it domain.Item) string {
	if it.PlannedSlot == "" {
		return "~"
	}
	return it.PlannedSlot
}

// importedAt 返回导入事项的实际写入时刻（items.imported 里的 at）；非导入事项不在表里。
func importedAt(tx *store.Tx) (map[string]time.Time, error) {
	r, err := tx.Query(`SELECT id, imported FROM items WHERE imported <> ''`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]time.Time{}
	for r.Next() {
		var id, raw string
		if err := r.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var im struct {
			At time.Time `json:"at"`
		}
		if err := json.Unmarshal([]byte(raw), &im); err != nil {
			return nil, fmt.Errorf("items: %s: corrupt imported: %w", id, err)
		}
		out[id] = im.At
	}
	return out, r.Err()
}

// laterOf 取较晚者：久未推进从导入时刻与最后一次非导入变更中较晚的一个起算（§7.3）。
// 事项的 updated_at 按事件的 at 折叠，导入事件的 at 是过去的原始时刻，所以与导入时刻取较晚者即可。
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
