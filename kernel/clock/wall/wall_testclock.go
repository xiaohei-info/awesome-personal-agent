//go:build apa_testclock

package wall

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TestClockBuild 报告当前二进制是否为测试时钟构建（test-clock-build-flag）。
const TestClockBuild = true

// OffsetFile 是 APA_HOME 下的偏移文件名：内容为 Go 时长（"36h"）或整数秒，只能往大改。
const OffsetFile = "test-clock-offset"

const pollEvery = 50 * time.Millisecond

var tc struct {
	mu   sync.Mutex
	path string
	off  time.Duration
	raw  string
	once sync.Once
}

var changed = make(chan struct{}, 1)

// SetHome 登记 APA_HOME、清零偏移并启动轮询（每进程一个 goroutine，进程退出即止）。
func SetHome(home string) {
	tc.mu.Lock()
	tc.path, tc.off, tc.raw = filepath.Join(home, OffsetFile), 0, ""
	tc.mu.Unlock()
	tc.once.Do(func() {
		go func() {
			for range time.Tick(pollEvery) {
				refresh()
			}
		}()
	})
}

// Now 返回系统时间加上当前偏移；每次调用都先重读偏移文件，写入后无需重启即可生效。
func Now() time.Time {
	refresh()
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return time.Now().Add(tc.off)
}

// Changed 在偏移前进后发一次信号，供内核时钟循环立即重算到期。
func Changed() <-chan struct{} { return changed }

func refresh() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.path == "" {
		return
	}
	b, err := os.ReadFile(tc.path)
	if err != nil {
		if !os.IsNotExist(err) && tc.raw != "!"+err.Error() {
			tc.raw = "!" + err.Error()
			slog.Warn("test clock: cannot read offset file", "path", tc.path, "err", err)
		}
		return
	}
	raw := strings.TrimSpace(string(b))
	if raw == tc.raw {
		return
	}
	tc.raw = raw
	d, err := parseOffset(raw)
	switch {
	case err != nil:
		slog.Warn("test clock: offset ignored", "value", raw, "err", err)
	case d < tc.off:
		slog.Warn("test clock: offset ignored, it only moves forward", "value", raw, "current", tc.off.String())
	case d > tc.off:
		slog.Info("test clock: offset advanced", "from", tc.off.String(), "to", d.String())
		tc.off = d
		select {
		case changed <- struct{}{}:
		default:
		}
	}
}

func parseOffset(s string) (time.Duration, error) {
	if s != "" && strings.Trim(s, "0123456789") == "" {
		s += "s"
	}
	return time.ParseDuration(s)
}
