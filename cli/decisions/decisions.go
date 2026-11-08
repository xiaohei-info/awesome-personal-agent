// Package decisions 是 apa 的决策与建议命令（附录 B.1、设计 §7.2、§14.5）：ask、decide、accept、dismiss、grant list。
//
// 本包只把命令行参数搬成 POST /v1/items/{id}/{ask|decide|accept|dismiss} 的请求体：
// 授权（web_only 20、初筛未返回 22、运行身份 4）、代答证据与"只认第一次回答"（6）都由内核判定，
// 错误体连同 web_url 由 cli/client 原样还原并映射为退出码。这里没有任何绕过 web_only 的选项，也不读写存储。
package decisions

import (
	"context"
	"flag"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() {
	client.Register("ask", ask)
	client.Register("decide", decide)
	client.Register("accept", accept)
	client.Register("dismiss", dismiss)
	client.Register("grant", grant)
}

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

// kv 收集可重复的 key=value 标志。
type kv []string

func (k *kv) String() string     { return strings.Join(*k, ",") }
func (k *kv) Set(v string) error { *k = append(*k, v); return nil }

// parse 取首个位置参数作为事项 id，其余按 flag 解析；多余的位置参数是用法错误。
func parse(name, synopsis string, args []string, define func(*flag.FlagSet)) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", usage("missing item id for "+name, "usage: "+synopsis)
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return "", usage("invalid arguments for "+name, "usage: "+synopsis)
	}
	return args[0], nil
}

// post 发写请求；空串字段不进入请求体，由内核按缺省或校验处理。
func post(c *client.Ctx, id, verb string, body map[string]any) error {
	for k, v := range body {
		if s, ok := v.(string); ok && s == "" {
			delete(body, k)
		}
	}
	var out any
	if err := c.Do(context.Background(), "POST", "/items/"+url.PathEscape(id)+"/"+verb, body, &out); err != nil {
		return err
	}
	return c.Print(out)
}

const askUsage = `apa ask <id> --question <q> [--option <id=label>]... [--next <id=phase>]... [--recommend <id>] [--free-text] [--deadline <RFC3339> --default-on-timeout <id>] [--resume-note <t>] [--reason <t>]`

func ask(c *client.Ctx, args []string) error {
	var q, rec, defTimeout, deadline, note, reason string
	var free bool
	var opts, nexts kv
	id, err := parse("ask", askUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&q, "question", "", "")
		fs.Var(&opts, "option", "")
		fs.Var(&nexts, "next", "")
		fs.StringVar(&rec, "recommend", "", "")
		fs.BoolVar(&free, "free-text", false, "")
		fs.StringVar(&deadline, "deadline", "", "")
		fs.StringVar(&defTimeout, "default-on-timeout", "", "")
		fs.StringVar(&note, "resume-note", "", "")
		fs.StringVar(&reason, "reason", "", "")
	})
	if err != nil {
		return err
	}
	next := map[string]string{}
	for _, n := range nexts {
		k, v, ok := strings.Cut(n, "=")
		if !ok || k == "" || v == "" {
			return usage("--next must be <option-id>=<phase>", "usage: "+askUsage)
		}
		next[k] = v
	}
	options := make([]map[string]string, 0, len(opts))
	for _, o := range opts {
		oid, label, ok := strings.Cut(o, "=")
		if !ok || oid == "" || label == "" {
			return usage("--option must be <id>=<label>", "usage: "+askUsage)
		}
		opt := map[string]string{"id": oid, "label": label}
		if p, has := next[oid]; has {
			opt["next"] = p
			delete(next, oid)
		}
		options = append(options, opt)
	}
	if len(next) > 0 {
		return usage("--next refers to an option that was not given with --option", "usage: "+askUsage)
	}
	a := map[string]any{"question": q, "recommend": rec, "default_on_timeout": defTimeout, "resume_note": note}
	if len(options) > 0 {
		a["options"] = options
	}
	if free {
		a["free_text"] = true
	}
	if deadline != "" {
		if _, err := time.Parse(time.RFC3339, deadline); err != nil {
			return usage("--deadline must be an RFC3339 time", "e.g. --deadline 2026-01-02T15:04:05Z")
		}
		a["deadline"] = deadline
	}
	for k, v := range a {
		if s, ok := v.(string); ok && s == "" {
			delete(a, k)
		}
	}
	return post(c, id, "ask", map[string]any{"ask": a, "reason": reason})
}

const decideUsage = `apa decide <id> (--option <id> | --text <t>) [--evidence <owner's words>] [--reason <t>]`

func decide(c *client.Ctx, args []string) error {
	var opt, text, reason string
	id, err := parse("decide", decideUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&opt, "option", "", "")
		fs.StringVar(&text, "text", "", "")
		fs.StringVar(&reason, "reason", "", "")
	})
	if err != nil {
		return err
	}
	return post(c, id, "decide", map[string]any{"choice": opt, "text": text, "reason": reason})
}

const acceptUsage = `apa accept <id> [--evidence <owner's words>] [--reason <t>]`

func accept(c *client.Ctx, args []string) error {
	var reason string
	id, err := parse("accept", acceptUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&reason, "reason", "", "")
	})
	if err != nil {
		return err
	}
	return post(c, id, "accept", map[string]any{"reason": reason})
}

const dismissUsage = `apa dismiss <id> [--reason <t>] [--evidence <owner's words>]`

func dismiss(c *client.Ctx, args []string) error {
	var reason string
	id, err := parse("dismiss", dismissUsage, args, func(fs *flag.FlagSet) {
		fs.StringVar(&reason, "reason", "", "")
	})
	if err != nil {
		return err
	}
	return post(c, id, "dismiss", map[string]any{"reason": reason})
}

const grantUsage = "usage: apa grant list"

// grant list 只读：列出该凭据名下未撤销、未过期的授权单。签发和撤销只在 Web，这里没有对应的写命令。
func grant(c *client.Ctx, args []string) error {
	if len(args) != 1 || args[0] != "list" {
		return usage("apa grant takes the subcommand list", grantUsage)
	}
	var out any
	if err := c.Do(context.Background(), "GET", "/grants", nil, &out); err != nil {
		return err
	}
	return c.Print(out)
}
