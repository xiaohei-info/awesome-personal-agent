//go:build !apa_testclock

package wall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 发布构建里偏移文件不生效：既不改变 Now，也没有唤醒通道。
func TestOffsetFileIgnoredWithoutTestClockTag(t *testing.T) {
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
		t.Fatal("Changed() must be nil (never fires) without the tag")
	}
}
