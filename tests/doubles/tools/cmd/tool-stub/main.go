// tool-stub 是 TOOL_STUB 的可执行入口。以工具名（himalaya、gog、gh 等）调用时回放录制输出；
// 以 tool-stub 调用时：`tool-stub install <TOOL_STUB_BIN> <仓库根>` 把自己以各工具名放进目录，`tool-stub case <TOOL_STUB_BIN> <名>` 切换 case。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/tools"
)

func main() {
	name := filepath.Base(os.Args[0])
	if name == "tool-stub" {
		os.Exit(admin(os.Args[1:]))
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tool-stub:", err)
		os.Exit(tools.ExitNoCase)
	}
	os.Exit(tools.Run(name, os.Args[1:], os.Environ(), filepath.Dir(self), os.Stdout, os.Stderr))
}

func admin(a []string) int {
	var err error
	switch {
	case len(a) == 3 && a[0] == "install":
		var self string
		if self, err = os.Executable(); err == nil {
			err = tools.Install(a[1], self, a[2])
		}
	case len(a) == 3 && a[0] == "case":
		err = tools.SetCase(a[1], a[2])
	default:
		fmt.Fprintln(os.Stderr, "usage: tool-stub install <bin-dir> <repo-root> | tool-stub case <bin-dir> <case>")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tool-stub:", err)
		return 1
	}
	return 0
}
