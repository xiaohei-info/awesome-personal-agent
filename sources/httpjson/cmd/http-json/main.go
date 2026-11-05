// http-json 是通用 HTTP JSON 轮询程序的可执行文件：stdin JSON → 退出码 + stdout JSONL。
package main

import (
	"context"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/sources/httpjson"
)

func main() {
	os.Exit(httpjson.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr))
}
