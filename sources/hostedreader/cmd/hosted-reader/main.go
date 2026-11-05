// hosted-reader 是托管文档只读读取器的可执行文件：stdin JSON → 退出码 + stdout。
package main

import (
	"context"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/sources/hostedreader"
)

func main() {
	os.Exit(hostedreader.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr))
}
