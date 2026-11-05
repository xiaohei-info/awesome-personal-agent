// redline 守住 AGENTS.md「红线检查」描述的架构边界：只扫描 import、标识符与名字，不校验业务规则。
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	os.Exit(Run(root, os.Stdout))
}

// Run 返回退出码：0 通过，1 发现违规，2 无法完成扫描。
func Run(root string, out io.Writer) int {
	vs, err := Check(root)
	if err != nil {
		fmt.Fprintln(out, "redline: error:", err)
		return 2
	}
	for _, v := range vs {
		fmt.Fprintln(out, v)
	}
	if len(vs) > 0 {
		fmt.Fprintf(out, "redline: %d violation(s)\n", len(vs))
		return 1
	}
	fmt.Fprintln(out, "redline: ok")
	return 0
}
