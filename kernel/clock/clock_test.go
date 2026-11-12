package clock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fixture struct {
	st   *store.Store
	now  time.Time
	got  []Command
	fail error
	c    *Clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{Module, eventlog.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{st: st, now: t0}
	f.c, err = New(st, func(tx *store.Tx, c Command) error {
		if f.fail != nil {
			return f.fail
		}
		f.got = append(f.got, c)
		// handler 在同一事务里写 Event：到期与命令效果原子。
		_, err := eventlog.Append(tx, domain.Event{Type: "timer.fired", Object: c.Target, Actor: c.Actor, Payload: []byte(`{}`)})
		return err
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) add(t *testing.T, tm domain.Timer) string {
	t.Helper()
	var id string
	err := f.st.Update(context.Background(), func(tx *store.Tx) (err error) { id, err = Add(tx, tm); return })
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) tick(t *testing.T) int {
	t.Helper()
	n, err := f.c.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) events(t *testing.T) int {
	var n int
	f.st.View(context.Background(), func(tx *store.Tx) error {
		h, err := eventlog.Head(tx)
		n = int(h)
		return err
	})
	return n
}

func TestDueFiresSystemCommandOnce(t *testing.T) {
	f := newFixture(t)
	f.add(t, domain.Timer{Target: "item-1", Kind: domain.TimerDue, At: t0.Add(time.Hour)})
	if f.tick(t) != 0 || len(f.got) != 0 {
		t.Fatal("fired before due")
	}
	f.now = t0.Add(time.Hour)
	if f.tick(t) != 1 || len(f.got) != 1 {
		t.Fatalf("got %d commands", len(f.got))
	}
	c := f.got[0]
	if c.Target != "item-1" || c.Kind != domain.TimerDue || c.Actor.Type != domain.ActorSystem || c.Actor.Via != domain.ViaClock {
		t.Fatalf("command %+v", c)
	}
	if f.tick(t) != 0 || len(f.got) != 1 {
		t.Fatal("one-shot fired twice")
	}
	if f.events(t) != 1 {
		t.Fatal("handler event missing")
	}
}

func TestOneShotAfterDowntimeFiresAll(t *testing.T) {
	f := newFixture(t)
	for i, k := range []domain.TimerKind{domain.TimerRemind, domain.TimerFollowUp, domain.TimerWaitUntil} {
		f.add(t, domain.Timer{Target: "item-1", Kind: k, At: t0.Add(time.Duration(i+1) * time.Hour)})
	}
	f.now = t0.Add(10 * time.Hour) // 内核停机后醒来
	if n := f.tick(t); n != 3 {
		t.Fatalf("fired %d, want 3", n)
	}
	if f.got[0].Kind != domain.TimerRemind || f.got[2].Kind != domain.TimerWaitUntil {
		t.Fatalf("order %+v", f.got)
	}
}

func TestRecurCatchesUpOnlyLatestMissed(t *testing.T) {
	f := newFixture(t)
	f.add(t, domain.Timer{Target: "pack-1", Kind: domain.TimerRecur, At: t0.Add(24 * time.Hour), Recurrence: "24h"})
	f.now = t0.Add(24*time.Hour*5 + time.Hour) // 错过第 1..5 次
	if f.tick(t) != 1 || len(f.got) != 1 {
		t.Fatalf("fired %d", len(f.got))
	}
	if want := t0.Add(24 * time.Hour * 5); !f.got[0].Due.Equal(want) {
		t.Fatalf("due %v want latest missed %v", f.got[0].Due, want)
	}
	if f.tick(t) != 0 {
		t.Fatal("refired in same period")
	}
	f.now = t0.Add(24 * time.Hour * 6) // 下一次落在原节拍上
	if f.tick(t) != 1 || !f.got[1].Due.Equal(f.now) {
		t.Fatalf("next occurrence %+v", f.got)
	}
}

func TestHandlerFailureRollsBackAndRetries(t *testing.T) {
	f := newFixture(t)
	f.add(t, domain.Timer{Target: "i", Kind: domain.TimerDue, At: t0})
	f.add(t, domain.Timer{Target: "j", Kind: domain.TimerDue, At: t0})
	f.fail = errors.New("boom")
	if _, err := f.c.Tick(context.Background()); !errors.Is(err, f.fail) {
		t.Fatalf("err %v", err)
	}
	if f.events(t) != 0 {
		t.Fatal("partial effect leaked")
	}
	f.fail = nil
	if f.tick(t) != 2 {
		t.Fatal("timers lost after handler failure")
	}
}

func TestAddInCallerTxRollsBackAndCancel(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("rollback")
	err := f.st.Update(context.Background(), func(tx *store.Tx) error {
		if _, err := Add(tx, domain.Timer{Target: "i", Kind: domain.TimerDue, At: t0}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || f.tick(t) != 0 {
		t.Fatal("timer survived caller rollback")
	}
	id := f.add(t, domain.Timer{Target: "i", Kind: domain.TimerDue, At: t0})
	if err := f.st.Update(context.Background(), func(tx *store.Tx) error { return Cancel(tx, id) }); err != nil {
		t.Fatal(err)
	}
	if f.tick(t) != 0 {
		t.Fatal("cancelled timer fired")
	}
	err = f.st.Update(context.Background(), func(tx *store.Tx) error { return Cancel(tx, id) })
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeNotFound {
		t.Fatalf("cancel missing: %v", err)
	}
}

func TestAddValidation(t *testing.T) {
	f := newFixture(t)
	bad := []domain.Timer{
		{Kind: domain.TimerDue, At: t0},
		{Target: "i", Kind: "nope", At: t0},
		{Target: "i", Kind: domain.TimerDue},
		{Target: "i", Kind: domain.TimerRecur, At: t0},
		{Target: "i", Kind: domain.TimerRecur, At: t0, Recurrence: "soon"},
		{Target: "i", Kind: domain.TimerRecur, At: t0, Recurrence: "0s"},
		{Target: "i", Kind: domain.TimerDue, At: t0, Recurrence: "1h"},
	}
	for i, tm := range bad {
		err := f.st.Update(context.Background(), func(tx *store.Tx) error { _, err := Add(tx, tm); return err })
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
			t.Errorf("case %d: err %v", i, err)
		}
	}
	if _, err := New(f.st, nil, nil); err == nil {
		t.Error("nil handler accepted")
	}
}

func TestRunScansUntilCancelled(t *testing.T) {
	f := newFixture(t)
	f.add(t, domain.Timer{Target: "i", Kind: domain.TimerDue, At: t0})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.c.Run(ctx, 5*time.Millisecond) }()
	deadline := time.After(5 * time.Second)
	for f.events(t) == 0 {
		select {
		case <-deadline:
			t.Fatal("loop never fired")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run returned %v", err)
	}
}

func TestScheduleNext(t *testing.T) {
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, time.UTC) }
	for _, c := range []struct {
		spec  string
		after time.Time
		want  time.Time
	}{
		{"CRON_TZ=UTC 0 8 * * 1-5", at(2026, 1, 1, 8, 0), at(2026, 1, 2, 8, 0)}, // 周四 08:00 之后是周五
		{"CRON_TZ=UTC 0 8 * * 1-5", at(2026, 1, 2, 9, 0), at(2026, 1, 5, 8, 0)}, // 周五之后跳过周末
		{"CRON_TZ=UTC */15 * * * *", at(2026, 1, 1, 8, 7), at(2026, 1, 1, 8, 15)},
		{"CRON_TZ=UTC 30 9,17 * * *", at(2026, 1, 1, 9, 30), at(2026, 1, 1, 17, 30)},
		{"CRON_TZ=UTC 5-10/5 0 * * *", at(2026, 1, 1, 0, 5), at(2026, 1, 1, 0, 10)},
		{"CRON_TZ=UTC 0 0 1 * *", at(2026, 1, 31, 12, 0), at(2026, 2, 1, 0, 0)},
		{"CRON_TZ=UTC 0 0 29 2 *", at(2026, 3, 1, 0, 0), at(2028, 2, 29, 0, 0)},
		{"CRON_TZ=UTC 0 0 * * 7", at(2026, 1, 1, 0, 0), at(2026, 1, 4, 0, 0)},           // 7 即周日
		{"CRON_TZ=UTC 0 0 13 * 5", at(2026, 2, 1, 0, 0), at(2026, 2, 6, 0, 0)},          // 日与周都限定：并集，先到者（周五 6 日）
		{"CRON_TZ=Asia/Shanghai 0 8 * * *", at(2026, 1, 1, 0, 0), at(2026, 1, 2, 0, 0)}, // 上海 08:00 = UTC 00:00，严格晚于
		{"24h", at(2026, 1, 1, 8, 0), at(2026, 1, 2, 8, 0)},
	} {
		s, err := ParseSchedule(c.spec)
		if err != nil {
			t.Fatalf("%q: %v", c.spec, err)
		}
		if got := s.Next(c.after); !got.Equal(c.want) {
			t.Errorf("%q after %v = %v, want %v", c.spec, c.after, got, c.want)
		}
	}
	// 缺省时区是本机时区：不带前缀的 cron 在 time.Local 的墙上时间触发。
	at0, err := Start("0 8 * * 1-5", t0)
	if err != nil || at0.In(time.Local).Hour() != 8 || at0.In(time.Local).Minute() != 0 || at0.In(time.Local).Weekday() == time.Saturday || at0.In(time.Local).Weekday() == time.Sunday || !at0.After(t0) {
		t.Fatalf("local default: %v %v", at0, err)
	}
}

func TestScheduleRejectsBadSpecs(t *testing.T) {
	for _, s := range []string{"", "0s", "-1h", "daily", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "CRON_TZ=Nowhere/Land * * * * *", "CRON_TZ= * * * * *"} {
		if _, err := ParseSchedule(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if _, err := Start("0 0 31 2 *", t0); err == nil {
		t.Error("an expression that never fires must be rejected at creation")
	}
}

// cron 的周期定时与时长一样：同一节拍只触发一次，停机错过多个节拍只补最近一次，下一次落回原节拍。
func TestRecurCronCatchesUpOnlyLatestMissed(t *testing.T) {
	f := newFixture(t)
	const spec = "CRON_TZ=UTC 0 8 * * 1-5"
	first, err := Start(spec, t0) // t0 = 2026-01-01 周四 00:00
	if err != nil || !first.Equal(t0.Add(8*time.Hour)) {
		t.Fatalf("first %v %v", first, err)
	}
	f.add(t, domain.Timer{Target: "pack-1", Kind: domain.TimerRecur, At: first, Recurrence: spec})
	f.now = first
	if f.tick(t) != 1 || f.tick(t) != 0 {
		t.Fatal("one beat must fire once")
	}
	// 停机跨过周五、周一、周二、周三，周三 12:00 醒来：只补周三 08:00。
	f.now = time.Date(2026, 1, 7, 12, 0, 0, 0, time.UTC)
	if f.tick(t) != 1 || len(f.got) != 2 {
		t.Fatalf("catch-up fired %d", len(f.got))
	}
	if want := time.Date(2026, 1, 7, 8, 0, 0, 0, time.UTC); !f.got[1].Due.Equal(want) {
		t.Fatalf("due %v want %v", f.got[1].Due, want)
	}
	f.now = time.Date(2026, 1, 8, 8, 0, 0, 0, time.UTC) // 下一拍：周四
	if f.tick(t) != 1 || !f.got[2].Due.Equal(f.now) {
		t.Fatalf("next beat %+v", f.got)
	}
}

// Command.Late 是实际触发比 Timer 登记时刻晚多久；周期定时按第一个错过的节拍算，而不是补发的最近一拍。
func TestCommandLateIsMeasuredFromScheduledTime(t *testing.T) {
	f := newFixture(t)
	if err := f.st.Update(context.Background(), func(tx *store.Tx) error {
		if _, err := Add(tx, domain.Timer{ID: "once", Target: "a", Kind: domain.TimerDue, At: t0}); err != nil {
			return err
		}
		_, err := Add(tx, domain.Timer{ID: "rec", Target: "b", Kind: domain.TimerRecur, At: t0, Recurrence: "1m"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.now = t0.Add(8 * time.Minute)
	if _, err := f.c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 2 || f.got[0].Late != 8*time.Minute || f.got[1].Late != 8*time.Minute {
		t.Fatalf("late: %+v", f.got)
	}
}
