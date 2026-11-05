// dir-watch 是本地目录监视程序的可执行文件：stdin JSON → 退出码 + stdout 事件 JSONL。
package main

import (
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/sources/dirwatch"
)

func main() {
	os.Exit(dirwatch.Run(os.Stdin, os.Stdout, os.Stderr))
}
