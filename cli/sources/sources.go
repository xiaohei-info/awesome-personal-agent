// Package sources 是 apa 的来源命令组（附录 B.1「来源」）。目前只有 `apa source auth <id>`（设计 §4.2）：
// 在本机启动附带拉取程序的认证模式，凭据写入来源自己的目录，内核不读。其余子命令属于后续的来源命令组。
package sources

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const usageText = "usage: apa source auth <id>"

func init() { client.Register("source", source) }

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

func source(c *client.Ctx, args []string) error {
	if len(args) == 0 || args[0] != "auth" {
		return usage("unknown source subcommand", usageText)
	}
	if len(args) != 2 || strings.HasPrefix(args[1], "-") {
		return usage("exactly one source id is required", usageText)
	}
	return auth(c, args[1])
}

// auth 经 API 读来源登记（CLI 不碰存储），在本机运行 `<命令> auth <id>`。程序失败时退出码是 1，详情在它自己的输出里。
func auth(c *client.Ctx, id string) error {
	ctx := context.Background()
	var s domain.Source
	if err := c.Do(ctx, "GET", "/sources/"+url.PathEscape(id), nil, &s); err != nil {
		return err
	}
	argv := strings.Fields(s.Command)
	if s.Type != domain.SourcePull || len(argv) == 0 {
		return usage("source "+id+" is not a bundled pull program", "tool-adapted sources use the tool's own auth command; webhook sources use `apa source webhook`")
	}
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	dir := config.LayoutOf(home).SourceDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "auth", id)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Env.Stdin, c.Env.Stdout, c.Env.Stderr
	return cmd.Run()
}
