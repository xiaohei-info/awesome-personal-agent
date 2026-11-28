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
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("project", usageText, run) }

const usageText = "usage: apa project list [--full] | show <id> | create --name N [--intro T] [--tag T]... [--status S] | " +
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
		full := len(rest) == 1 && rest[0] == "--full"
		if len(rest) != 0 && !full {
			return usage("list takes only --full")
		}
		return list(ctx, c, full)
	case "show":
		id, rest, err := splitID(rest)
		if err != nil || len(rest) != 0 {
			return usage("show takes exactly one project id")
		}
		// 逐级往下：这个项目的未结事项、在项目内检索、项目规范（没有时是项目材料）（§17.2）。
		var out map[string]any
		if err := c.Do(ctx, "GET", "/projects/"+url.PathEscape(id), nil, &out); err != nil {
			return err
		}
		last := "apa material list --project " + id
		if card, _ := out["card"].(map[string]any); card != nil {
			if g, _ := card["guideline"].(string); g != "" {
				last = "apa material show " + g
			}
		}
		out["next"] = []string{"apa item list --project " + id + " --phase open", "apa search <words> --project " + id, last}
		return c.Print(out)
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

// listKeys 是项目清单默认每行留下的字段；仓库、执行者与采集设置用 project show 或 --full。
var listKeys = []string{"id", "name", "intro", "tags", "status", "system_role"}

// list 是项目清单：默认每行只留 listKeys、简介截到 60 字，并且不超过一次读取的上限（放不下的从末尾删掉，卡片随之删），
// 写明总数，另加"下一步"。--full 原样给完整的行。
func list(ctx context.Context, c *client.Ctx, full bool) error {
	var out map[string]any
	if err := c.Do(ctx, "GET", "/projects", nil, &out); err != nil {
		return err
	}
	next := []string{"apa project show <id>", "apa search <words>"}
	if full {
		out["next"] = next
		return c.Print(out)
	}
	rows, _ := out["projects"].([]any)
	for i, r := range rows {
		if p, ok := r.(map[string]any); ok {
			slim := map[string]any{}
			for _, k := range listKeys {
				if v, ok := p[k]; ok {
					slim[k] = v
				}
			}
			if intro, _ := slim["intro"].(string); len([]rune(intro)) > 60 {
				slim["intro"] = string([]rune(intro)[:60]) + "…"
			}
			rows[i] = slim
		}
	}
	cards, _ := out["cards"].(map[string]any)
	out["total"] = len(rows)
	client.Fit(out, "projects", kcontext.InjectBudget, func(kept int) {
		kc := map[string]any{}
		for _, r := range rows[:kept] {
			if p, ok := r.(map[string]any); ok {
				id, _ := p["id"].(string)
				kc[id] = cards[id]
			}
		}
		out["cards"] = kc
		if out["next"] = next; kept < len(rows) { // 放不下的项目按名字找
			out["next"] = []string{"apa project show <id>", "apa search <words> --type project"}
		}
	})
	return c.Print(out)
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
	if !create { // 新项目的对话捕获恒为开启，创建时没有这一项；update 保留它（3a，默认已开，一般不需要）
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
	}
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
