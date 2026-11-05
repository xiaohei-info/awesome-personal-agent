// Package packs 实现 `apa pack list/validate` 与 `apa instance add/list/enable/disable`（附录 B.1、附录 C.10/C.11）。
// list 与 instance 是内核 /v1/packs、/v1/instances 的薄客户端：授权、参数校验与事件都在内核。
// validate 不连内核：直接调用内核的包校验函数在本地预检，与加载器同一套规则，因此可离线运行。
// 本包没有切规则模式的命令——规则切 auto 只走 Web 会话（§14.8）。
package packs

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
)

func init() {
	client.Register("pack", runPack)
	client.Register("instance", runInstance)
}

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

const (
	packUsage     = "usage: apa pack list | validate <dir>"
	instanceUsage = "usage: apa instance list | add <pack> --param k=v [--param k=v]... [--project P] [--disabled] | enable <id> | disable <id>"
)

func runPack(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no pack subcommand given", packUsage)
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return usage("list takes no arguments", packUsage)
		}
		return call(c, "GET", "/packs", nil)
	case "validate":
		if len(args) != 2 || args[1] == "" || strings.HasPrefix(args[1], "-") {
			return usage("validate takes exactly one pack directory", packUsage)
		}
		return validate(c, args[1])
	}
	return usage("unknown pack subcommand "+args[0], packUsage)
}

// validate 预检本地包目录。被拒绝时退出码 2，why 带加载器给出的拒绝原因（来自包内容，不含账本数据）。
func validate(c *client.Ctx, dir string) error {
	p, err := kp.Validate(dir, items.NewKinds().CheckSchema)
	if err != nil {
		return usage("pack rejected: "+err.Error(), "fix the pack as described, then run `apa pack validate` again")
	}
	return c.Print(map[string]any{"ok": true, "name": p.Name, "version": p.Version, "hash": p.Hash})
}

func runInstance(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no instance subcommand given", instanceUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		if len(rest) != 0 {
			return usage("list takes no arguments", instanceUsage)
		}
		return call(c, "GET", "/instances", nil)
	case "enable", "disable":
		if len(rest) != 1 || rest[0] == "" || strings.HasPrefix(rest[0], "-") {
			return usage(sub+" takes exactly one instance id", instanceUsage)
		}
		return call(c, "PATCH", "/instances/"+url.PathEscape(rest[0]), map[string]any{"enabled": sub == "enable"})
	case "add":
		body, err := addBody(rest)
		if err != nil {
			return err
		}
		return call(c, "POST", "/instances", body)
	}
	return usage("unknown instance subcommand "+sub, instanceUsage)
}

// addBody 解析 `<包> --param k=v ...`。值按 JSON 解读（3、true、"x"、[1]），不是合法 JSON 时按字符串；
// 是否合乎包的 instance_params 由内核判定。
func addBody(args []string) (map[string]any, error) {
	if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return nil, usage("add needs a pack name first", instanceUsage)
	}
	params := map[string]json.RawMessage{}
	fs := flag.NewFlagSet("instance", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "")
	disabled := fs.Bool("disabled", false, "")
	fs.Func("param", "", func(kv string) error {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return errors.New("param must be k=v")
		}
		if !json.Valid([]byte(v)) {
			b, _ := json.Marshal(v)
			v = string(b)
		}
		params[k] = json.RawMessage(v)
		return nil
	})
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return nil, usage("invalid arguments for add", instanceUsage)
	}
	body := map[string]any{"pack": args[0], "params": params}
	if *project != "" {
		body["project"] = *project
	}
	if *disabled {
		body["enabled"] = false
	}
	return body, nil
}

func call(c *client.Ctx, method, path string, body map[string]any) error {
	var in, out any
	if body != nil {
		in = body
	}
	if err := c.Do(context.Background(), method, path, in, &out); err != nil {
		return err
	}
	return c.Print(out)
}
