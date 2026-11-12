// Package items 是 apa 的事项命令组（附录 B.1「事项」「上下文」）：
// item show/list/add/update/transition/relate、propose、inbox。
//
// 这里只做参数到 HTTP 请求的搬运：校验、授权、版本、幂等、到期时钟全部由内核判定，
// 内核的错误体与退出码原样透出；不直接访问存储。
package items

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

func init() {
	client.Register("item", item)
	client.Register("today", today)
	client.Register("propose", propose)
	client.Register("inbox", inbox)
}

var bg = context.Background()

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

const itemUsage = "usage: apa item show <id> | list [--phase --kind --project --assignee] | add | update <id> | progress <id> --text <t> [--on <YYYY-MM-DD>] | transition <id> --to <phase> --reason <r> | relate <from> --to <item or material id> --type <t>"

func item(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no item subcommand given", itemUsage)
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "show":
		return show(c, args)
	case "list":
		return list(c, args)
	case "add":
		return add(c, args, "item add")
	case "update":
		return write(c, args, "update", "PATCH", "")
	case "progress":
		return write(c, args, "progress", "POST", "/progress")
	case "transition":
		return write(c, args, "transition", "POST", "/transition")
	case "relate":
		return write(c, args, "relate", "POST", "/relations")
	}
	return usage("unknown item subcommand "+sub, itemUsage)
}

func propose(c *client.Ctx, args []string) error { return add(c, args, "propose") }

// form 收集一条命令的 flag：只有命令行上出现过的 flag 才进请求体，空值表示清空（JSON null）。
type form struct {
	fs   *flag.FlagSet
	vals map[string]*string
	hdr  map[string]*string
}

func newForm(name string, body, header, boolean []string) (*form, map[string]*bool) {
	f := &form{fs: flag.NewFlagSet(name, flag.ContinueOnError), vals: map[string]*string{}, hdr: map[string]*string{}}
	f.fs.SetOutput(io.Discard)
	for _, n := range body {
		f.vals[n] = f.fs.String(n, "", "")
	}
	for _, n := range header {
		f.hdr[n] = f.fs.String(n, "", "")
	}
	bools := map[string]*bool{}
	for _, n := range boolean {
		bools[n] = f.fs.Bool(n, false, "")
	}
	return f, bools
}

// parse 解析 args；needID 时第一个位置参数是事项 id，必须写在 flag 之前。
func (f *form) parse(args []string, needID bool, hint string) (id string, _ error) {
	if needID {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return "", usage("an item id is required first", hint)
		}
		id, args = args[0], args[1:]
	}
	if err := f.fs.Parse(args); err != nil || f.fs.NArg() != 0 {
		return "", usage("invalid arguments", hint)
	}
	return id, nil
}

// body 把出现过的 flag 译成请求体字段。
func (f *form) body(hint string) (map[string]any, error) {
	m := map[string]any{}
	var bad error
	f.fs.Visit(func(fl *flag.Flag) {
		v, ok := f.vals[fl.Name]
		if !ok || bad != nil {
			return
		}
		key := strings.ReplaceAll(fl.Name, "-", "_")
		switch {
		case *v == "":
			m[key] = nil
		case fl.Name == "tags":
			m[key] = strings.Split(*v, ",")
		case fl.Name == "waiting-on":
			k, ref, found := strings.Cut(*v, ":")
			if !found || k == "" || ref == "" {
				bad = usage("--waiting-on must be <kind>:<ref>", "kind is human, executor, external or time; e.g. --waiting-on external:quote or --waiting-on time:2026-01-01T00:00:00Z")
				return
			}
			m[key] = map[string]string{"kind": k, "ref": ref}
		case fl.Name == "ask" || fl.Name == "spec":
			if !json.Valid([]byte(*v)) {
				bad = usage("--"+fl.Name+" must be valid JSON", hint)
				return
			}
			m[key] = json.RawMessage(*v)
		default:
			m[key] = *v
		}
	})
	return m, bad
}

// headers：--version → If-Match，--idempotency-key → Idempotency-Key（--evidence 是全局参数，由 client 以 X-APA-Evidence 头发出）。
func (f *form) headers() map[string]string {
	h := map[string]string{}
	for flagName, header := range map[string]string{"version": "If-Match", "idempotency-key": "Idempotency-Key"} {
		if p, ok := f.hdr[flagName]; ok && *p != "" {
			h[header] = *p
		}
	}
	return h
}

var itemFields = []string{"kind", "title", "summary", "body", "status-label", "priority", "project", "tags", "assignee", "waiting-on", "ask", "spec", "planned-for", "reason"}

// add 实现 item add 与 propose：POST /items。propose 不带 --phase，由内核缺省为 proposed。
func add(c *client.Ctx, args []string, name string) error {
	hint := "usage: apa " + name + " --kind <kind> --title <title> [--summary --body --priority --project --tags a,b --assignee --planned-for <YYYY-MM-DD> --reason]"
	fields := itemFields
	if name == "item add" {
		fields = append([]string{"phase"}, fields...)
		hint += " [--phase <phase>]"
	}
	hint += " [--evidence <text> | --grant <id>]"
	f, _ := newForm(name, fields, []string{"idempotency-key"}, nil)
	if _, err := f.parse(args, false, hint); err != nil {
		return err
	}
	body, err := f.body(hint)
	if err != nil {
		return err
	}
	if c.Evidence != "" {
		body["evidence"] = c.Evidence // 创建的体收 evidence；头同样有效
	}
	return send(c, f, "POST", "/items", body)
}

// write 实现 update / transition / relate：id 来自第一个位置参数。
func write(c *client.Ctx, args []string, sub, method, suffix string) error {
	var fields []string
	var bools []string
	hint := ""
	switch sub {
	case "update":
		fields = itemFields[1:]
		hint = "usage: apa item update <id> [--title --summary --body --status-label --priority --project --tags --assignee --waiting-on --ask --spec --planned-for <YYYY-MM-DD>] --reason <r> [--evidence <text> | --grant <id> --version <n>]"
	case "progress":
		fields = []string{"text", "on"}
		hint = "usage: apa item progress <id> --text <text> [--on <YYYY-MM-DD, not later than today>] [--evidence <text> | --grant <id>]"
	case "transition":
		fields = []string{"to", "reason", "waiting-on"}
		hint = "usage: apa item transition <id> --to <proposed|open|waiting|needs_input|done|dropped> --reason <r> [--waiting-on <kind>:<ref> --evidence <text> | --grant <id> --version <n>]"
	case "relate":
		fields, bools = []string{"to", "type", "reason"}, []string{"remove"}
		hint = "usage: apa item relate <from> --to <item or material id> --type <parent|blocks|derived_from|supersedes|relates|basis|output|minutes> [--remove] [--reason <r> --evidence <text> | --grant <id> --version <n>]"
	}
	f, rm := newForm("item "+sub, fields, []string{"version", "idempotency-key"}, bools)
	id, err := f.parse(args, true, hint)
	if err != nil {
		return err
	}
	body, err := f.body(hint)
	if err != nil {
		return err
	}
	if sub == "progress" && body["text"] == nil {
		return usage("--text is required", hint)
	}
	if sub == "transition" && body["to"] == nil {
		return usage("--to is required", hint)
	}
	if sub == "transition" {
		// 转阶段的体收 evidence（头同样有效）；update 只走头。
		if c.Evidence != "" {
			body["evidence"] = c.Evidence
		}
	}
	if sub == "relate" {
		if body["to"] == nil || body["type"] == nil {
			return usage("--to and --type are required", hint)
		}
		if *rm["remove"] {
			body["remove"] = true
		}
	}
	return send(c, f, method, "/items/"+url.PathEscape(id)+suffix, body)
}

func send(c *client.Ctx, f *form, method, path string, body map[string]any) error {
	var out map[string]any
	if err := c.DoHeaders(bg, f.headers(), method, path, body, &out); err != nil {
		return err
	}
	return c.Print(out)
}

func show(c *client.Ctx, args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usage("exactly one item id is required", "usage: apa item show <id>")
	}
	id := url.PathEscape(args[0])
	var out map[string]any
	if err := c.Do(bg, "GET", "/items/"+id, nil, &out); err != nil {
		return err
	}
	// 挂靠消息另有路由，过滤在内核（§14.6）。
	var msgs struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := c.Do(bg, "GET", "/items/"+id+"/messages", nil, &msgs); err != nil {
		return err
	}
	out["messages"] = msgs.Messages
	return c.Print(out)
}

func list(c *client.Ctx, args []string) error {
	f, _ := newForm("item list", []string{"phase", "kind", "project", "assignee"}, nil, nil)
	if _, err := f.parse(args, false, "usage: apa item list [--phase <p>] [--kind <k>] [--project <id>] [--assignee <executor>]"); err != nil {
		return err
	}
	q := url.Values{}
	for n, v := range f.vals {
		if *v != "" {
			q.Set(n, *v)
		}
	}
	var out map[string]any
	if err := c.Do(bg, "GET", "/items?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	return c.Print(out)
}

// today：GET /today 原样输出四组（planned、due、pending、stale）。
func today(c *client.Ctx, args []string) error {
	if len(args) != 0 {
		return usage("today takes no arguments", "usage: apa today")
	}
	var out map[string]any
	if err := c.Do(bg, "GET", "/today", nil, &out); err != nil {
		return err
	}
	return c.Print(out)
}

type inboxItem struct {
	ID  string `json:"id"`
	Ask *struct {
		ScreeningPending bool `json:"screening_pending"`
	} `json:"ask"`
}

// inbox：待决 = needs_input，建议 = proposed。web_only 初筛未返回的决策请求不列出（附录 B.1）。
func inbox(c *client.Ctx, args []string) error {
	if len(args) != 0 {
		return usage("inbox takes no arguments", "usage: apa inbox")
	}
	res := map[string][]json.RawMessage{}
	for key, phase := range map[string]string{"pending": "needs_input", "suggestions": "proposed"} {
		var l struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := c.Do(bg, "GET", "/items?phase="+phase, nil, &l); err != nil {
			return err
		}
		res[key] = []json.RawMessage{}
		for _, raw := range l.Items {
			var it inboxItem
			if json.Unmarshal(raw, &it) == nil && it.Ask != nil && it.Ask.ScreeningPending {
				continue
			}
			res[key] = append(res[key], raw)
		}
	}
	return c.Print(res)
}
