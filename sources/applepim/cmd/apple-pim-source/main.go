// apple-pim-source 是 Apple 邮件、日历、提醒事项来源适配器的可执行文件：stdin JSON → 退出码 + stdout JSONL。
package main

import (
	"context"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/sources/applepim"
)

func main() {
	os.Exit(applepim.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr))
}
