// Package connections 是 apa 的连接命令组（附录 B.1「对外」，设计 §7.6）：connection list，
// 以及 Agent 的渠道草稿 propose、drafts、test-draft、discard。
//
// 薄客户端：只调 /v1/connections 与 /v1/connections/drafts；token 能读到哪些字段、能不能提交与测试，都由服务端一处判定。
// 凭据只从标准输入读（--secret-stdin，一个 JSON 对象），不进命令行、环境变量与输出；本人确认只在 Web。
package connections

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("connection", run) }

const usageHint = "usage: apa connection list [--direction channel|output] | propose --type <type> [--name <name>] [--connection <id>] [--set <key>=<value>]... [--route <class>]... [--secret-stdin] | drafts | test-draft <draft-id> | discard <draft-id>"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no connection subcommand given")
	}
	switch args[0] {
	case "list":
		return list(c, args[1:])
	case "propose":
		return propose(c, args[1:])
	case "drafts":
		if len(args) != 1 {
			return usage("drafts takes no arguments")
		}
		return pass(c, "GET", "/connections/drafts", nil)
	case "test-draft", "discard":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return usage("a draft id is required")
		}
		if args[0] == "discard" {
			return pass(c, "DELETE", "/connections/drafts/"+url.PathEscape(args[1]), nil)
		}
		return pass(c, "POST", "/connections/drafts/"+url.PathEscape(args[1])+"/test", map[string]any{})
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
	path := "/connections"
	if *dir != "" {
		path += "?" + url.Values{"direction": {*dir}}.Encode()
	}
	return pass(c, "GET", path, nil)
}

// pass 调一条路由并原样输出响应（新增字段不会被吞掉）。
func pass(c *client.Ctx, method, path string, body any) error {
	var raw map[string]any
	if err := c.Do(context.Background(), method, path, body, &raw); err != nil {
		return err
	}
	return c.Print(raw)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func propose(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("connection propose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	typ, name, id := fs.String("type", "", ""), fs.String("name", "", ""), fs.String("connection", "", "")
	stdin := fs.Bool("secret-stdin", false, "")
	var sets, routes multi
	fs.Var(&sets, "set", "")
	fs.Var(&routes, "route", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments")
	}
	if *typ == "" && *id == "" {
		return usage("give --type for a new channel or --connection for an existing one")
	}
	body := map[string]any{}
	for k, v := range map[string]string{"type": *typ, "name": *name, "connection": *id} {
		if v != "" {
			body[k] = v
		}
	}
	if len(sets) > 0 {
		cfg := map[string]string{}
		for _, kv := range sets {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return usage("--set takes <key>=<value>")
			}
			cfg[k] = v
		}
		body["config"] = cfg
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "route" {
			body["routes"] = []string(routes)
		}
	})
	if *stdin {
		var sec map[string]string
		if err := json.NewDecoder(c.Env.Stdin).Decode(&sec); err != nil {
			return usage("--secret-stdin expects one JSON object of credential fields on standard input") // 不回显输入
		}
		body["secrets"] = sec
	}
	return pass(c, "POST", "/connections/drafts", body)
}
