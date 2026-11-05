//go:build e2e

package agents

import (
	"os"
	"testing"
)

// TestMain 在启动任何 apa serve 之前设置内核环境里的"机密"与 XDG 变量（ACT-OUT-05）：
// 子进程不继承内核环境，只拿白名单变量。
func TestMain(m *testing.M) {
	os.Setenv("ACC_SECRET", "leak")
	os.Setenv("XDG_CONFIG_HOME", "/tmp/acc-xdg-config")
	os.Exit(m.Run())
}
