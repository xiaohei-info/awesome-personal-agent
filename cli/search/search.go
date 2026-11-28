// Package search 实现 `apa search`（附录 B.1「检索」、设计 §7.1）：跨项目、跨类型的有界检索，是 GET /search 的薄客户端。
// 输出不超过一次读取的字数上限（kcontext.InjectBudget），超出的行留给续页；每次都带至多三条"下一步"命令。
package search

import (
	"context"
	"flag"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const usageText = "usage: apa search <query> [--type item|material|project|source]... [--project <id>] [--phase <p>] [--kind <k>] [--tag <t>]... " +
	"[--item <id>] [--after <YYYY-MM-DD>] [--before <YYYY-MM-DD>] [--include-dropped] [--limit <1-50>] [--cursor <c>]"

func init() { client.Register("search", usageText, run) }

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || strings.TrimSpace(args[0]) == "" {
		return domain.NewError(domain.CodeInvalidRequest, "a query is required first", usageText)
	}
	text, rest := args[0], args[1:]
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var types, tags multi
	fs.Var(&types, "type", "")
	fs.Var(&tags, "tag", "")
	vals := map[string]*string{}
	for _, n := range []string{"project", "phase", "kind", "item", "after", "before", "limit", "cursor"} {
		vals[n] = fs.String(n, "", "")
	}
	dropped := fs.Bool("include-dropped", false, "")
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
		return domain.NewError(domain.CodeInvalidRequest, "invalid arguments for search", usageText)
	}
	q := url.Values{"q": {text}}
	for n, v := range vals {
		if *v != "" {
			q.Set(n, *v)
		}
	}
	for _, t := range types {
		q.Add("type", t)
	}
	for _, t := range tags {
		q.Add("tag", t)
	}
	if *dropped {
		q.Set("include_dropped", "true")
	}
	if *vals["project"] == "" {
		if here := here(c); here != "" {
			q.Set("here", here)
		}
	}
	var out map[string]any
	if err := c.Do(context.Background(), "GET", "/search?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	offset, _ := strconv.Atoi(*vals["cursor"])
	rows, _ := out["hits"].([]any)
	server, _ := out["next_cursor"].(string)
	client.Fit(out, "hits", kcontext.InjectBudget, func(kept int) {
		if out["next_cursor"] = server; kept < len(rows) {
			out["next_cursor"] = strconv.Itoa(offset + kept)
		}
		if out["next_cursor"] == "" {
			delete(out, "next_cursor")
		}
		out["next"] = next(text, rest, out)
	})
	return c.Print(out)
}

// here 是当前所在的项目（APA_PROJECT，其次当前目录的映射），只用于加权；查不到就不加。
func here(c *client.Ctx) string {
	if p := c.Project(); p != "" {
		return p
	}
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	if cwd == "" || host == "" {
		return ""
	}
	var r struct {
		Project *struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if c.Do(context.Background(), "GET", "/projects/resolve?"+url.Values{"path": {cwd}, "host": {host}}.Encode(), nil, &r) != nil || r.Project == nil {
		return ""
	}
	return r.Project.ID
}

// show 是读一条命中的命令。
func show(typ, id string) string {
	switch typ {
	case "material":
		return "apa material show " + id
	case "project":
		return "apa project show " + id
	case "source":
		return "apa source event " + id
	}
	return "apa item show " + id
}

// next 给至多三条下一步：读第一条命中、续页、查已放弃的；零命中时给少几个词、查已放弃的、查来源事件。
func next(text string, rest []string, out map[string]any) []string {
	base := "apa search " + client.Quote(text)
	var flags []string
	for i := 0; i < len(rest); i++ { // 续页沿用原来的筛选，去掉旧游标
		if rest[i] == "--cursor" {
			i++
			continue
		}
		if !strings.HasPrefix(rest[i], "--cursor=") {
			flags = append(flags, client.Quote(rest[i]))
		}
	}
	with := func(extra ...string) string {
		return strings.Join(append(append([]string{base}, flags...), extra...), " ")
	}
	var cmds []string
	rows, _ := out["hits"].([]any)
	if len(rows) > 0 {
		if h, ok := rows[0].(map[string]any); ok {
			typ, _ := h["type"].(string)
			id, _ := h["id"].(string)
			cmds = append(cmds, show(typ, id))
		}
		if c, _ := out["next_cursor"].(string); c != "" {
			cmds = append(cmds, with("--cursor", c))
		}
	} else if shorter := shorten(text); shorter != "" {
		cmds = append(cmds, "apa search "+client.Quote(shorter))
	}
	if w, _ := out["withheld"].(map[string]any); w["dropped"] != nil || len(rows) == 0 {
		cmds = append(cmds, with("--include-dropped"))
	}
	if len(rows) == 0 {
		cmds = append(cmds, "apa search "+client.Quote(text)+" --type source")
	}
	return cmds[:min(3, len(cmds))]
}

// shorten 是零命中时"少几个词"的建议：多个词时取第一个，一整句中文时取前四个字；单个英文词不拆。
func shorten(text string) string {
	if f := strings.Fields(text); len(f) > 1 {
		return f[0]
	}
	if rs := []rune(strings.TrimSpace(text)); len(rs) > 4 && unicode.Is(unicode.Han, rs[0]) {
		return string(rs[:4])
	}
	return ""
}
