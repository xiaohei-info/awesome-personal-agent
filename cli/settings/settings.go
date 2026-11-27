// Package settings 是 apa 的设置与物理删除命令（附录 B.1「设置」）：Web 能做的设置都有 CLI 入口。
//
// 薄客户端：settings get/put/post/delete 原样调 /v1/settings/<路径>，请求体从文件或标准输入读；
// 能不能做由服务端按调用者的权限卡判定（§14.3），这里不判、不改写。
package settings

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() {
	client.Register("settings", usageHint, run)
	client.Register("purge", purgeHint, purge)
}

const usageHint = "usage: apa settings get <路径> | put <路径> [<JSON 文件>|-] | post <路径> [<JSON 文件>|-] | delete <路径>"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no settings subcommand given")
	}
	method := map[string]string{"get": "GET", "put": "PUT", "post": "POST", "delete": "DELETE"}[args[0]]
	if method == "" {
		return usage("unknown settings subcommand " + args[0])
	}
	rest := args[1:]
	if len(rest) == 0 || len(rest) > 2 || (len(rest) == 2 && (method == "GET" || method == "DELETE")) {
		return usage("give one settings path, and for put or post an optional JSON file or - for stdin")
	}
	path := "/settings/" + strings.TrimPrefix(rest[0], "/")
	if strings.Contains(path, "..") {
		return usage("the path must stay under /settings")
	}
	var body any
	if method == "PUT" || method == "POST" {
		raw, err := readJSON(c, rest[1:])
		if err != nil {
			return err
		}
		body = raw
	}
	var out json.RawMessage
	if err := c.Do(context.Background(), method, path, body, &out); err != nil {
		return err
	}
	if len(out) == 0 {
		return nil
	}
	return c.Print(out)
}

// readJSON 读请求体：给了文件读文件，- 或省略读标准输入；必须是合法 JSON。
func readJSON(c *client.Ctx, from []string) (json.RawMessage, error) {
	var (
		b   []byte
		err error
	)
	if len(from) == 1 && from[0] != "-" {
		b, err = os.ReadFile(from[0])
	} else {
		b, err = io.ReadAll(c.Env.Stdin)
	}
	if err != nil {
		return nil, usage("cannot read the request body: " + err.Error())
	}
	if !json.Valid(b) {
		return nil, usage("the request body is not valid JSON")
	}
	return json.RawMessage(b), nil
}

const purgeHint = "usage: apa purge <事项、材料或来源事件 id>"

// purge 物理删除一个对象（主文档 §6.15）：属权限卡的"物理删除"组，开放只能限时。
func purge(c *client.Ctx, args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return domain.NewError(domain.CodeInvalidRequest, "apa purge takes exactly one object id", purgeHint)
	}
	var out json.RawMessage
	if err := c.Do(context.Background(), "POST", "/settings/purge", map[string]string{"object": args[0]}, &out); err != nil {
		return err
	}
	return c.Print(out)
}
