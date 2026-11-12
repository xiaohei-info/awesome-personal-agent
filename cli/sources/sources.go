// Package sources 是 apa 的来源命令组（附录 B.1「来源」）：list/show/add/enable/disable、webhook create|rotate|revoke、ingests、rejects、replay，
// 以及本机的 `apa source auth <id>`（设计 §4.2：启动附带拉取程序的认证模式，凭据写入来源自己的目录，内核不读）。
// 除 auth 外只是内核 /v1/sources 的薄客户端：授权、校验、可执行命令只 Web（退出码 20）都由内核判定。
package sources

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const usageText = "usage: apa source list | show <id> | add --id ID [--type pull|webhook|script] [--command C] [--schedule D] [--timeout S] [--source-type T] [--credentials-path P] [--dedupe-domain D] [--default-project P] [--config JSON] [--enabled=false] | " +
	"enable <id> | disable <id> | webhook create|rotate|revoke <id> | ingests <id> | rejects <id> [--after N] [--limit N] | replay <ingest-id> | auth <id>"

func init() { client.Register("source", source) }

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

func source(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no source subcommand given", usageText)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		if len(rest) != 0 {
			return usage("list takes no arguments", usageText)
		}
		return call(c, "GET", "/sources", nil)
	case "show":
		id, err := oneID(rest, "show")
		if err != nil {
			return err
		}
		return call(c, "GET", "/sources/"+url.PathEscape(id), nil)
	case "add":
		body, err := addBody(rest)
		if err != nil {
			return err
		}
		return call(c, "POST", "/sources", body)
	case "enable", "disable":
		id, err := oneID(rest, sub)
		if err != nil {
			return err
		}
		return call(c, "PATCH", "/sources/"+url.PathEscape(id), map[string]any{"enabled": sub == "enable"})
	case "webhook":
		if len(rest) == 0 || (rest[0] != "create" && rest[0] != "rotate" && rest[0] != "revoke") {
			return usage("webhook needs create, rotate or revoke", usageText)
		}
		id, err := oneID(rest[1:], "webhook "+rest[0])
		if err != nil {
			return err
		}
		method := "POST"
		if rest[0] == "revoke" {
			method = "DELETE"
		}
		return call(c, method, "/sources/"+url.PathEscape(id)+"/webhook", nil)
	case "ingests":
		id, err := oneID(rest, "ingests")
		if err != nil {
			return err
		}
		return call(c, "GET", "/sources/"+url.PathEscape(id)+"/ingests", nil)
	case "rejects":
		path, err := rejectsPath(rest)
		if err != nil {
			return err
		}
		return call(c, "GET", path, nil)
	case "replay":
		id, err := oneID(rest, "replay")
		if err != nil {
			return err
		}
		return call(c, "POST", "/ingests/"+url.PathEscape(id)+"/replay", nil)
	case "auth":
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			return usage("exactly one source id is required", usageText)
		}
		return auth(c, rest[0])
	}
	return usage("unknown source subcommand "+sub, usageText)
}

func call(c *client.Ctx, method, path string, body any) error {
	var out any
	if err := c.Do(context.Background(), method, path, body, &out); err != nil {
		return err
	}
	return c.Print(out)
}

func oneID(args []string, sub string) (string, error) {
	if len(args) != 1 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return "", usage(sub+" takes exactly one id", usageText)
	}
	return args[0], nil
}

// rejectsPath 解析 `rejects <id> [--after N] [--limit N]`；分页取值由内核校验，next_after 原样给下一次的 --after。
func rejectsPath(args []string) (string, error) {
	if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return "", usage("rejects takes an id first", usageText)
	}
	fs := flag.NewFlagSet("source rejects", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	q := url.Values{}
	for _, k := range []string{"after", "limit"} {
		fs.Func(k, "", func(v string) error { q.Set(k, v); return nil })
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return "", usage("invalid arguments for source rejects", usageText)
	}
	path := "/sources/" + url.PathEscape(args[0]) + "/rejects"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path, nil
}

// addBody 只带命令行上出现过的字段。带 --command 而没写 --type 视为自定义命令（script）：
// 内核对 delegate 返回 web_only（20）；附带的拉取程序要显式写 --type pull。
func addBody(args []string) (map[string]any, error) {
	fs := flag.NewFlagSet("source add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	body := map[string]any{}
	str := func(flagName, key string) {
		fs.Func(flagName, "", func(v string) error { body[key] = v; return nil })
	}
	str("id", "id")
	str("type", "type")
	str("command", "command")
	str("schedule", "schedule")
	str("source-type", "source_type")
	str("credentials-path", "credentials_path")
	str("dedupe-domain", "dedupe_domain")
	str("default-project", "default_project")
	fs.Func("timeout", "", func(v string) error {
		n, err := strconv.Atoi(v)
		body["timeout"] = n
		return err
	})
	fs.Func("config", "", func(v string) error {
		if !json.Valid([]byte(v)) {
			return strconv.ErrSyntax
		}
		body["config"] = json.RawMessage(v)
		return nil
	})
	fs.Func("enabled", "", func(v string) error {
		b, err := strconv.ParseBool(v)
		body["enabled"] = b
		return err
	})
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return nil, usage("invalid arguments for source add", usageText)
	}
	if body["id"] == nil {
		return nil, usage("source add needs --id", usageText)
	}
	if body["type"] == nil && body["command"] != nil {
		body["type"] = string(domain.SourceScript)
	}
	return body, nil
}

// auth 经 API 读来源登记（CLI 不碰存储），在本机运行 `<命令> auth <credentials_path>`，凭据写在登记的路径里（§9.2）。
// 程序失败时退出码是 1，详情在它自己的输出里。
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
	if s.CredentialsPath == "" {
		return usage("source "+id+" has no credentials_path", "register the source with a credentials_path, then run `apa source auth` again")
	}
	if err := os.MkdirAll(s.CredentialsPath, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "auth", s.CredentialsPath)...)
	cmd.Dir = s.CredentialsPath
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Env.Stdin, c.Env.Stdout, c.Env.Stderr
	return cmd.Run()
}
