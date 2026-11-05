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
