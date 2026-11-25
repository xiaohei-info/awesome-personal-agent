//go:build e2e

package baseline

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const leakChildEnv = "APA_E2E_LEAK_CHILD"

// leftovers 报告 pid 的进程组是否还有进程、paths 里哪些还在。
func leftovers(pid int, paths []string) []string {
	var left []string
	if pid > 1 && syscall.Kill(-pid, 0) == nil {
		left = append(left, fmt.Sprintf("进程组 %d", pid))
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			left = append(left, p)
		}
	}
	return left
}

// TestNoLeakAfterTest：用例正常结束后，它起的 apa serve 进程组与 APA_HOME、本地仓库目录都不在了。
func TestNoLeakAfterTest(t *testing.T) {
	var pid int
	var paths []string
	t.Run("env", func(t *testing.T) {
		e := New(t)
		e.StartKernel()
		pid, paths = e.ServePID(), []string{e.Root, strings.TrimPrefix(e.LocalRepo, "file://")}
	})
	if left := leftovers(pid, paths); len(left) > 0 {
		t.Fatalf("用例结束后有残留: %v", left)
	}
}

// TestLeakChild 只在 TestNoLeakOnAbort 拉起的子测试进程里运行：起一套环境，报出 pid 与目录后挂住，等父进程 kill -9。
func TestLeakChild(t *testing.T) {
	if os.Getenv(leakChildEnv) != "1" {
		t.Skip("只作 TestNoLeakOnAbort 的子进程运行")
	}
	e := New(t)
	e.StartKernel()
	fmt.Printf("LEAK %d %q %q %q\n", e.ServePID(), e.Root, strings.TrimPrefix(e.LocalRepo, "file://"), filepath.Dir(e.Bin["apa"]))
	time.Sleep(time.Hour)
}

// TestNoLeakOnAbort：测试进程被 kill -9（与 -timeout 超时、Ctrl-C 一样，t.Cleanup 和 TestMain 的 defer 都不运行）后，
// 它起的 apa serve 进程组、APA_HOME、本地仓库与二进制目录在数秒内被看管进程收掉。
func TestNoLeakOnAbort(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestLeakChild$", "-test.v")
	child.Env = append(os.Environ(), leakChildEnv+"=1")
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	child.Stderr = &errb
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	lines := make(chan string, 1)
	go func() {
		defer close(lines)
		for sc := bufio.NewScanner(out); sc.Scan(); {
			if strings.HasPrefix(sc.Text(), "LEAK ") {
				lines <- sc.Text()
				return
			}
		}
	}()
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Minute):
		t.Fatalf("子测试进程 5 分钟内没有起好环境: %s", errb.String())
	}
	var pid int
	var root, repo, bin string
	if _, err := fmt.Sscanf(line, "LEAK %d %q %q %q", &pid, &root, &repo, &bin); err != nil || pid <= 1 {
		t.Fatalf("子测试进程没有报出环境（%v）: %q %s", err, line, errb.String())
	}
	paths := []string{root, repo, bin}
	if left := leftovers(pid, paths); len(left) != len(paths)+1 {
		t.Fatalf("kill 之前环境就不全: 只有 %v", left)
	}
	t.Cleanup(func() { // 断言失败时也不把残留留给本机：按记录的进程组收掉
		if syscall.Kill(-pid, 0) == nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})

	_ = child.Process.Kill()
	_ = child.Wait()
	deadline := time.Now().Add(20 * time.Second)
	for len(leftovers(pid, paths)) > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if left := leftovers(pid, paths); len(left) > 0 {
		t.Fatalf("测试进程被 kill -9 后 20 秒仍有残留: %v", left)
	}
}
