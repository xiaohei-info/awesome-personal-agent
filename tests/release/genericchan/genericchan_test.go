//go:build e2e

// Package genericchan：渠道 ext 协议（Telegram）在未安装 harness 的主机上可用
package genericchan

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/release/internal/nohost"
)

// ACT-REL-04：渠道 ext 协议（Telegram）在未安装 harness 的主机上可用
func TestGenericChannelWithoutHarness(t *testing.T) {
	nohost.Run(t, []string{"./tests/e2e/channel"}, "^(TestFlowChannelTelegram|TestCHP)")
}
