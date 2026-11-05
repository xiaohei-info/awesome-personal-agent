// rss-source 是 RSS/Atom 拉取程序的可执行文件：stdin JSON → 退出码 + stdout JSONL。
package main

import (
	"context"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/sources/rss"
)

func main() {
	os.Exit(rss.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr))
}
