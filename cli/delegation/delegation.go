// Package delegation 是 apa 的委派命令组（附录 B.1、设计 §7.5、§14.7）：executors、delegate、report、declare。
//
// 这里只把参数搬成 GET /v1/executors、POST /v1/delegations、POST /v1/items/{id}/report、
// POST /v1/declarations 的请求；项目允许的执行者（21）、归属与运行身份（4）、不存在（5）、
// 终态再回执（6）、声明须本人在 Web 确认都由内核判定，错误体与退出码原样透出。不直接访问存储。
package delegation

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() {
	client.Register("executors", executorsUsage, executors)
	client.Register("delegate", delegateUsage, delegate)
	client.Register("report", reportUsage, report)
	client.Register("declare", declareUsage, declare)
	client.Register("dispatch", dispatchUsage, dispatch)
}

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func send(c *client.Ctx, hdr map[string]string, method, path string, body any) error {
	var out any
	if err := c.DoHeaders(context.Background(), hdr, method, path, body, &out); err != nil {
		return err
	}
	return c.Print(out)
}

func idem(key string) map[string]string {
	if key == "" {
		return nil
	}
	return map[string]string{"Idempotency-Key": key}
}

const (
	executorsUsage = "usage: apa executors [--project <id>] [--runner task|main_session|source_batch [--rule <rule>] [--schedule <executor>] [--item <executor>] [--delegated]]" +
		" | set <executor> [--model <m>] [--thinking <t>] [--override <key=value>]... | use <executor> --as daily|main|source | use <executor> --rule <rule>"
	useUsage = "usage: apa executors use <executor> --as daily|main|source | apa executors use <executor> --rule <rule key>; " +
		"an empty executor (\"\") clears the choice; daily is for background runs without a chosen executor, main for runs as the owner's own Agent, source for source processing"
	setUsage = "usage: apa executors set <executor> [--model <m>] [--thinking <t>] [--override <key=value>]...; " +
		"changes only the keys given, an empty value clears one (the declaration's own default applies); values must be ones apa executors lists under overridable"
)

func executors(c *client.Ctx, args []string) error {
	if len(args) > 0 && args[0] == "set" {
		return setExecutor(c, args[1:])
	}
	if len(args) > 0 && args[0] == "use" {
		return useExecutor(c, args[1:])
	}
	fs := flags("executors")
	q := url.Values{}
	for _, n := range []string{"project", "runner", "rule", "schedule", "item"} {
		fs.Func(n, "", func(v string) error { q.Set(n, v); return nil })
	}
	delegated := fs.Bool("delegated", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for executors", executorsUsage)
	}
	if *delegated {
		q.Set("delegated", "true")
	}
	if q.Get("runner") == "" && (q.Has("rule") || q.Has("schedule") || q.Has("item") || *delegated) {
		return usage("--rule, --schedule, --item and --delegated preview a selection; give --runner too", executorsUsage)
	}
	path := "/executors"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return send(c, nil, "GET", path, nil)
}

// useExecutor 选执行者（§13.6）：分工的日常、主执行者改 PUT /settings/agents，来源处理改 PUT /settings/source-pipeline，
// 规则固定的执行者改 rule_executor。先读现值再整段写回（带读到的策略版本，期间别处改过则冲突）；能不能改由内核按权限卡判定。
func useExecutor(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("an executor id is required first", useUsage)
	}
	id, args := args[0], args[1:]
	fs := flags("executors use")
	as := fs.String("as", "", "")
	rule := fs.String("rule", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || (*as == "") == (*rule == "") {
		return usage("give exactly one of --as and --rule", useUsage)
	}
	ctx := context.Background()
	route := "/settings/agents"
	if *as == "source" {
		route = "/settings/source-pipeline"
	}
	var cur map[string]json.RawMessage
	if err := c.Do(ctx, "GET", route, nil, &cur); err != nil {
		return err
	}
	body := map[string]any{"version": cur["version"]}
	switch {
	case *rule != "":
		m := map[string]string{}
		_ = json.Unmarshal(cur["rule_executor"], &m)
		if id == "" {
			delete(m, *rule)
		} else {
			m[*rule] = id
		}
		body["rule_executor"] = m
	case *as == "daily":
		body["default_executor"] = id
	case *as == "main":
		body["main_executor"] = id
	case *as == "source":
		sp := map[string]any{}
		_ = json.Unmarshal(cur["source_pipeline"], &sp)
		sp["executor"] = id
		body["source_pipeline"] = sp
	default:
		return usage("--as takes daily, main or source", useUsage)
	}
	return send(c, nil, "PUT", route, body)
}

// setExecutor 改执行者的可覆盖参数默认值（PUT /executors/{id}/overrides 整份替换）：先读现值，只改命令行给出的键。
// 取值是否在声明列出的范围内由内核判定。
func setExecutor(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("an executor id is required first", setUsage)
	}
	id, args := args[0], args[1:]
	fs := flags("executors set")
	fs.String("model", "", "")
	fs.String("thinking", "", "")
	set := pairFlag{}
	fs.Var(set, "override", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for executors set", setUsage)
	}
	fs.Visit(func(f *flag.Flag) { // --model 与 --thinking 是同名键的简写；出现过才算，空值也算（清除）
		if f.Name == "model" || f.Name == "thinking" {
			set[f.Name] = f.Value.String()
		}
	})
	if len(set) == 0 {
		return usage("nothing to set", setUsage)
	}
	var list struct {
		Executors []struct {
			ID        string            `json:"id"`
			Overrides map[string]string `json:"overrides"`
		} `json:"executors"`
	}
	if err := c.Do(context.Background(), "GET", "/executors", nil, &list); err != nil {
		return err
	}
	values := map[string]string{}
	for _, e := range list.Executors {
		if e.ID == id {
			for k, v := range e.Overrides {
				values[k] = v
			}
		}
	}
	for k, v := range set {
		values[k] = v
	}
	return send(c, nil, "PUT", "/executors/"+url.PathEscape(id)+"/overrides", map[string]any{"values": values})
}

const delegateUsage = "usage: apa delegate --parent <id> --assignee <executor> [--executor <json file|->] [--title --summary --body --priority --project --reason --idempotency-key] " +
	"[--start [--model <m>] [--thinking <t>] [--override <key=value>]...]   (--start: APA starts the executor itself; values must be ones apa executors lists)"

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

func delegate(c *client.Ctx, args []string) error {
	fs := flags("delegate")
	v := map[string]*string{}
	for _, n := range []string{"parent", "assignee", "title", "summary", "body", "priority", "project", "reason"} {
		v[n] = fs.String(n, "", "")
	}
	key := fs.String("idempotency-key", "", "")
	exec := fs.String("executor", "", "")
	start := fs.Bool("start", false, "")
	model, thinking := fs.String("model", "", ""), fs.String("thinking", "", "")
	overrides := pairFlag{}
	fs.Var(overrides, "override", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for delegate", delegateUsage)
	}
	if *v["parent"] == "" || *v["assignee"] == "" {
		return usage("--parent and --assignee are required", delegateUsage)
	}
	body := map[string]any{}
	for n, s := range v {
		if *s != "" {
			body[n] = *s
		}
	}
	// --model 与 --thinking 是最常用的两项可覆盖参数的简写，内核只认声明里的键与取值（附录 A.2）。
	for k, val := range map[string]string{"model": *model, "thinking": *thinking} {
		if val != "" {
			overrides[k] = val
		}
	}
	if *start {
		body["start"] = true
	}
	if len(overrides) > 0 {
		body["overrides"] = map[string]string(overrides)
	}
	if *exec != "" { // 随委派登记执行者（§6.17）：{kind, host?, label?, handle?}，harness 取 APA_HARNESS
		data, err := readFile(c, *exec)
		if err != nil || !json.Valid(data) {
			return usage("--executor must be a JSON file or - with valid JSON", delegateUsage)
		}
		body["executor"] = json.RawMessage(data)
	}
	return send(c, idem(*key), "POST", "/delegations", body)
}

type linkFlag []string

func (l *linkFlag) String() string     { return strings.Join(*l, ",") }
func (l *linkFlag) Set(v string) error { *l = append(*l, v); return nil }

const reportUsage = "usage: apa report (<id> | --session <dispatch session handle>) --outcome <progress|done|blocked> --summary <text> [--pr --commit --branch --link --file <report file>]... [--ack-cursor <c> --reason <t> --version <n> --idempotency-key <k>]; " +
	"done on a delegated item needs no PR: the summary (required) is the result, evidence is attached when there is one"

func report(c *client.Ctx, args []string) error {
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	fs := flags("report")
	session := fs.String("session", "", "")
	outcome, summary := fs.String("outcome", "", ""), fs.String("summary", "", "")
	cursor, reason, version, key := fs.String("ack-cursor", "", ""), fs.String("reason", "", ""), fs.String("version", "", ""), fs.String("idempotency-key", "", "")
	ev := map[string]*string{}
	for _, n := range []string{"pr", "commit", "branch", "file"} {
		ev[n] = fs.String(n, "", "")
	}
	var links linkFlag // --link 可重复：库内链接在报 done 时由内核登记为产出（主文档 §7.5）
	fs.Var(&links, "link", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for report", reportUsage)
	}
	if (id == "") == (*session == "") {
		return usage("give the item id first, or --session with the dispatch's session handle", reportUsage)
	}
	if *outcome == "" {
		return usage("--outcome is required", reportUsage)
	}
	evidence := map[string]string{}
	for n, s := range ev {
		if *s != "" {
			evidence[n] = *s
		}
	}
	evBody := map[string]any{}
	for n, v := range evidence {
		evBody[n] = v
	}
	switch len(links) { // 只给一个时保持原来的字符串形状
	case 0:
	case 1:
		evBody["link"] = links[0]
	default:
		evBody["link"] = []string(links)
	}
	body := map[string]any{"outcome": *outcome, "summary": *summary, "evidence": evBody}
	if *cursor != "" {
		body["cursor"] = *cursor
	}
	if *reason != "" {
		body["reason"] = *reason
	}
	hdr := idem(*key)
	if *version != "" {
		if hdr == nil {
			hdr = map[string]string{}
		}
		hdr["If-Match"] = *version
	}
	if *session != "" { // 按会话句柄：内核按调用方 harness 记下的委派记录找到子事项（§6.17）
		body["session"] = *session
		return send(c, hdr, "POST", "/reports", body)
	}
	return send(c, hdr, "POST", "/items/"+url.PathEscape(id)+"/report", body)
}

const dispatchUsage = "usage: apa dispatch add (<item> | --assignee <executor> [--executor <json file|->] [--about <item>]) [--session S] [--resumable] [--cwd D] [--repo R] [--handle <json file|->] [--continues <dispatch>] [--idempotency-key K] | " +
	"attach <dispatch> --parent <item> [--title --summary --body --priority --reason] | untrack <dispatch> | " +
	"update (<dispatch> | --session <handle>) [--state ended|lost] [--resumable true|false]   (--executor and --handle take a file, - for stdin, or inline JSON)"

// dispatch 写或推进委派记录（§6.17）：harness 取 APA_HARNESS；同会话去重、句柄校验与授权都在内核。
// add 不带事项 id 是还没挂到事项上的派活：之后每轮注入都提示，attach 挂上或 untrack 标为不跟踪。
func dispatch(c *client.Ctx, args []string) error {
	if len(args) < 1 || (args[0] != "add" && args[0] != "update" && (len(args) < 2 || strings.HasPrefix(args[1], "-"))) {
		return usage("apa dispatch needs add [<item>], attach <dispatch>, untrack <dispatch> or update <dispatch>", dispatchUsage)
	}
	sub, id, rest := args[0], "", args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	fs := flags("dispatch")
	switch sub {
	case "add":
		session, cwd, repo, handle, cont, key := fs.String("session", "", ""), fs.String("cwd", "", ""), fs.String("repo", "", ""),
			fs.String("handle", "", ""), fs.String("continues", "", ""), fs.String("idempotency-key", "", "")
		assignee, exec, about := fs.String("assignee", "", ""), fs.String("executor", "", ""), fs.String("about", "", "")
		resumable := fs.Bool("resumable", false, "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
			return usage("invalid arguments for dispatch add", dispatchUsage)
		}
		body := map[string]any{"session": *session, "resumable": *resumable, "cwd": *cwd, "repo": *repo, "continues": *cont}
		if *handle != "" {
			data, err := readFile(c, *handle)
			if err != nil || !json.Valid(data) {
				return usage("--handle must be a JSON file or - with valid JSON", dispatchUsage)
			}
			body["handle"] = json.RawMessage(data)
		}
		if id != "" {
			return send(c, idem(*key), "POST", "/items/"+url.PathEscape(id)+"/dispatches", body)
		}
		if *assignee == "" {
			return usage("give the delegated item, or --assignee for a dispatch not yet attached to an item", dispatchUsage)
		}
		body["assignee"] = *assignee
		if *about != "" {
			body["about"] = *about
		}
		if *exec != "" {
			data, err := readFile(c, *exec)
			if err != nil || !json.Valid(data) {
				return usage("--executor must be a JSON file or - with valid JSON", dispatchUsage)
			}
			body["executor"] = json.RawMessage(data)
		}
		return send(c, idem(*key), "POST", "/dispatches", body)
	case "attach":
		v := map[string]*string{}
		for _, n := range []string{"parent", "title", "summary", "body", "priority", "reason"} {
			v[n] = fs.String(n, "", "")
		}
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *v["parent"] == "" {
			return usage("dispatch attach needs --parent <item>", dispatchUsage)
		}
		body := map[string]any{}
		for n, s := range v {
			if *s != "" {
				body[n] = *s
			}
		}
		return send(c, nil, "POST", "/dispatches/"+url.PathEscape(id)+"/attach", body)
	case "untrack":
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
			return usage("invalid arguments for dispatch untrack", dispatchUsage)
		}
		return send(c, nil, "PATCH", "/dispatches/"+url.PathEscape(id), map[string]any{"state": "ended"})
	case "update":
		state, resumable, session := fs.String("state", "", ""), fs.String("resumable", "", ""), fs.String("session", "", "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || (id == "") == (*session == "") {
			return usage("dispatch update needs <dispatch> or --session <handle>", dispatchUsage)
		}
		body := map[string]any{}
		if *session != "" {
			body["session"] = *session
		}
		if *state != "" {
			body["state"] = *state
		}
		if *resumable != "" {
			b, err := strconv.ParseBool(*resumable)
			if err != nil {
				return usage("--resumable must be true or false", dispatchUsage)
			}
			body["resumable"] = b
		}
		if id == "" {
			return send(c, nil, "PATCH", "/dispatches", body)
		}
		return send(c, nil, "PATCH", "/dispatches/"+url.PathEscape(id), body)
	}
	return usage("unknown dispatch subcommand "+sub, dispatchUsage)
}

const declareUsage = "usage: apa declare <file|-> (a JSON declaration; takes effect only after the owner confirms it in Web)"

func declare(c *client.Ctx, args []string) error {
	if len(args) != 1 {
		return usage("exactly one declaration file is required", declareUsage)
	}
	data, err := readFile(c, args[0])
	if err != nil {
		return usage("cannot read declaration "+args[0], declareUsage)
	}
	if !json.Valid(data) {
		return usage("the declaration is not valid JSON", declareUsage)
	}
	return send(c, nil, "POST", "/declarations", json.RawMessage(data))
}

// readFile 读声明文件；"-" 读标准输入；以 { 开头的是行内 JSON（插件一次调用要同时给句柄与执行者登记时用）。
func readFile(c *client.Ctx, name string) ([]byte, error) {
	switch {
	case name == "-":
		return io.ReadAll(c.Env.Stdin)
	case strings.HasPrefix(strings.TrimSpace(name), "{"):
		return []byte(name), nil
	}
	return os.ReadFile(name)
}
