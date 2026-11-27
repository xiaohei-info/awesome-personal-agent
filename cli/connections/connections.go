// Package connections 是 apa 的连接命令组（附录 B.1「对外」，设计 §7.6）：列出与管理内置渠道连接。
//
// 薄客户端：只调 /v1/connections；token 能读到哪些字段（不含配置值与凭据）由服务端一处裁剪，这里不重算、不过滤；
// 写命令属权限卡的"渠道"组，由服务端判定（§14.3）。
package connections

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("connection", usageHint, run) }

const usageHint = "usage: apa connection list [--direction channel] | add [<JSON 文件>|-] | configure <id> [<JSON 文件>|-] | rename <id> <名字> | enable <id> | disable <id> | remove <id>"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no connection subcommand given")
	}
	switch args[0] {
	case "list":
		return list(c, args[1:])
	case "add":
		return write(c, "POST", "/connections", args[1:], 0)
	case "configure":
		return write(c, "PUT", "/connections/{id}", args[1:], 1)
	case "rename":
		if len(args) != 3 {
			return usage("rename takes an id and a name")
		}
		return send(c, "POST", "/connections/"+url.PathEscape(args[1])+"/rename", map[string]string{"name": args[2]})
	case "enable", "disable":
		if len(args) != 2 {
			return usage(args[0] + " takes one id")
		}
		return send(c, "POST", "/connections/"+url.PathEscape(args[1])+"/"+args[0], map[string]any{})
	case "remove":
		if len(args) != 2 {
			return usage("remove takes one id")
		}
		return send(c, "DELETE", "/connections/"+url.PathEscape(args[1]), nil)
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

// write 发一个带 JSON 体的写请求：ids 个位置参数先替换路径里的 {id}，其余是文件或 -（缺省读标准输入）。
func write(c *client.Ctx, method, path string, args []string, ids int) error {
	if len(args) < ids || len(args) > ids+1 {
		return usage("invalid arguments")
	}
	if ids == 1 {
		path = strings.Replace(path, "{id}", url.PathEscape(args[0]), 1)
	}
	var (
		b   []byte
		err error
	)
	if rest := args[ids:]; len(rest) == 1 && rest[0] != "-" {
		b, err = os.ReadFile(rest[0])
	} else {
		b, err = io.ReadAll(c.Env.Stdin)
	}
	if err != nil || !json.Valid(b) {
		return usage("the request body must be a JSON file or JSON on stdin")
	}
	return send(c, method, path, json.RawMessage(b))
}

func send(c *client.Ctx, method, path string, body any) error {
	var out json.RawMessage
	if err := c.Do(context.Background(), method, path, body, &out); err != nil {
		return err
	}
	if len(out) == 0 {
		return nil
	}
	return c.Print(out)
}
