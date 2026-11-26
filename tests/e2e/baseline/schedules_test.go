//go:build e2e

package baseline

import (
	"strings"
	"testing"
	"time"
)

// TestDefaultSchedulesPaused 验证验收环境与墙钟时刻无关：拨过一整周（含工作日 08:30 与周五 17:30）
// 也不出现默认安排建的简报、复盘事项与运行。
func TestDefaultSchedulesPaused(t *testing.T) {
	e := Setup(t)
	e.TCAdvance(8 * 24 * 60 * 60)
	time.Sleep(5 * time.Second)
	for _, kind := range []string{"brief", "review"} {
		r := e.Apa(e.OBS, "", "item", "list", "--kind", kind)
		if r.Code != 0 || strings.Contains(r.Stdout, "itm_") {
			t.Errorf("默认安排不应在验收环境触发 %s: %d %s%s", kind, r.Code, r.Stdout, r.Stderr)
		}
	}
}
