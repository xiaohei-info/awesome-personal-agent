//go:build e2e

package automation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// TestMain 在测试进程结束后删除 baseline 构建的二进制目录（APA 测试残留清理）。
func TestMain(m *testing.M) { os.Exit(run(m)) }

// run 用 defer 删除本包自建的二进制目录（与 baseline 的共享目录一起），测试 panic 时也清。
func run(m *testing.M) int {
	defer func() {
		if harnessBin != "" {
			os.RemoveAll(filepath.Dir(harnessBin))
		}
	}()
	return baseline.Run(m)
}
