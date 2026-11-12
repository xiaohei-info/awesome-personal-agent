//go:build apa_testclock

package clock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func setOffset(t *testing.T, home, v string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, OffsetFile), []byte(v), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 写偏移后，Run 不等下一个 tick 就触发到期（interval 设成 1 小时）。
func TestRunFiresDueImmediatelyAfterOffsetWrite(t *testing.T) {
	home := t.TempDir()
	SetHome(home)
	fired := make(chan Command, 1)
	st := newFixture(t).st
	c, err := New(st, func(tx *store.Tx, cmd Command) error { fired <- cmd; return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := Add(tx, domain.Timer{ID: "tm_far", Target: "item:1", Kind: domain.TimerDue, At: Now().Add(24 * time.Hour)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx, time.Hour)
	select {
	case <-fired:
		t.Fatal("fired before the offset was written")
	case <-time.After(200 * time.Millisecond):
	}
	setOffset(t, home, "25h")
	select {
	case cmd := <-fired:
		if cmd.TimerID != "tm_far" {
			t.Fatalf("unexpected timer %s", cmd.TimerID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("due timer not fired right after the offset write")
	}
}
