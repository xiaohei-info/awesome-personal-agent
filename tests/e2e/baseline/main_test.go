//go:build e2e

package baseline

import (
	"os"
	"testing"
)

// TestMain 在测试进程结束后删除构建的二进制目录（APA 测试残留清理）。
func TestMain(m *testing.M) { os.Exit(Run(m)) }
