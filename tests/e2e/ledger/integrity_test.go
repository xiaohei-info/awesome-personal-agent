//go:build e2e

package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for ; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if dir == filepath.Dir(dir) {
			t.Fatal("找不到仓库根目录")
		}
	}
}

// goTest 在仓库根运行 go test，返回合并输出；失败即用例失败。
func goTest(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"test", "-count=1"}, args...)...)
	cmd.Dir = repoRootDir(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test %v 失败: %v\n%s", args, err, out)
	}
	return string(out)
}

// passed 断言 `go test -v` 输出里这些用例都以 PASS 结束（而不是不存在或被跳过）。
func passed(t *testing.T, out string, names ...string) {
	t.Helper()
	for _, n := range names {
		if !regexp.MustCompile(`(?m)^--- PASS: ` + regexp.QuoteMeta(n) + ` `).MatchString(out) {
			t.Errorf("用例 %s 不存在或未通过", n)
		}
	}
}

// TestFlowLedgerIntegrity 按验收文档 flow-ledger-integrity 执行 ACT-LED-01..04。
func TestFlowLedgerIntegrity(t *testing.T) {
	t.Run("ACT-LED-01 由 Event 重建投影结果一致", func(t *testing.T) {
		// 项目、事项（含决策请求）、关系按 Event 折叠后与现有投影逐字段比较，事项投影重建后与原投影相同；
		// 强杀实验另验 SQLite 完整与折叠一致。
		out := goTest(t, "-v", "-run", "TestProjectionRebuildFromEventsMatchesAllFields", "./kernel/invariants/")
		passed(t, out, "TestProjectionRebuildFromEventsMatchesAllFields")
		out = goTest(t, "-v", "-run", "TestProjectionFoldsFromEvents|TestTimestampsFoldFromEvents|TestCorrelationKeysFoldAndRebuild", "./kernel/items/")
		passed(t, out, "TestProjectionFoldsFromEvents", "TestTimestampsFoldFromEvents", "TestCorrelationKeysFoldAndRebuild")
		out = goTest(t, "-v", "-run", "TestKillDuringWriteLoopKeepsVersionAndEventsAligned", "./tests/crash/")
		passed(t, out, "TestKillDuringWriteLoopKeepsVersionAndEventsAligned")
	})

	t.Run("ACT-LED-02 变更和 Event 同事务提交", func(t *testing.T) {
		out := goTest(t, "-v", "-run", "TestInv1LedgerChangeAndEventCommitTogether", "./kernel/invariants/")
		passed(t, out, "TestInv1LedgerChangeAndEventCommitTogether")

		// 真实二进制强杀：循环写事项时 kill -9 apa serve，重启后每个事项 version == Event 数。
		e := setup(t)
		var mu sync.Mutex
		acked := 0
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				r := e.Apa(e.DEL, "", "item", "add", "--kind", "task", "--phase", "open", "--title", fmt.Sprintf("kill-%d", i), "--evidence", "owner: add")
				if r.Code != 0 {
					return // 服务被杀后写入失败，循环结束
				}
				mu.Lock()
				acked++
				mu.Unlock()
			}
		}()
		waitFor(t, 60*time.Second, "杀前至少写入 15 个事项", func() bool { mu.Lock(); defer mu.Unlock(); return acked >= 15 })
		pid := e.ServePID()
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("kill -9 %d: %v", pid, err)
		}
		<-done
		close(stop)
		mu.Lock()
		before := acked
		mu.Unlock()
		e.StopKernel()
		e.StartKernelAgain()

		var list struct {
			Items []struct {
				ID      string  `json:"id"`
				Title   string  `json:"title"`
				Version float64 `json:"version"`
			} `json:"items"`
		}
		r := e.Web("GET", "/v1/items?limit=1000", "")
		if r.Code != 200 {
			t.Fatalf("列事项: %d %s", r.Code, r.Body)
		}
		if err := json.Unmarshal([]byte(r.Body), &list); err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, it := range list.Items {
			if !strings.HasPrefix(it.Title, "kill-") {
				continue
			}
			checked++
			// version 只随事项自身的变更 Event（item.*）递增；重启后的运行、告警等 Event 不改版本。
			n := 0
			for _, ev := range audit(t, e, "object="+it.ID) {
				if strings.HasPrefix(ev.Type, "item.") {
					n++
				}
			}
			if n != int(it.Version) {
				t.Errorf("事项 %s version=%v 但 item.* Event 有 %d 条", it.ID, it.Version, n)
			}
		}
		if checked < before {
			t.Errorf("杀前已确认写入 %d 个事项，重启后只剩 %d 个", before, checked)
		}
	})

	t.Run("ACT-LED-03 §6.14 不变量都有测试", func(t *testing.T) {
		out := goTest(t, "-v", "./kernel/invariants/")
		passed(t, out,
			"TestInv1LedgerChangeAndEventCommitTogether", "TestInv2ProposedItemsProduceNoDelivery",
			"TestInv3PhaseEntryConstraints", "TestInv3ReportDoneNeedsResultEvidence",
			"TestInv4WebOnlyOnlyEscalates", "TestInv5AllowedExecutorsBindEveryEntry",
			"TestInv6RuleActionEventsDoNotRetriggerRules", "TestInv7OneRunPerItem",
			"TestInv8RunIdentityCannotAnswer", "TestInv9RevisionsAreAppendOnly",
			"TestInv10OnlyOutcallReachesNetworkOrSubprocess", "TestInv11ProxyDecisionsNeedWordsAndSession")
		goTest(t, "./kernel/...")
	})

	t.Run("ACT-LED-04 红线检查进 CI 且能拦下违规", func(t *testing.T) {
		root := repoRootDir(t)
		ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
		if err != nil || !strings.Contains(string(ci), "go run ./tools/redline") {
			t.Fatalf("CI 配置不含红线检查步骤: %v", err)
		}
		bin := filepath.Join(t.TempDir(), "redline")
		build := exec.Command("go", "build", "-o", bin, "./tools/redline")
		build.Dir = root
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("构建 redline: %v\n%s", err, out)
		}
		runCheck := func(dir string) (int, string) {
			var buf bytes.Buffer
			c := exec.Command(bin, dir)
			c.Stdout, c.Stderr = &buf, &buf
			err := c.Run()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			return code, buf.String()
		}

		// 干净代码：在真实仓库副本上通过。
		work := filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{"kernel", "sources", "ext", "packs", "plugins"} {
			if _, err := os.Stat(filepath.Join(root, d)); err != nil {
				continue
			}
			if out, err := exec.Command("cp", "-r", filepath.Join(root, d), work).CombinedOutput(); err != nil {
				t.Fatalf("复制 %s: %v\n%s", d, err, out)
			}
		}
		if code, out := runCheck(work); code != 0 {
			t.Fatalf("干净代码应通过，退出码 %d:\n%s", code, out)
		}

		// 三处违规，各自单独使检查失败并指出位置；每次检查后撤销。
		for _, c := range []struct{ name, file, body string }{
			{"内核 import 网络库", "kernel/items/zz_violation.go", "package items\n\nimport _ \"net/http\"\n"},
			{"内核出现 harness 名", "kernel/items/zz_violation.go", "package items\n\n// uses hermes directly\nvar zzHermes = \"hermes\"\n"},
			{"packs 出现 harness 名", "packs/zz_violation.md", "this pack mentions hermes\n"},
		} {
			p := filepath.Join(work, c.file)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			code, out := runCheck(work)
			if code != 1 || !strings.Contains(out, filepath.Base(c.file)) {
				t.Errorf("%s: 应退出码 1 且指出 %s，实际 %d:\n%s", c.name, c.file, code, out)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if code, out := runCheck(work); code != 0 {
				t.Errorf("%s 撤销后应恢复通过，实际 %d:\n%s", c.name, code, out)
			}
		}
	})
}
