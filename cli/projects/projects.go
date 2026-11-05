// Package projects 实现 `apa project list/show/create/update/repos`（附录 B.1、设计 §12.1、§14.7）。
// 只是内核 /v1/projects 的薄客户端：授权、校验与事件都在内核。
// allowed_executors 没有成功路径：带上该字段时原样交给内核，由内核按调用者级别返回 20 或 4。
package projects

import (
	"context"
	"flag"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("project", run) }

const usageText = "usage: apa project list | show <id> | create --name N [--intro T] [--tag T]... [--status S] [--capture=bool] | " +
	"update <id> [--name N] [--intro T] [--tag T]... [--status S] [--capture=bool] | " +
	"repos add|remove <id> --remote URL | repos add|remove <id> --path DIR [--host H]"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageText) }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no project subcommand given")
	}
	sub, rest := args[0], args[1:]
	ctx := context.Background()
	switch sub {
	case "list":
		if len(rest) != 0 {
			return usage("list takes no arguments")
		}
		return call(ctx, c, "GET", "/projects", nil)
	case "show":
		id, rest, err := splitID(rest)
		if err != nil || len(rest) != 0 {
			return usage("show takes exactly one project id")
		}
		return call(ctx, c, "GET", "/projects/"+url.PathEscape(id), nil)
	case "create":
		body, err := fields(rest, true)
		if err != nil {
			return err
		}
		return call(ctx, c, "POST", "/projects", body)
	case "update":
		id, rest, err := splitID(rest)
		if err != nil {
			return usage("update needs a project id first")
		}
		body, err := fields(rest, false)
		if err != nil {
			return err
		}
		return call(ctx, c, "PATCH", "/projects/"+url.PathEscape(id), body)
	case "repos":
		return repos(ctx, c, rest)
	}
	return usage("unknown project subcommand " + sub)
}

func call(ctx context.Context, c *client.Ctx, method, path string, body map[string]any) error {
	var out any
	var in any
	if body != nil {
		in = body
	}
	if err := c.Do(ctx, method, path, in, &out); err != nil {
		return err
	}
	return c.Print(out)
}

// splitID 取第一个位置参数作为项目 id（flag 包在首个非 flag 参数处停止，所以先摘出来）。
func splitID(args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || args[0] == "" {
		return "", nil, usage("missing project id")
	}
	return args[0], args[1:], nil
}

func newFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("project", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// fields 把命令行解析为请求体；只携带用户显式给出的字段，PATCH 因此是部分更新。
func fields(args []string, create bool) (map[string]any, error) {
	fs := newFlags()
	body := map[string]any{}
	str := func(name string) {
		fs.Func(name, "", func(v string) error { body[name] = v; return nil })
	}
	str("name")
	str("intro")
	str("status")
	fs.Func("tag", "", func(v string) error {
		tags, _ := body["tags"].([]string)
		body["tags"] = append(tags, v)
		return nil
	})
	fs.Func("capture", "", func(v string) error {
		switch v {
		case "true", "":
			body["capture"] = true
		case "false":
			body["capture"] = false
		default:
			return os.ErrInvalid
		}
		return nil
	})
	args = bareCapture(args)
	fs.Func("allowed-executors", "", func(v string) error {
		ids := []string{}
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		body["allowed_executors"] = ids
		return nil
	})
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return nil, usage("invalid arguments for project")
	}
	if create && body["name"] == nil {
		return nil, usage("create needs --name")
	}
	if !create && len(body) == 0 {
		return nil, usage("update needs at least one field to change")
	}
	return body, nil
}

// --capture 可以不带值：`--capture` 等价于 `--capture=true`。
func bareCapture(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "--capture" || a == "-capture" {
			a = "--capture=true"
		}
		out[i] = a
	}
	return out
}

// repos：给了 --path 就是本机检出（worker 可写），否则是远程地址（需要 delegate）。谁能做由内核判定。
func repos(ctx context.Context, c *client.Ctx, args []string) error {
	if len(args) < 1 || (args[0] != "add" && args[0] != "remove") {
		return usage("repos needs add or remove")
	}
	verb := args[0]
	id, rest, err := splitID(args[1:])
	if err != nil {
		return usage("repos " + verb + " needs a project id first")
	}
	fs := newFlags()
	remote := fs.String("remote", "", "")
	host := fs.String("host", "", "")
	path := fs.String("path", "", "")
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for repos")
	}
	body := map[string]any{}
	var op string
	switch {
	case *path != "":
		h := *host
		if h == "" {
			if h, err = os.Hostname(); err != nil {
				return usage("cannot determine this machine's host name; pass --host")
			}
		}
		body["host"], body["path"] = h, *path
		if *remote != "" {
			body["remote"] = *remote
		}
		op = map[string]string{"add": "set_checkout", "remove": "remove_checkout"}[verb]
	case *remote != "":
		body["remote"] = *remote
		op = map[string]string{"add": "add_remote", "remove": "remove_remote"}[verb]
	default:
		return usage("repos needs --remote or --path")
	}
	body["op"] = op
	return call(ctx, c, "POST", "/projects/"+url.PathEscape(id)+"/repos", body)
}
