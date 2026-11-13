//go:build e2e

package release

import (
	"strings"
	"testing"
)

// 放在 e2e 标签下：它在内部起 6 个 e2e 套件，约 5 分钟；默认 `go test ./...` 带它会逼近 10 分钟的包超时（CI 上曾因此失败）。
// 契约命令 `go test -tags=e2e ./tests/release/...` 照常运行它。
// ACT-REL-04：未安装任何 harness 时，账本、Web、CLI、判断型、生成型、Source 流水线、Telegram 渠道协议可用；
// 只有任务型和后台执行不可用，级联终止于 needs_input。用真实 apa 二进制的 e2e 套件在剔除 harness 的 PATH 下跑。
func TestGenericCapabilitiesWithoutHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过：需要先构建 Web（npm --prefix web run build）")
	}
	root := repoRoot(t)
	env := noHarnessEnv(t)
	suites := []string{
		"./tests/e2e/ledger",     // 账本与 CLI
		"./tests/e2e/agents",     // 判断型、生成型，级联终止于 needs_input
		"./tests/e2e/screening",  // 判断型初筛
		"./tests/e2e/sourcepull", // Source 流水线
		"./tests/e2e/channel",    // 渠道 ext 协议
	}
	args := append([]string{"test", "-tags", "e2e", "-count=1", "-p", "2"}, suites...)
	out, code := run(t, root, env, "go", args...)
	if code != 0 {
		t.Fatalf("未装 harness 时通用能力失败（退出码 %d）:\n%s", code, tail(out, 4000))
	}
	for _, s := range suites {
		if !strings.Contains(out, "ok  \tgithub.com/xiaohei-info/awesome-personal-agent/"+strings.TrimPrefix(s, "./")) {
			t.Errorf("套件 %s 没有通过的记录:\n%s", s, tail(out, 2000))
		}
	}
}
