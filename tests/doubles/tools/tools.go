// Package tools 是来源工具的录制样本替身（TOOL_STUB）：与 himalaya、gog、apple-pim 三个 CLI、gh 同名的可执行，
// 按 $TOOL_STUB_BIN/case 回放 sources/*/testdata 与 testdata/gh 里录制的输出，并把 argv 与环境变量名记入 $TOOL_LOG。
// 只用于测试与验收环境，不得被 apa 二进制或内核代码导入；不连任何真实账号。
package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Names 是替身冒充的工具名。
var Names = []string{"himalaya", "gog", "mail-cli", "calendar-cli", "reminder-cli", "gh"}

// 替身自己的退出码，避开真实工具的 0/1/2。
const (
	ExitNoCase   = 99 // 没有设置 case 或 case 不存在
	ExitNoRecord = 98 // 该 case 没有录制这个工具的输出
	ExitRefused  = 64 // 非只读命令或未录制的子命令
)

// LogEntry 是 $TOOL_LOG 的一行。环境变量只记名字，不记值（凭据不落日志）。
type LogEntry struct {
	Time string   `json:"time"`
	Tool string   `json:"tool"`
	Case string   `json:"case"`
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
}

type out struct {
	stdout, stderr string
	exit           int
}

func ok(s string) out { return out{stdout: s} }
func failf(code int, f string, a ...any) out {
	return out{stderr: fmt.Sprintf(f, a...) + "\n", exit: code}
}

// SetCase 写 binDir/case。
func SetCase(binDir, name string) error {
	return os.WriteFile(filepath.Join(binDir, "case"), []byte(name+"\n"), 0o644)
}

// Install 把 self（替身可执行）以各工具名复制到 binDir，并记下仓库根（回放样本的位置）。
// 复制而非符号链接：替身靠自身路径找到 binDir。
func Install(binDir, self, root string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	for _, n := range Names {
		if err := os.WriteFile(filepath.Join(binDir, n), b, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(binDir, "root"), []byte(root), 0o644)
}

// Run 以工具名 name 执行一次调用。binDir 是替身所在目录（含 case、root）。返回退出码。
func Run(name string, args, env []string, binDir string, stdout, stderr io.Writer) int {
	caseName := readTrim(filepath.Join(binDir, "case"))
	if caseName == "" { // 验收约定用 case 文件；单测里直接设环境变量也可
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "TOOL_STUB_CASE="); ok {
				caseName = v
			}
		}
	}
	logPath := filepath.Join(binDir, "tool.log")
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "TOOL_LOG="); ok && v != "" {
			logPath = v
		}
	}
	if err := appendLog(logPath, name, caseName, args, env); err != nil {
		fmt.Fprintln(stderr, "tool-stub: 写 TOOL_LOG 失败:", err)
		return ExitNoCase
	}
	root := readTrim(filepath.Join(binDir, "root"))
	if root == "" {
		root = findRoot()
	}
	if caseName == "" {
		fmt.Fprintf(stderr, "tool-stub: %s/case 为空，且未设置 TOOL_STUB_CASE\n", binDir)
		return ExitNoCase
	}
	r := dispatch(name, caseName, args, root)
	io.WriteString(stdout, r.stdout)
	io.WriteString(stderr, r.stderr)
	return r.exit
}

func dispatch(tool, c string, args []string, root string) out {
	fam := map[string]string{"himalaya": "mail-", "gog": "cal-", "mail-cli": "pim-mail", "calendar-cli": "pim-calendar", "reminder-cli": "pim-reminders", "gh": "gh-"}
	prefix, known := fam[tool]
	if !known {
		return failf(ExitNoRecord, "tool-stub: 未知工具 %q", tool)
	}
	if !strings.HasPrefix(c, prefix) {
		if c2 := strings.SplitN(c, "-", 2)[0]; c2 != "mail" && c2 != "cal" && c2 != "gh" && c2 != "pim" {
			return failf(ExitNoCase, "tool-stub: 未知 case %q", c)
		}
		return failf(ExitNoRecord, "tool-stub: case %q 没有录制 %s 的输出", c, tool)
	}
	switch tool {
	case "himalaya":
		return mailCall(c, args, root)
	case "gog":
		return gogCall(c, args, root)
	case "gh":
		return ghCall(c, args, root)
	}
	return pimCall(tool, c, args, root)
}

func appendLog(path, tool, c string, args, env []string) error {
	names := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		names = append(names, k)
	}
	sort.Strings(names)
	line, _ := json.Marshal(LogEntry{Time: time.Now().UTC().Format(time.RFC3339Nano), Tool: tool, Case: c, Argv: append([]string{}, args...), Env: names})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

func readTrim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// findRoot 自当前目录向上找 go.mod。
func findRoot() string {
	d, _ := os.Getwd()
	for ; d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	return "."
}

func readFixture(root string, rel ...string) ([]byte, error) {
	return os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
}

// lastNonJSONArg 取去掉尾部 JSON 标志后的最后一个参数（himalaya 的邮件序号）。
func trimJSONFlag(args []string) []string {
	n := len(args)
	switch {
	case n >= 2 && args[n-2] == "--output" && args[n-1] == "json":
		return args[:n-2]
	case n >= 1 && args[n-1] == "--json":
		return args[:n-1]
	}
	return args
}

// marshal 不转义 < > &（himalaya 等工具的真实输出里邮件地址是原样的）。
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSpace(b.Bytes())
}
