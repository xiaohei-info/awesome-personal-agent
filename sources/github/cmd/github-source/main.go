// github-source 是 GitHub 拉取程序的可执行文件。
// 无参数：Source 契约（stdin JSON → stdout 事件 JSONL，退出码见 §9.2）；
// `auth <source_id>`：认证模式，凭据写入该来源自己的目录。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/sources/github"
)

func main() {
	ctx := context.Background()
	if len(os.Args) == 3 && os.Args[1] == "auth" {
		dir, err := github.ConfigDir(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "github source:", err)
			os.Exit(github.ExitAuth)
		}
		os.Exit(github.Auth(ctx, github.GH{ConfigDir: dir}, os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: github-source            (Source contract on stdin)\n       github-source auth <source_id>")
		os.Exit(github.ExitAuth)
	}
	// 来源 id 在 stdin 里，所以先解析出来再决定凭据目录。
	os.Exit(github.RunStdin(ctx, os.Stdin, os.Stdout, os.Stderr, func(id string) (github.API, error) {
		dir, err := github.ConfigDir(id)
		return github.GH{ConfigDir: dir}, err
	}, time.Now))
}
