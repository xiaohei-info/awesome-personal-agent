//go:build e2e

// Package genericfwd：渠道 ext 的 webhook 转发白名单在未安装 harness 的主机上可用
package genericfwd

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/release/internal/nohost"
)

// ACT-REL-04：渠道 ext 的 webhook 转发白名单在未安装 harness 的主机上可用
func TestGenericForwardWithoutHarness(t *testing.T) {
	nohost.Run(t, []string{"./tests/e2e/channel"}, "^(TestFlowForwardAllowlist|TestForwardAllowlistDefaults)$")
}
