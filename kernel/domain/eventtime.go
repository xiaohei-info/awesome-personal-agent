package domain

import (
	"fmt"
	"strings"
	"time"
)

// EventTime 是统一时间字段的解析结果：DateOnly 表示写法只到日，At 是当天零点（UTC，只用日期本身）。
// 只到日的语义是某天结束前，落成事项的截止时由 Schedule 按目标时区补 23:59:59。
type EventTime struct {
	At       time.Time
	DateOnly bool
}

// ParseEventTime 解析统一时间字段（due_at、starts_at、ends_at）：只到日或 RFC3339，其他写法不认。
func ParseEventTime(s string) (EventTime, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return EventTime{}, false
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return EventTime{At: t, DateOnly: true}, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return EventTime{At: t}, true
	}
	return EventTime{}, false
}

// EventSchedule 是来源事件的统一时间字段映射出的事项时间（§9.2、§9.3）。
// From 记哪几个字段来自来源（deadline、planned_for、planned_slot），供溯源标记与来源刷新判断。
type EventSchedule struct {
	Deadline   *time.Time
	PlannedFor string
	Slot       string
	From       []string
}

// Schedule 是把来源事件的时间落成事项时间的唯一一处解析：不按来源类型分支，只认统一字段。
// 只到日的截止按 loc 落成当天 23:59:59（§6.3）；全天或只到日的开始时间只给计划日期、不给时段；
// 起止同在本地的同一天且有钟点时给时段，跨天只给开始那天的计划日期。
func (e SourceEvent) Schedule(loc *time.Location) EventSchedule {
	var out EventSchedule
	if t, ok := ParseEventTime(e.DueAt); ok {
		d := t.At
		if t.DateOnly {
			d = time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc)
		} else {
			d = d.In(loc)
		}
		out.Deadline = &d
		out.From = append(out.From, "deadline")
	}
	start, ok := ParseEventTime(e.StartsAt)
	if !ok {
		return out
	}
	s := start.At
	if start.DateOnly {
		s = time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, loc)
	} else {
		s = s.In(loc)
	}
	end, hasEnd := ParseEventTime(e.EndsAt)
	out.PlannedFor = s.Format(time.DateOnly)
	out.From = append(out.From, "planned_for")
	if e.AllDay || start.DateOnly || !hasEnd || end.DateOnly {
		return out
	}
	x := end.At.In(loc)
	if x.Format(time.DateOnly) != out.PlannedFor || !x.After(s) {
		return out
	}
	slot := fmt.Sprintf("%s - %s", s.Format("15:04"), x.Format("15:04"))
	if _, _, ok := ParseSlot(slot); !ok {
		return out
	}
	out.Slot = slot
	out.From = append(out.From, "planned_slot")
	return out
}
