// Package stubharness 是测试用的无头运行替身（STUB_HARNESS）。
// 只用于测试与验收环境，不得被 apa 二进制或内核代码导入。
//
// 行为开关：提示词（任一参数）中的 `STUB_MODE=<mode>` 优先，其次环境变量 STUB_HARNESS_MODE，默认 receipt。
//
//	receipt      执行 STUB_HARNESS_RECEIPT_CMD（调 CLI 回执），成功后在 stdout 末段写摘要并退出 0
//	no-receipt   不回执、不写摘要，退出 0
//	exit1        退出码 1
//	hang         挂起不退出（STUB_HARNESS_HANG_FOR 可设上限，如 2m）；STUB_HARNESS_SPAWN_CHILD=1 时派生子进程并把 pid 写入 stub-harness-child.pid
//	summary-only 不调 CLI，只在 stdout 末段写摘要，退出 0
//
// 摘要是 stdout 最后一行 JSON：{"session_id":"<STUB_HARNESS_SESSION_ID，默认 stub-session-1>","outcome":"done"}。
// 启动后立即把收到的环境变量写入运行目录（STUB_HARNESS_RUN_DIR，默认当前目录）的 stub-harness-env.json。
package stubharness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var promptMode = regexp.MustCompile(`STUB_MODE=([a-z0-9-]+)`)

// Run 以 args（提示词）与 env（KEY=VALUE）执行一次；dir 是运行目录的兜底值。返回退出码。
func Run(args, env []string, dir string, stdout io.Writer) int {
	e := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			e[k] = v
		}
	}
	if d := e["STUB_HARNESS_RUN_DIR"]; d != "" {
		dir = d
	}
	if raw, err := json.Marshal(e); err != nil || os.WriteFile(filepath.Join(dir, "stub-harness-env.json"), raw, 0o600) != nil {
		fmt.Fprintln(os.Stderr, "stub-harness: cannot record env")
		return 2
	}

	mode := e["STUB_HARNESS_MODE"]
	if m := promptMode.FindStringSubmatch(strings.Join(args, " ")); m != nil {
		mode = m[1]
	}
	if mode == "" {
		mode = "receipt"
	}
	summary := func() {
		sid := e["STUB_HARNESS_SESSION_ID"]
		if sid == "" {
			sid = "stub-session-1"
		}
		b, _ := json.Marshal(map[string]string{"session_id": sid, "outcome": "done"})
		fmt.Fprintln(stdout, string(b))
	}

	switch mode {
	case "receipt":
		argv := strings.Fields(e["STUB_HARNESS_RECEIPT_CMD"])
		if len(argv) == 0 {
			fmt.Fprintln(os.Stderr, "stub-harness: STUB_HARNESS_RECEIPT_CMD not set")
			return 2
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, io.Discard, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "stub-harness: receipt command failed:", err)
			return 3
		}
		summary()
		return 0
	case "no-receipt":
		return 0
	case "exit1":
		return 1
	case "summary-only":
		summary()
		return 0
	case "hang":
		if e["STUB_HARNESS_SPAWN_CHILD"] == "1" && e["STUB_HARNESS_CHILD"] != "1" {
			self, err := os.Executable()
			if err != nil {
				return 2
			}
			child := exec.Command(self)
			child.Dir, child.Env = dir, append(env[:len(env):len(env)], "STUB_HARNESS_CHILD=1", "STUB_HARNESS_MODE=hang")
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, "stub-harness: spawn child:", err)
				return 2
			}
			os.WriteFile(filepath.Join(dir, "stub-harness-child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
		if d, err := time.ParseDuration(e["STUB_HARNESS_HANG_FOR"]); err == nil && d > 0 {
			time.Sleep(d)
			return 0
		}
		time.Sleep(1<<63 - 1) // select{} 会被运行时判为死锁而退出
		return 0
	default:
		fmt.Fprintln(os.Stderr, "stub-harness: unknown mode", mode)
		return 2
	}
}
