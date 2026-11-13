package search

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func itemIDs(its []domain.Item) string {
	var s []string
	for _, it := range its {
		s = append(s, it.ID)
	}
	return strings.Join(s, ",")
}

func TestFilterItemsFullText(t *testing.T) {
	its := []domain.Item{
		{ID: "a", Title: "护照续签"},
		{ID: "b", Body: "需要办理护照续签手续"},
		{ID: "c", Title: "无关"},
		{ID: "d", Title: "旅行"},
		{ID: "e", Summary: "Renew Passport"},
	}
	prog := map[string]string{"d": " 等护照照片"}
	got, err := FilterItems(its, prog, ItemQuery{Text: "护照"})
	if err != nil || itemIDs(got) != "a,b,d" {
		t.Fatalf("护照 = %s %v", itemIDs(got), err)
	}
	if got, _ := FilterItems(its, prog, ItemQuery{Text: "passport"}); itemIDs(got) != "e" {
		t.Fatalf("passport = %s", itemIDs(got))
	}
	if got, _ := FilterItems(its, prog, ItemQuery{Text: "护照 照片"}); itemIDs(got) != "d" {
		t.Fatalf("all terms must match = %s", itemIDs(got))
	}
	if _, err := FilterItems(its, nil, ItemQuery{Text: "!!"}); err == nil {
		t.Fatal("text without searchable characters must be rejected")
	}
}

func TestFilterItemsSortAndPlannedFor(t *testing.T) {
	d := func(day int) *time.Time { x := time.Date(2026, 5, day, 0, 0, 0, 0, time.UTC); return &x }
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, dl *time.Time, plan string, p domain.Priority, upd int) domain.Item {
		return domain.Item{ID: id, Deadline: dl, PlannedFor: plan, Priority: p, Tags: []string{"t" + id},
			ItemKernelState: domain.ItemKernelState{UpdatedAt: base.AddDate(0, 0, upd)}}
	}
	its := []domain.Item{
		mk("1", nil, "", "", 1),
		mk("2", d(9), "2026-05-03", domain.PriorityLow, 4),
		mk("3", d(2), "2026-05-01", domain.PriorityUrgent, 2),
		mk("4", d(5), "2026-05-03", domain.PriorityHigh, 3),
	}
	for sort, want := range map[string]string{
		"deadline": "3,4,2,1", "planned_for": "3,2,4,1", "priority": "3,4,2,1", "updated": "2,4,3,1",
	} {
		got, err := FilterItems(its, nil, ItemQuery{Sort: sort})
		if err != nil || itemIDs(got) != want {
			t.Errorf("sort %s = %s (%v), want %s", sort, itemIDs(got), err, want)
		}
	}
	if itemIDs(its) != "1,2,3,4" {
		t.Fatal("input must not be reordered")
	}
	if got, _ := FilterItems(its, nil, ItemQuery{PlannedFor: "2026-05-03"}); itemIDs(got) != "2,4" {
		t.Fatalf("planned_for filter = %s", itemIDs(got))
	}
	if got, _ := FilterItems(its, nil, ItemQuery{Tag: "t3"}); itemIDs(got) != "3" {
		t.Fatalf("tag filter = %s", itemIDs(got))
	}
	for _, bad := range []ItemQuery{{Sort: "nope"}, {PlannedFor: "2026-13-40"}} {
		if _, err := FilterItems(its, nil, bad); err == nil {
			t.Fatalf("%+v must be rejected", bad)
		}
	}
}
