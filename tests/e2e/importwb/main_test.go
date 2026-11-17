//go:build e2e

package importwb

import (
	"os"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// TestMain 在测试进程结束后删除 baseline 构建的二进制目录。
func TestMain(m *testing.M) { os.Exit(baseline.Run(m)) }
