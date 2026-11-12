package delivery

import (
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 打扰控制的日上限与静音（设计 §7.4）：被压的提醒改走 Web 未读，原因记在 Suppressed。
const (
	SuppressedDailyCap = "daily_cap"
	SuppressedMuted    = "muted"

	// noticePrefix 是超限说明的 id 前缀，后接自然日；说明不计入日上限。
	noticePrefix = "capnotice:"
	// noticeItem 与触达的合并摘要共用占位事项（attention.DigestItem）：说明不属于任何事项，不随事项变化而作废。
	noticeItem = "attention:digest"
)

// Target 是一条投递要对照静音与豁免的对象。Item 取自投递本身；规则 alert 出口没有事项，只有 Source。
type Target struct {
	Source, Project string
	// Exempt：开启了截止豁免的临期决策，不受日上限（静音仍生效）。
	Exempt bool
}

// CreateGated 在 Create 之前套用日上限与静音：
//   - 静音：投递的事项、项目或来源命中未到期的静音，决策/提醒/简报改为只留 Web 未读（含注入路由）；
//   - 日上限：经渠道推送的决策/提醒/简报每个自然日不超过 DailyCap 条（0 不限），超出的只留 Web 未读，
//     并在当天第一次超限时补发一条说明；
//   - 健康类、唤醒类、web_only 决策、豁免的临期决策不受日上限；健康与唤醒也不受静音；注入与 Web 路由不计入日上限。
//
// 自然日按静默时段的时区算，未设置取 UTC。同 id 重复调用返回首次结果，不重复计数或补发说明。
func (q *Queue) CreateGated(tx *store.Tx, pol policy.Policy, d domain.Delivery, t Target) (domain.Delivery, bool, error) {
	if old, err := q.Get(tx, d.ID); err == nil {
		return old, false, nil
	} else if e, ok := err.(*domain.Error); !ok || e.Code != domain.CodeNotFound {
		return d, false, err
	}
	if d.Class != domain.DeliveryHealth && d.Class != domain.DeliveryWake {
		now := q.o.Now()
		if d.Route != RouteWeb && muted(pol.Disturb.Mutes, d.Item, t, now) {
			d.Route, d.Suppressed = RouteWeb, SuppressedMuted
		} else if over, err := q.overCap(tx, pol, d, t, now); err != nil {
			return d, false, err
		} else if over {
			notice := d
			d.Route, d.Suppressed = RouteWeb, SuppressedDailyCap
			if err := q.noticeOnce(tx, pol, notice, now); err != nil {
				return d, false, err
			}
		}
	}
	return q.Create(tx, d)
}

// Muted 报告该事项/对象此刻是否被静音；触达据此把合并摘要里被静音的成员拆出来单独走 Web。
func (q *Queue) Muted(pol policy.Policy, item string, t Target) bool {
	return muted(pol.Disturb.Mutes, item, t, q.o.Now())
}

func muted(mutes []policy.Mute, item string, t Target, now time.Time) bool {
	for _, m := range mutes {
		if m.Until != nil && !now.Before(*m.Until) {
			continue
		}
		switch {
		case m.Scope == policy.MuteItem && m.ID == item,
			m.Scope == policy.MuteProject && t.Project != "" && m.ID == t.Project,
			m.Scope == policy.MuteSource && t.Source != "" && m.ID == t.Source:
			return true
		}
	}
	return false
}

// dayBounds 返回 now 所在自然日的 [起, 止) 与日期标签。
func dayBounds(pol policy.Policy, now time.Time) (time.Time, time.Time, string) {
	loc := time.UTC
	if q := pol.Disturb.Quiet; q != nil && q.Zone != "" {
		if l, err := time.LoadLocation(q.Zone); err == nil { // 策略写入时已校验
			loc = l
		}
	}
	l := now.In(loc)
	start := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1), start.Format("2006-01-02")
}

func (q *Queue) overCap(tx *store.Tx, pol policy.Policy, d domain.Delivery, t Target, now time.Time) (bool, error) {
	limit := pol.Disturb.DailyCap
	if limit <= 0 || !strings.HasPrefix(d.Route, "channel:") || t.Exempt || d.Content.WebOnly {
		return false, nil
	}
	from, to, _ := dayBounds(pol, now)
	var n int64
	err := query(tx, func(r *store.Rows) error { return r.Scan(&n) },
		`SELECT COUNT(*) FROM delivery_delivery
		WHERE route LIKE 'channel:%' AND class IN ('decision', 'reminder', 'brief') AND suppressed = ''
		AND id NOT LIKE ? AND created_at >= ? AND created_at < ?`, noticePrefix+"%", ns(from), ns(to))
	return n >= int64(limit), err
}

// noticeOnce 当天第一次超限时补发一条说明（走被压提醒原本的渠道）；之后同一天不再发。
func (q *Queue) noticeOnce(tx *store.Tx, pol policy.Policy, over domain.Delivery, now time.Time) error {
	_, _, day := dayBounds(pol, now)
	_, _, err := q.Create(tx, domain.Delivery{
		ID: noticePrefix + day, Item: noticeItem, Class: domain.DeliveryReminder, Route: over.Route, Version: 1,
		Content: domain.DeliveryContent{
			Title:   "今天的提醒已达日上限",
			Summary: "今天经渠道推送的提醒已达上限，超出的提醒只留在 Web 未读：目前另有 1 条，之后超出的同样只在 Web，请到 Web 查看。",
		},
	})
	return err
}
