// Package items 是 apa 的事项命令组（附录 B.1「事项」「上下文」）：
// item show/list/add/update/comment/thread/transition/relate、propose、inbox。
//
// 这里只做参数到 HTTP 请求的搬运：校验、授权、版本、幂等、到期时钟全部由内核判定，
// 内核的错误体与退出码原样透出；不直接访问存储。
package items

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() {
	client.Register("item", itemHelp, item)
	client.Register("today", todayUsage, today)
	client.Register("propose", addUsage("propose"), propose)
	client.Register("inbox", inboxUsage, inbox)
}

var bg = context.Background()

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

const itemUsage = "usage: apa item show <id> [--part body|progress|messages --offset --full] | list [--phase --kind --project --assignee --tag --q --sort --planned-for --from --to --origin-source --origin-ref --limit --cursor --full] | add | update <id> | comment <id> --text <t> [--reply-to <comment id>] [--mention <executor id or session:main>]... [--model --thinking --override <k=v>...] [--on <YYYY-MM-DD>] | thread <id> [--before <n> --limit <n> --full --format json|tree] | transition <id> --to <phase> --reason <r> | relate <from> (--to <item or material id> --type <t> | --key <k> [--domain <d>])"

const (
	todayUsage = "usage: apa today [--full]"
	inboxUsage = "usage: apa inbox [--full]"
)

// 各写子命令的完整用法：参数错误时放进 next，-h / --help 时整段输出。
const (
	updateUsage  = "usage: apa item update <id> [--title --summary --body --status-label --priority --project --tags --assignee --waiting-on --ask --spec --planned-for <YYYY-MM-DD, empty clears it and the slot> --planned-slot <HH:MM - HH:MM, empty clears> --daily-at <HH:MM, empty clears; a planned date clears it> --deadline <RFC 3339|YYYY-MM-DD, empty clears> --origin <JSON> --ext <JSON>] --reason <r> [--version <n>]"
	commentUsage = "usage: apa item comment <id> --text <text> [--reply-to <comment id on the same item, from apa item thread>] " +
		"[--mention <executor id, or session:main for the resident main session>]... (at most 3; each mentioned executor gets a child item and APA starts it, its report comes back as a reply; " +
		"anyone who cannot be started fails the whole comment; no --mention wakes no one) " +
		"[--model <m>] [--thinking <t>] [--override <key=value>]... (only with exactly one executor mentioned; values from apa executors) [--on <YYYY-MM-DD, not later than today>]"
	threadUsage = "usage: apa item thread <id> [--before <before from the previous page>] [--limit <1-100, default 20>] [--full] [--format json|tree]; " +
		"newest last, the page stays within the read budget; without --full each entry omits its raw events (steps); " +
		"comments carry reply_to and mentions (who was mentioned and how far each got); --format tree prints the replies nested under what they answer"
	transitionUsage = "usage: apa item transition <id> --to <proposed|open|waiting|needs_input|done|dropped> --reason <r> [--waiting-on <kind>:<ref> --evidence <note or result evidence> --version <n>]"
	relateUsage     = "usage: apa item relate <from> --to <item or material id> --type <parent|blocks|derived_from|supersedes|relates|basis|output|minutes> [--remove] [--reason <r> --version <n>]\n   or: apa item relate <from> --key <correlation key> [--domain <dedupe domain>] [--remove]"
)

// itemHelp 是 `apa item … --help` 的输出：总览加各写子命令的全部参数。
var itemHelp = strings.Join([]string{itemUsage, addUsage("item add"), updateUsage, commentUsage, threadUsage, transitionUsage, relateUsage}, "\n")

func addUsage(name string) string {
	u := "usage: apa " + name + " --kind <kind> --title <title> [--summary --body --priority --project --tags a,b --assignee --planned-for <YYYY-MM-DD> --planned-slot <HH:MM - HH:MM> --daily-at <HH:MM, no date> --deadline <RFC 3339|YYYY-MM-DD> --origin <JSON> --ext <JSON> --reason]"
	if name == "item add" {
		u += " [--phase <phase>]"
	}
	return u + " [--evidence <note or result evidence>]"
}

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
	case "comment":
		return comment(c, args)
	case "thread":
		return thread(c, args)
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
		case fl.Name == "deadline":
			t, err := parseDeadline(*v)
			if err != nil {
				bad = usage("--deadline must be an RFC 3339 time or YYYY-MM-DD", hint)
				return
			}
			m[key] = t
		case fl.Name == "ask" || fl.Name == "spec" || fl.Name == "origin" || fl.Name == "ext":
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

// parseDeadline：RFC 3339 原样给内核；只给日期时取本机时区当天 23:59:59（§6.8）。
func parseDeadline(v string) (string, error) {
	if _, err := time.Parse(time.RFC3339, v); err == nil {
		return v, nil
	}
	d, err := time.ParseInLocation("2006-01-02", v, time.Local)
	if err != nil {
		return "", err
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.Local).Format(time.RFC3339), nil
}

// headers：--version → If-Match，--idempotency-key → Idempotency-Key。
func (f *form) headers() map[string]string {
	h := map[string]string{}
	for flagName, header := range map[string]string{"version": "If-Match", "idempotency-key": "Idempotency-Key"} {
		if p, ok := f.hdr[flagName]; ok && *p != "" {
			h[header] = *p
		}
	}
	return h
}

var itemFields = []string{"kind", "title", "summary", "body", "status-label", "priority", "project", "tags", "assignee", "waiting-on", "ask", "spec", "planned-for", "planned-slot", "daily-at", "deadline", "origin", "ext", "reason"}

// add 实现 item add 与 propose：POST /items。propose 不带 --phase，由内核缺省为 proposed。
func add(c *client.Ctx, args []string, name string) error {
	hint := addUsage(name)
	fields := itemFields
	if name == "item add" {
		fields = append([]string{"phase"}, fields...)
	}
	f, _ := newForm(name, fields, []string{"idempotency-key"}, nil)
	if _, err := f.parse(args, false, hint); err != nil {
		return err
	}
	body, err := f.body(hint)
	if err != nil {
		return err
	}
	withNote(c, body)
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
		hint = updateUsage
	case "transition":
		fields = []string{"to", "reason", "waiting-on"}
		hint = transitionUsage
	case "relate":
		fields, bools = []string{"to", "type", "reason", "key", "domain"}, []string{"remove"}
		hint = relateUsage
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
	if sub == "transition" {
		if body["to"] == nil {
			return usage("--to is required", hint)
		}
		withNote(c, body)
	}
	if sub == "relate" {
		if body["key"] != nil { // 登记或移除关联键：POST / DELETE …/correlation-keys
			if body["to"] != nil || body["type"] != nil {
				return usage("--key cannot be combined with --to or --type", hint)
			}
			method, suffix = "POST", "/correlation-keys"
			if *rm["remove"] {
				method = "DELETE"
			}
		} else {
			if body["to"] == nil || body["type"] == nil {
				return usage("--to and --type are required (or --key)", hint)
			}
			if body["domain"] != nil {
				return usage("--domain only applies to --key", hint)
			}
			if *rm["remove"] {
				body["remove"] = true
			}
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

// 单条读取与列表的默认上限（设计 §17.2）：只在命令行输出里截，HTTP 响应不变，不回写账本。
const (
	bodyHead     = 2000 // item show 默认给的正文字数
	recentN      = 5    // item show 默认给的最近进展与挂靠消息条数
	listDefault  = 30   // item list 默认条数
	listMax      = 200  // item list 最多条数
	summaryChars = 60   // item list 每行摘要的字数
)

const showUsage = "usage: apa item show <id> [--part body|progress|messages] [--offset <n>] [--full]; " +
	"by default the body is cut to its first 2000 characters and only the latest 5 progress notes and messages are shown"

func show(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("an item id is required first", showUsage)
	}
	id, rest := args[0], args[1:]
	fs := flag.NewFlagSet("item show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	part, full, offset := fs.String("part", "", ""), fs.Bool("full", false, ""), fs.Int("offset", 0, "")
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *offset < 0 || *full && *part != "" ||
		!slices.Contains([]string{"", "body", "progress", "messages"}, *part) {
		return usage("invalid arguments for item show", showUsage)
	}
	path := "/items/" + url.PathEscape(id)
	var out map[string]any
	if err := c.Do(bg, "GET", path, nil, &out); err != nil {
		return err
	}
	// 挂靠消息另有路由，过滤在内核（§14.6）。
	var msgs struct {
		Messages []any `json:"messages"`
	}
	if err := c.Do(bg, "GET", path+"/messages", nil, &msgs); err != nil {
		return err
	}
	if msgs.Messages == nil {
		msgs.Messages = []any{}
	}
	out["messages"] = msgs.Messages
	if *full {
		return c.Print(out)
	}
	it, _ := out["item"].(map[string]any)
	body, _ := it["body"].(string)
	progress, _ := out["progress"].([]any)
	self := "apa item show " + id
	switch *part {
	case "body":
		rs := []rune(body)
		from := min(*offset, len(rs))
		var seg map[string]any
		// 一段取多少：从上限往下减，直到连同转义与续读命令整份不超过一次读取的上限。
		for to := min(len(rs), from+kcontext.InjectBudget); ; to -= max(1, (to-from)/10) {
			seg = map[string]any{"id": id, "body": string(rs[from:to]), "chars": len(rs), "offset": from, "next": []string{}}
			if to < len(rs) {
				seg["next"] = []string{self + " --part body --offset " + strconv.Itoa(to)}
			}
			if to <= from+1 || client.Size(seg) <= kcontext.InjectBudget {
				break
			}
		}
		return c.Print(seg)
	case "progress", "messages":
		list := progress
		if *part == "messages" {
			list = msgs.Messages
		}
		return c.Print(older(self, *part, list, *offset))
	}
	// 默认：正文前 bodyHead 字、最近 recentN 条进展与消息（消息不带正文预览），写明各部分的总量与给了多少。
	parts := map[string]any{}
	if n := utf8.RuneCountInString(body); n > bodyHead {
		it["body"] = string([]rune(body)[:bodyHead])
		parts["body"] = map[string]int{"chars": n, "shown": bodyHead}
	}
	if len(progress) > recentN {
		out["progress"] = progress[len(progress)-recentN:]
		parts["progress"] = map[string]int{"count": len(progress), "shown": recentN}
	}
	ms := msgs.Messages
	if len(ms) > recentN {
		ms = ms[len(ms)-recentN:]
		parts["messages"] = map[string]int{"count": len(msgs.Messages), "shown": recentN}
	}
	for _, m := range ms {
		if mm, ok := m.(map[string]any); ok {
			delete(mm, "detail")
			delete(mm, "content")
		}
	}
	out["messages"] = ms
	if len(parts) > 0 {
		out["parts"] = parts
	}
	var next []string
	if parts["body"] != nil {
		next = append(next, self+" --part body --offset "+strconv.Itoa(bodyHead))
	}
	if parts["progress"] != nil {
		next = append(next, self+" --part progress --offset "+strconv.Itoa(recentN))
	}
	if len(msgs.Messages) > 0 {
		next = append(next, self+" --part messages")
	}
	out["next"] = append(next, "apa why "+id)[:min(3, len(next)+1)]
	return c.Print(out)
}

// listFlag 是可重复给出的字符串参数。
type listFlag []string

func (l *listFlag) String() string     { return "" }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// pairFlag 是可重复给出的 key=value 参数。
type pairFlag map[string]string

func (p pairFlag) String() string { return "" }
func (p pairFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return usage("--override takes key=value", "e.g. --override model=m1")
	}
	p[k] = val
	return nil
}

// comment：POST /items/{id}/comments。带 --mention 时内核写发言并叫起被 @ 的执行者或主会话（delegate.mention，§6.3）。
func comment(c *client.Ctx, args []string) error {
	f, _ := newForm("item comment", []string{"text", "on", "reply-to", "model", "thinking"}, []string{"version", "idempotency-key"}, nil)
	var mentions listFlag
	overrides := pairFlag{}
	f.fs.Var(&mentions, "mention", "")
	f.fs.Var(overrides, "override", "")
	id, err := f.parse(args, true, commentUsage)
	if err != nil {
		return err
	}
	body, err := f.body(commentUsage)
	if err != nil {
		return err
	}
	if body["text"] == nil {
		return usage("--text is required", commentUsage)
	}
	for _, k := range []string{"model", "thinking"} {
		if _, ok := body[k]; ok {
			overrides[k] = *f.vals[k]
			delete(body, k)
		}
	}
	if len(overrides) > 0 {
		if len(mentions) != 1 || mentions[0] == domain.MentionMain {
			return usage("--model, --thinking and --override only apply with exactly one executor mentioned", commentUsage)
		}
		body["overrides"] = map[string]string(overrides)
	}
	if len(mentions) > 0 {
		body["mentions"] = []string(mentions)
	}
	return send(c, f, "POST", "/items/"+url.PathEscape(id)+"/comments", body)
}

// thread：GET /items/{id}/thread，讨论流的一页；页大小与字数上限由内核守住。
func thread(c *client.Ctx, args []string) error {
	f, bools := newForm("item thread", []string{"before", "limit", "format"}, nil, []string{"full"})
	id, err := f.parse(args, true, threadUsage)
	if err != nil {
		return err
	}
	format := *f.vals["format"]
	if format != "" && format != "json" && format != "tree" {
		return usage("--format is json or tree", threadUsage)
	}
	q := url.Values{}
	for n, v := range f.vals {
		if *v != "" && n != "format" {
			q.Set(n, *v)
		}
	}
	if *bools["full"] {
		q.Set("full", "1")
	}
	var out threadPage
	if err := c.Do(bg, "GET", "/items/"+url.PathEscape(id)+"/thread?"+q.Encode(), nil, &out.raw); err != nil {
		return err
	}
	if format != "tree" {
		return c.Print(out.raw)
	}
	b, _ := json.Marshal(out.raw)
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	_, err = io.WriteString(c.Env.Stdout, out.tree(id))
	return err
}

// threadPage 是讨论流一页里画回复树要用的字段（附录 B.2 GET /items/{id}/thread）。
type threadPage struct {
	raw     map[string]any
	Entries []struct {
		At   time.Time `json:"at"`
		Who  struct{ Kind, Name string }
		Kind string `json:"kind"`
		Args struct {
			ID       string `json:"id"`
			Text     string `json:"text"`
			ReplyTo  string `json:"reply_to"`
			Mentions []struct {
				Target, State string
			} `json:"mentions"`
		} `json:"args"`
	} `json:"entries"`
	More   int   `json:"more"`
	Before int64 `json:"before"`
}

// tree 把一页画成文字：发言按回复关系缩进在被回复的那条下面，其余经过各占一行；@ 的状态写在发言末尾。
func (p threadPage) tree(item string) string {
	kids := map[string][]int{}
	here := map[string]bool{}
	for _, e := range p.Entries {
		if e.Kind == "progress" {
			here[e.Args.ID] = true
		}
	}
	for i, e := range p.Entries {
		if e.Kind == "progress" && here[e.Args.ReplyTo] {
			kids[e.Args.ReplyTo] = append(kids[e.Args.ReplyTo], i)
		}
	}
	var b strings.Builder
	if p.More > 0 {
		fmt.Fprintf(&b, "(%d earlier; apa item thread %s --before %d --format tree)\n", p.More, item, p.Before)
	}
	var line func(i, depth int)
	line = func(i, depth int) {
		e := p.Entries[i]
		who := e.Who.Name
		if who == "" {
			who = e.Who.Kind
		}
		b.WriteString(strings.Repeat("  ", depth))
		if depth > 0 {
			b.WriteString("└ ")
		}
		at := e.At.Local().Format("01-02 15:04")
		if e.Kind != "progress" {
			fmt.Fprintf(&b, "· %s %s %s\n", at, who, e.Kind)
			return
		}
		text, _, _ := strings.Cut(e.Args.Text, "\n")
		if r := []rune(text); len(r) > 120 {
			text = string(r[:120]) + "…"
		}
		fmt.Fprintf(&b, "[%s] %s %s: %s", e.Args.ID, at, who, text)
		for _, m := range e.Args.Mentions {
			fmt.Fprintf(&b, "  @%s(%s)", m.Target, m.State)
		}
		if e.Args.ReplyTo != "" && depth == 0 {
			fmt.Fprintf(&b, "  (reply to %s)", e.Args.ReplyTo)
		}
		b.WriteString("\n")
		for _, k := range kids[e.Args.ID] {
			line(k, depth+1)
		}
	}
	for i, e := range p.Entries {
		if e.Kind == "progress" && here[e.Args.ReplyTo] {
			continue // 画在被回复的那条下面
		}
		line(i, 0)
	}
	return b.String()
}

// older 是进展或挂靠消息的一页：从最近的往前数，跳过 offset 条，按先后排列；连同续读命令整份不超过一次读取的上限。
func older(self, part string, list []any, offset int) map[string]any {
	end := max(0, len(list)-offset)
	rows := list[max(0, end-listMax):end]
	var out map[string]any
	for {
		out = map[string]any{part: rows, "count": len(list), "offset": offset, "next": []string{}}
		if shown := offset + len(rows); shown < len(list) {
			out["next"] = []string{self + " --part " + part + " --offset " + strconv.Itoa(shown)}
		}
		if len(rows) <= 1 || client.Size(out) <= kcontext.InjectBudget {
			return out
		}
		rows = rows[1:] // 从最早的一端删，留下最近的
	}
}

const listUsage = "usage: apa item list [--phase <p>] [--kind <k>] [--project <id>] [--assignee <executor>] [--tag <t>] [--q <text>] " +
	"[--sort updated|deadline|planned_for|priority] [--planned-for <YYYY-MM-DD>] [--from <YYYY-MM-DD> --to <YYYY-MM-DD>, at most 62 days] " +
	"[--origin-source <source, e.g. import:notes>] [--origin-ref <external id>] [--limit <1-200, default 30>] [--cursor <c>] [--full]; " +
	"rows are short (no body, summary cut to 60 characters): read one with apa item show <id>, page with the cursor in next"

func list(c *client.Ctx, args []string) error {
	f, bools := newForm("item list", []string{"phase", "kind", "project", "assignee", "tag", "q", "sort", "planned-for", "from", "to", "origin-source", "origin-ref", "limit", "cursor"}, nil, []string{"full"})
	if _, err := f.parse(args, false, listUsage); err != nil {
		return err
	}
	q := url.Values{}
	for n, v := range f.vals {
		if *v != "" {
			q.Set(strings.ReplaceAll(n, "-", "_"), *v)
		}
	}
	if !q.Has("limit") {
		q.Set("limit", strconv.Itoa(listDefault))
	}
	if n, err := strconv.Atoi(q.Get("limit")); err != nil || n < 1 || n > listMax {
		return usage("--limit must be 1 to 200", listUsage)
	}
	var out map[string]any
	if err := c.Do(bg, "GET", "/items?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	full := *bools["full"]
	next := func() {
		var cmds []string
		if cur, _ := out["next_cursor"].(string); cur != "" {
			nq := maps.Clone(q)
			nq.Set("cursor", cur)
			cmd := []string{"apa item list"}
			for _, n := range slices.Sorted(maps.Keys(nq)) {
				cmd = append(cmd, "--"+strings.ReplaceAll(n, "_", "-"), client.Quote(nq.Get(n)))
			}
			if full {
				cmd = append(cmd, "--full")
			}
			cmds = append(cmds, strings.Join(cmd, " "))
		}
		if rows, _ := out["items"].([]any); len(rows) > 0 {
			if m, ok := rows[0].(map[string]any); ok {
				cmds = append(cmds, "apa item show "+fmt.Sprint(m["id"]))
			}
		}
		out["next"] = append(cmds, "apa search <words>")
	}
	if full { // --full 是本人或脚本显式要完整的行：只按条数分页
		next()
		return c.Print(out)
	}
	rows, _ := out["items"].([]any)
	for _, r := range rows {
		slim(r)
	}
	// 建议的来源与分组是给建议页用的，条数一多就比事项本身还大。
	delete(out, "origins")
	delete(out, "groups")
	offset, _ := strconv.Atoi(q.Get("cursor"))
	server, _ := out["next_cursor"].(string)
	client.Fit(out, "items", kcontext.InjectBudget, func(kept int) {
		if out["next_cursor"] = server; kept < len(rows) {
			out["next_cursor"] = strconv.Itoa(offset + kept)
		}
		if out["next_cursor"] == "" {
			delete(out, "next_cursor")
		}
		next()
	})
	return c.Print(out)
}

// slimKeys 是列表默认每行留下的字段：判断与引用够用，全文用 item show 或 --full。
// version、chain_depth、created_at 是 v0.1 列表就有的键，留着不破坏旧脚本（附录 B.6）。
var slimKeys = []string{"id", "title", "phase", "kind", "project", "priority", "tags", "assignee", "status_label",
	"deadline", "planned_for", "planned_slot", "daily_at", "version", "chain_depth", "created_at", "updated_at", "summary"}

func slim(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k := range m {
		if !slices.Contains(slimKeys, k) {
			delete(m, k)
		}
	}
	if s, _ := m["summary"].(string); utf8.RuneCountInString(s) > summaryChars {
		m["summary"] = string([]rune(s)[:summaryChars]) + "…"
	}
}

// compact 从列表里的事项去掉大字段（设计 §17）：today 与 inbox 只给判断与引用够用的字段，全文用 item show 或 --full。
func compact(list any) {
	items, _ := list.([]any)
	for _, v := range items {
		if m, ok := v.(map[string]any); ok {
			for _, k := range []string{"body", "spec", "ext", "proposal"} {
				delete(m, k)
			}
		}
	}
}

// fullFlag 解析只接受 --full 的读命令。
func fullFlag(name, hint string, args []string) (bool, error) {
	f, bools := newForm(name, nil, nil, []string{"full"})
	if _, err := f.parse(args, false, hint); err != nil {
		return false, err
	}
	return *bools["full"], nil
}

// today：GET /today 的各组事项，默认精简。
func today(c *client.Ctx, args []string) error {
	full, err := fullFlag("today", todayUsage, args)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := c.Do(bg, "GET", "/today", nil, &out); err != nil {
		return err
	}
	if !full {
		for _, group := range out {
			compact(group)
		}
	}
	return c.Print(out)
}

// inbox：待决 = needs_input，建议 = proposed。
func inbox(c *client.Ctx, args []string) error {
	full, err := fullFlag("inbox", inboxUsage, args)
	if err != nil {
		return err
	}
	res := map[string][]any{}
	for key, phase := range map[string]string{"pending": "needs_input", "suggestions": "proposed"} {
		var l struct {
			Items []any `json:"items"`
		}
		if err := c.Do(bg, "GET", "/items?phase="+phase, nil, &l); err != nil {
			return err
		}
		if !full {
			compact(l.Items)
		}
		res[key] = append([]any{}, l.Items...)
	}
	return c.Print(res)
}

// withNote 把 --evidence 作为说明或结果证据放进请求体（委派进入 done 必须带证据）；它不参与权限判定。
func withNote(c *client.Ctx, body map[string]any) {
	if c.Note != "" {
		body["evidence"] = c.Note
	}
}
