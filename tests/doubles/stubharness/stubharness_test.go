package stubharness_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/stubharness"
)

var bin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "stubharness")
	bin = filepath.Join(dir, "stub-harness")
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/stub-harness").CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout string
	code   int
	dir    string
}

func run(t *testing.T, env []string, args ...string) result {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	code := stubharness.Run(args, append([]string{"PATH=" + os.Getenv("PATH")}, env...), dir, &out)
	return result{out.String(), code, dir}
}

func lastLine(s string) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return l[len(l)-1]
}

func readEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "stub-harness-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// flow-env-baseline: 环境变量写入运行目录。
func TestRecordsEnvironmentInRunDir(t *testing.T) {
	r := run(t, []string{"ACC_SECRET=leak", "APA_RUN=r1", "STUB_HARNESS_MODE=exit1"}, "prompt")
	env := readEnv(t, r.dir)
	if env["ACC_SECRET"] != "leak" || env["APA_RUN"] != "r1" {
		t.Fatalf("env not recorded: %v", env)
	}
	if _, ok := env["APA_TOKEN"]; ok {
		t.Fatal("recorded a variable that was not passed")
	}
}

// flow-agent-cascade: 模式 1，调 CLI 回执后退出；回执命令真被执行且继承运行环境。
func TestReceiptModeInvokesCLIAndExitsZero(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "receipt.txt")
	cli := filepath.Join(dir, "fakecli.sh")
	os.WriteFile(cli, []byte("#!/bin/sh\necho \"$APA_RUN $*\" > "+marker+"\n"), 0o755)
	r := run(t, []string{"APA_RUN=r7", "STUB_HARNESS_RECEIPT_CMD=" + cli + " report item-1 --outcome done"}, "do it")
	if r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	got, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(got)) != "r7 report item-1 --outcome done" {
		t.Fatalf("receipt cmd result %q", got)
	}
	var sum map[string]string
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &sum); err != nil || sum["session_id"] == "" {
		t.Fatalf("stdout tail must carry session id: %q", r.stdout)
	}
}

func TestReceiptModeFailsLoudlyWhenCLIFails(t *testing.T) {
	r := run(t, []string{"STUB_HARNESS_RECEIPT_CMD=false"}, "x")
	if r.code == 0 {
		t.Fatal("must not pretend receipt succeeded")
	}
	r = run(t, nil, "x") // 未配置回执命令
	if r.code == 0 {
		t.Fatal("receipt mode without command must fail")
	}
}

// 模式 2：不回执退出，也不写摘要。
func TestNoReceiptExitsZeroWithoutSummary(t *testing.T) {
	r := run(t, []string{"STUB_HARNESS_MODE=no-receipt"}, "x")
	if r.code != 0 || strings.Contains(r.stdout, "session_id") {
		t.Fatalf("code=%d stdout=%q", r.code, r.stdout)
	}
}

// 模式 3：退出码 1。
func TestExitOne(t *testing.T) {
	if r := run(t, []string{"STUB_HARNESS_MODE=exit1"}, "x"); r.code != 1 {
		t.Fatalf("exit %d", r.code)
	}
}

// 模式 5：只在 stdout 末段写结构化摘要，不调 CLI。
func TestSummaryOnlyWritesStructuredTail(t *testing.T) {
	r := run(t, []string{"STUB_HARNESS_MODE=summary-only", "STUB_HARNESS_SESSION_ID=s-42", "STUB_HARNESS_RECEIPT_CMD=false"}, "x")
	if r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	var sum map[string]string
	if err := json.Unmarshal([]byte(lastLine(r.stdout)), &sum); err != nil {
		t.Fatalf("tail not json: %q", r.stdout)
	}
	if sum["session_id"] != "s-42" || sum["outcome"] != "done" {
		t.Fatalf("summary %v", sum)
	}
}

// 提示词中的开关优先于环境开关（按提示词切换行为）。
func TestPromptSwitchOverridesEnv(t *testing.T) {
	r := run(t, []string{"STUB_HARNESS_MODE=exit1"}, "template ... STUB_MODE=no-receipt ... item-1")
	if r.code != 0 {
		t.Fatalf("prompt switch ignored, exit %d", r.code)
	}
}

func TestUnknownModeRejected(t *testing.T) {
	if r := run(t, []string{"STUB_HARNESS_MODE=bogus"}, "x"); r.code != 2 {
		t.Fatalf("exit %d", r.code)
	}
}

// 模式 4：挂起不退出，直到被杀；可派生子进程并记录其 pid。
func TestHangUntilKilledAndSpawnsChild(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(bin, "x")
	cmd.Dir = dir
	cmd.Env = []string{"STUB_HARNESS_MODE=hang", "STUB_HARNESS_SPAWN_CHILD=1"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	var child int
	for i := 0; i < 100 && child == 0; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "stub-harness-child.pid")); err == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if child == 0 || syscall.Kill(child, 0) != nil {
		t.Fatalf("child not running: %d", child)
	}
	select {
	case err := <-done:
		t.Fatalf("hang mode exited: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("not killable")
	}
}

func TestHangForBoundedThenExits(t *testing.T) {
	start := time.Now()
	r := run(t, []string{"STUB_HARNESS_MODE=hang", "STUB_HARNESS_HANG_FOR=200ms"}, "x")
	if r.code != 0 || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("code=%d elapsed=%v", r.code, time.Since(start))
	}
}
