// Package context 实现 `apa context` 与 `apa persona`（附录 B.1、设计 §7.6、§11.2）。
// 薄客户端：增量、游标、注入渲染、目录到项目的映射都在内核（GET /v1/context）。
// 这里只把明确给出的 session、scope、project 与当前目录/主机名搬进请求，不猜、不计算。
package context

import (
	"context"
	"flag"
	"io"
	"net/url"
	"os"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() {
	client.Register("context", run)
	client.Register("persona", persona)
}

func usage(why, usageText string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, usageText)
}

const contextUsage = "usage: apa context [--session ID] [--scope project|cwd|item|all] [--item ID] [--project ID] [--path DIR] [--host H] [--instruction N] [--format json|inject]"

func run(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	session := fs.String("session", "", "")
	scope := fs.String("scope", "", "")
	item := fs.String("item", "", "")
	project := fs.String("project", "", "")
	path := fs.String("path", "", "")
	host := fs.String("host", "", "")
	format := fs.String("format", "", "")
	instruction := fs.Int("instruction", 0, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *instruction < 0 {
		return usage("invalid arguments for context", contextUsage)
	}
	switch *format {
	case "", "json", "inject":
	default:
		return usage("format must be json or inject", contextUsage)
	}

	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("scope", *scope)
	set("item", *item)
	set("format", *format)
	if *instruction > 0 {
		q.Set("instruction", strconv.Itoa(*instruction))
	}
	// 项目：--project 优先，其次 APA_PROJECT；二者都没有才用当前目录交给内核映射。事项作用域不需要项目。
	if p := firstNonEmpty(*project, c.Project()); p != "" {
		q.Set("project", p)
	} else if *scope != "item" {
		cwd := *path
		if cwd == "" {
			cwd, _ = os.Getwd() // 取不到就不带目录：内核按无项目作用域处理，不猜
		}
		if cwd != "" {
			h := *host
			if h == "" {
				h, _ = os.Hostname()
			}
			if h == "" {
				return usage("cannot determine this machine's host name; pass --host", contextUsage)
			}
			q.Set("path", cwd)
			q.Set("host", h)
		}
	}

	cc := c
	if *session != "" { // 显式会话覆盖 APA_SESSION；内核用同一个头记游标
		cp := *c
		cp.Env.Getenv = func(k string) string {
			if k == client.EnvSession {
				return *session
			}
			return c.Env.Getenv(k)
		}
		cc = &cp
	}

	if *format == "inject" {
		var raw []byte
		if err := cc.Do(context.Background(), "GET", "/context?"+q.Encode(), nil, &raw); err != nil {
			return err
		}
		_, err := c.Env.Stdout.Write(raw)
		return err
	}
	var out any
	if err := cc.Do(context.Background(), "GET", "/context?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	return c.Print(out)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// persona 输出当前人格：本人写的，没写则是内置默认（附录 B.1）。语言取 --lang，其次 Web 里的设置，
// 再其次进程的区域设置（LC_ALL、LC_MESSAGES、LANG，作为 locale 交给内核），都没有按英文。
func persona(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("persona", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	lang := fs.String("lang", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for persona", "usage: apa persona [--lang zh|en]")
	}
	q := url.Values{}
	if *lang != "" {
		q.Set("lang", *lang)
	}
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := c.Env.Getenv(k); v != "" {
			q.Set("locale", v)
			break
		}
	}
	path := "/context/persona"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Persona string `json:"persona"`
	}
	if err := c.Do(context.Background(), "GET", path, nil, &out); err != nil {
		return err
	}
	_, err := io.WriteString(c.Env.Stdout, out.Persona+"\n")
	return err
}
