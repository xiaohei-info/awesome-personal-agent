//go:build e2e

// Package generic：账本与 CLI、判断型与生成型、判断型初筛、Source 流水线在未安装 harness 的主机上可用
package generic

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/release/internal/nohost"
)

// ACT-REL-04：账本与 CLI、判断型与生成型、判断型初筛、Source 流水线在未安装 harness 的主机上可用
func TestGenericCapabilitiesWithoutHarness(t *testing.T) {
	nohost.Run(t, []string{"./tests/e2e/ledger", "./tests/e2e/agents", "./tests/e2e/screening", "./tests/e2e/sourcepull"}, "")
}
