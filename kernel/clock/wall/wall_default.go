//go:build !apa_testclock

// Package wall 是进程唯一的时间源（叶子包，store、eventlog 等低层包也能用）；
// 其余代码经 kernel/clock 的同名函数取用。发布构建恒为系统时间，apa_testclock 构建叠加偏移文件。
package wall

import "time"

// TestClockBuild 报告当前二进制是否为测试时钟构建（test-clock-build-flag）；发布构建恒为 false。
const TestClockBuild = false

// OffsetFile 是测试构建在 APA_HOME 下读取的偏移文件名；发布构建不读它。
const OffsetFile = "test-clock-offset"

// Now 是进程唯一的时钟：kernel/clock 以外的生产代码一律经它取时间，不直接调 time.Now。
func Now() time.Time { return time.Now() }

// SetHome 登记 APA_HOME；发布构建里什么也不做。
func SetHome(string) {}

// Changed 在偏移前进时发信号；发布构建里恒为 nil（永不触发）。
func Changed() <-chan struct{} { return nil }
