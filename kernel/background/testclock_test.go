//go:build apa_testclock

package background

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 写偏移后，未注入 Now 的 Service 取到的 Now 前进：运行结束记录的时间戳立即落在偏移之后。
func TestTestClockOffsetMovesRunFinishedTimestamp(t *testing.T) {
	home := t.TempDir()
	clock.SetHome(home)
	useClockNow = true
	t.Cleanup(func() { useClockNow = false })
	if err := os.WriteFile(filepath.Join(home, clock.OffsetFile), []byte("100h"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := setup(t, 0)
	e.configure("silent")
	e.auto("main")
	e.open(e.newItem("a", []string{"agent-ok"}, nil))
	e.waitResult()
	fin := e.ofType(domain.EvRunFinished)
	if len(fin) != 1 {
		t.Fatalf("run.finished events = %d", len(fin))
	}
	if got := time.Until(fin[0].At); got < 99*time.Hour || got > 101*time.Hour {
		t.Fatalf("run.finished is %v ahead of real time, want ~100h", got)
	}
}
