package clock

import (
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock/wall"
)

// TestClockBuild 报告当前二进制是否为测试时钟构建（test-clock-build-flag）；发布构建恒为 false。
const TestClockBuild = wall.TestClockBuild

// OffsetFile 是测试构建在 APA_HOME 下读取的偏移文件名。
const OffsetFile = wall.OffsetFile

// Now 是进程唯一的时钟：kernel/clock 以外的生产代码一律经它取时间，tools/redline 禁止直接调 time.Now。
func Now() time.Time { return wall.Now() }

// SetHome 登记 APA_HOME，测试构建据此读偏移文件；发布构建里什么也不做。
func SetHome(home string) { wall.SetHome(home) }

// Changed 在偏移前进时发信号，供时钟循环立即重算到期；发布构建里恒为 nil。
func Changed() <-chan struct{} { return wall.Changed() }
