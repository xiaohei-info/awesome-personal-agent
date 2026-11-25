package runner

import (
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 统一时间字段（§9.2）：只认 YYYY-MM-DD 与 RFC3339；有结束必须有开始且不早于开始；全天要求只到日。
// 格式不合的事件整行拒收，与 metrics、items 同一处置；旧来源程序不带这些字段时照常入账。
func TestParseEventTimes(t *testing.T) {
	base := func(extra string) []byte {
		return []byte(`{"id":"e1","type":"reminder.due","subject":"交材料","occurred_at":"2026-12-01T00:00:00Z"` + extra + "}\n")
	}
	ok := []string{
		`,"due_at":"2026-12-02"`,
		`,"due_at":"2026-12-02T09:30:00+08:00"`,
		`,"starts_at":"2026-12-02","ends_at":"2026-12-03","all_day":true`,
		`,"starts_at":"2026-12-02T09:00:00+08:00","ends_at":"2026-12-02T10:00:00+08:00"`,
	}
	for _, extra := range ok {
		if p := parse(base(extra), 1<<20); len(p.events) != 1 || len(p.rejects) != 0 {
			t.Errorf("%s should be accepted: events=%d rejects=%+v", extra, len(p.events), p.rejects)
		}
	}
	bad := map[string]string{
		`,"due_at":"2026/12/02"`:                             "due_at",
		`,"starts_at":"下周三"`:                                 "starts_at",
		`,"ends_at":"2026-12-03"`:                            "ends_at requires starts_at",
		`,"starts_at":"2026-12-03","ends_at":"2026-12-02"`:   "ends_at must not be before starts_at",
		`,"starts_at":"2026-12-02T09:00:00Z","all_day":true`: "all_day requires date-only",
		`,"all_day":true`:                                    "all_day requires starts_at",
	}
	for extra, want := range bad {
		p := parse(base(extra), 1<<20)
		if len(p.events) != 0 || len(p.rejects) != 1 || !strings.Contains(p.rejects[0].Reason, want) {
			t.Errorf("%s should be rejected with %q: events=%d rejects=%+v", extra, want, len(p.events), p.rejects)
		}
	}
}

// 统一时间字段随事件落账、读回原样；没有它们的旧事件读回为空（迁移只增列）。
func TestEventTimesPersistAndReadBack(t *testing.T) {
	reg := store.NewRegistry()
	if err := reg.Register(Module); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := domain.Source{ID: "src_cal"}
	err = st.Update(bg, func(tx *store.Tx) error {
		if err := insertEvent(tx, src, "", parsedEvent{ID: "old", Type: "event.created", Subject: "旧", OccurredAt: t0}, t0); err != nil {
			return err
		}
		return insertEvent(tx, src, "", parsedEvent{ID: "cal", Type: "event.created", Subject: "周会", OccurredAt: t0,
			StartsAt: "2026-12-02T09:00:00+08:00", EndsAt: "2026-12-02T10:00:00+08:00", DueAt: "2026-12-02"}, t0)
	})
	if err != nil {
		t.Fatal(err)
	}
	var evs []domain.SourceEvent
	if err := st.View(bg, func(tx *store.Tx) (err error) { evs, err = Events(tx, src.ID); return }); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].DueAt != "" || evs[0].StartsAt != "" || evs[0].EndsAt != "" || evs[0].AllDay {
		t.Fatalf("old event: %+v", evs[0])
	}
	got := evs[1]
	if got.DueAt != "2026-12-02" || got.StartsAt != "2026-12-02T09:00:00+08:00" || got.EndsAt != "2026-12-02T10:00:00+08:00" || got.AllDay {
		t.Fatalf("event times: %+v", got)
	}
}
