//go:build apa_testclock

package attention

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 写偏移后，未注入 Now 的触达取到的 Now 前进：决策截止已过，下一次时钟扫描立即产出“截止已过”提醒。
func TestTestClockOffsetPastDeadlineRemindsImmediately(t *testing.T) {
	home := t.TempDir()
	clock.SetHome(home)
	useClockNow = true
	t.Cleanup(func() { useClockNow = false })
	e := setup(t)
	e.toChannel()
	e.policy(func(p *policy.Policy) { p.Disturb.MergeWindowSec = 0 })
	id := e.asked(map[string]any{"deadline": clock.Now().Add(time.Hour)})
	if first := e.claim("channel:chan"); first == nil || first.Class != domain.DeliveryDecision {
		t.Fatalf("decision not delivered: %+v", first)
	}
	if _, err := e.clk.Tick(bg); err != nil {
		t.Fatal(err)
	}
	if d := e.claim("channel:chan"); d != nil {
		t.Fatalf("reminder before the deadline: %+v", d)
	}
	if err := os.WriteFile(filepath.Join(home, clock.OffsetFile), []byte("2h"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.clk.Tick(bg); err != nil { // 偏移写入后的第一次扫描
		t.Fatal(err)
	}
	if re := e.claim("channel:chan"); re == nil || re.Class != domain.DeliveryReminder || re.Item != id {
		t.Fatalf("no deadline reminder right after the offset write: %+v", re)
	}
}
