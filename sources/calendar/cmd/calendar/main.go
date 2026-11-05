// calendar 是日历来源适配器的可执行文件：stdin JSON → 退出码 + stdout JSONL。
package main

import (
	"context"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/sources/calendar"
)

func main() {
	os.Exit(calendar.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr))
}
