// github-source 是 GitHub 拉取程序的可执行文件。
// 无参数：Source 契约（stdin JSON → stdout 事件 JSONL，退出码见 §9.2），凭据目录是 stdin 里的 credentials_path；
// `auth <credentials_path>`：认证模式，凭据写入该目录（`apa source auth <id>` 传入登记的路径）。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/sources/github"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		return github.RunStdin(ctx, stdin, stdout, stderr, func(dir string) github.API { return github.GH{ConfigDir: dir} }, time.Now)
	case len(args) == 2 && args[0] == "auth" && filepath.IsAbs(args[1]):
		return github.Auth(ctx, github.GH{ConfigDir: args[1]}, stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, "usage: github-source                            (Source contract on stdin)\n       github-source auth <absolute credentials_path>")
	return github.ExitAuth
}
