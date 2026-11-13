//go:build e2e

package mapping

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 这三条 v0.1 继承流程的全流程测试各自留在所属包里（共用 helper 与同包其他流程在一起），
// 本节点是它们的唯一全流程 owner：此处用 `go test -json` 真实重跑，并逐条断言验收文档里的
// 每个 business-action 都有一个通过的子测试，任何一条缺失、失败或被跳过都让本节点失败（不改断言迁就实现）。
// 每个流程都在替身环境里用测试构建的真实 apa 二进制执行，凭原话的命令带 APA_SESSION_KIND=direct。
type inheritedFlow struct {
	name    string
	pkg     string
	run     string // go test -run 正则
	actions []string
	// whole 里的 action 没有独立子测试，由同名的顶层测试整体承载（通过即该 action 通过）。
	whole map[string]string
}

func seq(prefix string, n int) []string {
	var ids []string
	for i := 1; i <= n; i++ {
		ids = append(ids, fmt.Sprintf("%s-%02d", prefix, i))
	}
	return ids
}

var inheritedFlows = []inheritedFlow{
	{"flow-allowed-executors", "../authz", "^TestFlowAllowedExecutors$", seq("ACT-ALW", 10), nil},
	{"flow-command-registration", "../authz", "^TestFlowCommandRegistration$", seq("ACT-CMD", 5), nil},
	{"flow-outcall-limits", "../agents", "^TestFlowOutcall(Queue|Short|SourceDedupe|Proc|Restart)$", seq("ACT-OUT", 8), map[string]string{"ACT-OUT-07": "TestFlowOutcallRestart"}},
}

func TestInheritedFlows(t *testing.T) {
	for _, f := range inheritedFlows {
		t.Run(f.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := exec.Command("go", "test", "-tags=e2e", "-count=1", "-json", "-timeout=10m", "-run", f.run, f.pkg)
			cmd.Stdout = &out
			var errOut bytes.Buffer
			cmd.Stderr = &errOut
			start := time.Now()
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s 的全流程测试失败: %v\n%s\n%s", f.name, err, tail(out.String()), errOut.String())
			}
			passed := map[string]bool{}
			sc := bufio.NewScanner(&out)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				line := sc.Text()
				if strings.Contains(line, `"Action":"pass"`) {
					passed[line] = true
				}
			}
			for _, id := range f.actions {
				found := false
				for line := range passed {
					if w, ok := f.whole[id]; ok {
						found = strings.Contains(line, `"Test":"`+w+`"`)
					} else {
						found = strings.Contains(line, `"Test":"`) && strings.Contains(line, "/"+id)
					}
					if found {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: %s 没有通过的子测试", f.name, id)
				}
			}
			t.Logf("%s: 检查 %d 条 action，用时 %s", f.name, len(f.actions), time.Since(start).Round(time.Second))
		})
	}
}

func tail(s string) string {
	if len(s) > 4000 {
		return s[len(s)-4000:]
	}
	return s
}
