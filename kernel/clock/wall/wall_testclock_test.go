//go:build apa_testclock

package wall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setOffset(t *testing.T, home, v string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, OffsetFile), []byte(v), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ahead() time.Duration { return time.Until(Now()) }

func TestOffsetOnlyMovesForward(t *testing.T) {
	home := t.TempDir()
	SetHome(home)
	if !TestClockBuild {
		t.Fatal("TestClockBuild must be true with the apa_testclock tag")
	}
	if d := ahead(); d > time.Minute {
		t.Fatalf("no offset file yet, got %v", d)
	}
	setOffset(t, home, "2h")
	if d := ahead(); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Fatalf("offset 2h not applied, got %v", d)
	}
	setOffset(t, home, "1h") // 往回调：忽略
	if d := ahead(); d < 2*time.Hour-time.Minute {
		t.Fatalf("offset moved backwards: %v", d)
	}
	setOffset(t, home, "garbage") // 非法：忽略
	if d := ahead(); d < 2*time.Hour-time.Minute {
		t.Fatalf("invalid offset changed the clock: %v", d)
	}
	setOffset(t, home, "10800") // 纯数字按秒
	if d := ahead(); d < 3*time.Hour-time.Minute {
		t.Fatalf("seconds offset not applied: %v", d)
	}
}

func TestChangedFiresWithoutRestart(t *testing.T) {
	home := t.TempDir()
	SetHome(home)
	setOffset(t, home, "1h")
	select {
	case <-Changed():
	case <-time.After(2 * time.Second):
		t.Fatal("Changed() did not fire after the offset file was written")
	}
}
