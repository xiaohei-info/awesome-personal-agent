//go:build e2e

package briefwb

import (
	"os"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// TestMain 在测试进程结束后删除 baseline 构建的二进制目录与本包构建的 SRC_STUB（APA 测试残留清理）。
func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	defer func() {
		if stubDir != "" {
			os.RemoveAll(stubDir)
		}
	}()
	return baseline.Run(m)
}
