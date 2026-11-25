package domain

import (
	"reflect"
	"testing"
	"time"
)

// 统一时间字段到事项时间的唯一一处映射（§9.2、§9.3）：只到日的截止落成当天 23:59:59；
// 全天或只到日的开始只给计划日期；同一天的定时起止给时段；跨天只给开始那天。
func TestEventSchedule(t *testing.T) {
	loc := time.FixedZone("+08", 8*3600)
	cases := []struct {
		name string
		ev   SourceEvent
		want EventSchedule
	}{
		{"date-only due", SourceEvent{DueAt: "2026-12-02"}, EventSchedule{
			Deadline: ptrTime(time.Date(2026, 12, 2, 23, 59, 59, 0, loc)), From: []string{"deadline"}}},
		{"timed due", SourceEvent{DueAt: "2026-12-02T09:30:00Z"}, EventSchedule{
			Deadline: ptrTime(time.Date(2026, 12, 2, 17, 30, 0, 0, loc)), From: []string{"deadline"}}},
		{"all-day start only", SourceEvent{StartsAt: "2026-12-02", AllDay: true}, EventSchedule{
			PlannedFor: "2026-12-02", From: []string{"planned_for"}}},
		{"all-day with end next day is still one day", SourceEvent{StartsAt: "2026-12-02", EndsAt: "2026-12-03", AllDay: true}, EventSchedule{
			PlannedFor: "2026-12-02", From: []string{"planned_for"}}},
		{"timed same day gives a slot", SourceEvent{StartsAt: "2026-12-02T01:00:00Z", EndsAt: "2026-12-02T02:30:00Z"}, EventSchedule{
			PlannedFor: "2026-12-02", Slot: "09:00 - 10:30", From: []string{"planned_for", "planned_slot"}}},
		{"cross-day gives only the start day", SourceEvent{StartsAt: "2026-12-02T15:00:00Z", EndsAt: "2026-12-03T01:00:00Z"}, EventSchedule{
			PlannedFor: "2026-12-02", From: []string{"planned_for"}}},
		{"due and starts together", SourceEvent{DueAt: "2026-12-05", StartsAt: "2026-12-02"}, EventSchedule{
			Deadline: ptrTime(time.Date(2026, 12, 5, 23, 59, 59, 0, loc)), PlannedFor: "2026-12-02", From: []string{"deadline", "planned_for"}}},
		{"no time fields", SourceEvent{}, EventSchedule{}},
	}
	for _, c := range cases {
		if got := c.ev.Schedule(loc); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
