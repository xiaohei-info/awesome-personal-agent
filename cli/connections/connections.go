// Package connections 是 apa 的连接命令组（附录 B.1「对外」，设计 §7.6）：connection list。
//
// 薄客户端：只调 GET /v1/connections；token 能读到哪些字段（不含配置值与凭据）由服务端一处裁剪，这里不重算、不过滤。
package connections

import (
	"context"
	"flag"
	"io"
	"net/url"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("connection", run) }

const usageHint = "usage: apa connection list [--direction channel]"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no connection subcommand given")
	}
	switch args[0] {
	case "list":
		return list(c, args[1:])
	}
	return usage("unknown connection subcommand " + args[0])
}

func list(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("connection list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("direction", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments")
	}
	if *dir != "" && *dir != "channel" {
		return usage("direction must be channel")
	}
	path := "/connections"
	if *dir != "" {
		path += "?" + url.Values{"direction": {*dir}}.Encode()
	}
	var raw map[string]any // 原样透传，新增字段不会被吞掉
	if err := c.Do(context.Background(), "GET", path, nil, &raw); err != nil {
		return err
	}
	return c.Print(raw)
}
