//go:build e2e

package materials

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// flow-purge、flow-retention 的全流程测试留在 tests/e2e/ops（与 flow-health 共用 helper），本节点是它们的唯一全流程 owner：
// 这里用 `go test -json` 真实重跑，并逐条断言验收文档里的每个 business-action 都有一个通过的子测试，
// 任何一条缺失、失败或被跳过都让本节点失败（不改断言迁就实现）。每个流程都在替身环境里用测试构建的真实 apa 二进制执行。
func TestOpsFlows(t *testing.T) {
	flows := []struct {
		name, run string
		prefix    string
		n         int
	}{
		{"flow-purge", "^TestFlowPurge$", "ACT-PRG", 5},
		{"flow-retention", "^TestFlowRetention$", "ACT-RET", 2},
	}
	for _, f := range flows {
		t.Run(f.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			cmd := exec.Command("go", "test", "-tags=e2e", "-count=1", "-json", "-timeout=15m", "-run", f.run, "../ops")
			cmd.Stdout, cmd.Stderr = &out, &errOut
			start := time.Now()
			if err := cmd.Run(); err != nil {
				s := out.String()
				if len(s) > 4000 {
					s = s[len(s)-4000:]
				}
				t.Fatalf("%s 的全流程测试失败: %v\n%s\n%s", f.name, err, s, errOut.String())
			}
			var passed []string
			sc := bufio.NewScanner(&out)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				if l := sc.Text(); strings.Contains(l, `"Action":"pass"`) {
					passed = append(passed, l)
				}
			}
			for i := 1; i <= f.n; i++ {
				id := fmt.Sprintf("%s-%02d", f.prefix, i)
				found := false
				for _, l := range passed {
					if strings.Contains(l, "/"+id) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: %s 没有通过的子测试", f.name, id)
				}
			}
			t.Logf("%s: 检查 %d 条 action，用时 %s", f.name, f.n, time.Since(start).Round(time.Second))
		})
	}
}
