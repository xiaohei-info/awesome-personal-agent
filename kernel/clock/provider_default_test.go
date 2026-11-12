//go:build !apa_testclock

package clock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 发布构建（默认构建）里偏移文件不生效：Now 仍是系统时间，也没有唤醒通道。
func TestOffsetFileIgnoredByDefaultBuild(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, OffsetFile), []byte("48h"), 0o644); err != nil {
		t.Fatal(err)
	}
	SetHome(home)
	if TestClockBuild {
		t.Fatal("TestClockBuild must be false without the apa_testclock tag")
	}
	if d := time.Until(Now()); d > time.Minute || d < -time.Minute {
		t.Fatalf("Now() moved by %v without the tag", d)
	}
	if Changed() != nil {
		t.Fatal("Changed() must be nil without the tag")
	}
}
