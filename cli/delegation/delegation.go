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
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() {
	client.Register("executors", executors)
	client.Register("delegate", delegate)
	client.Register("report", report)
	client.Register("declare", declare)
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

const executorsUsage = "usage: apa executors [--project <id>]"

func executors(c *client.Ctx, args []string) error {
	fs := flags("executors")
	project := fs.String("project", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for executors", executorsUsage)
	}
	path := "/executors"
	if *project != "" {
		path += "?project=" + url.QueryEscape(*project)
	}
	return send(c, nil, "GET", path, nil)
}

const delegateUsage = "usage: apa delegate --parent <id> --assignee <executor> [--title --summary --body --priority --project --reason --idempotency-key]"

func delegate(c *client.Ctx, args []string) error {
	fs := flags("delegate")
	v := map[string]*string{}
	for _, n := range []string{"parent", "assignee", "title", "summary", "body", "priority", "project", "reason"} {
		v[n] = fs.String(n, "", "")
	}
	key := fs.String("idempotency-key", "", "")
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
	return send(c, idem(*key), "POST", "/delegations", body)
}

const reportUsage = "usage: apa report <id> --outcome <progress|done|blocked> --summary <text> [--pr --commit --branch --link] [--ack-cursor <c> --reason <t> --version <n> --idempotency-key <k>]"

func report(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("an item id is required first", reportUsage)
	}
	id := args[0]
	fs := flags("report")
	outcome, summary := fs.String("outcome", "", ""), fs.String("summary", "", "")
	cursor, reason, version, key := fs.String("ack-cursor", "", ""), fs.String("reason", "", ""), fs.String("version", "", ""), fs.String("idempotency-key", "", "")
	ev := map[string]*string{}
	for _, n := range []string{"pr", "commit", "branch", "link"} {
		ev[n] = fs.String(n, "", "")
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for report", reportUsage)
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
	body := map[string]any{"outcome": *outcome, "summary": *summary, "evidence": evidence}
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
	return send(c, hdr, "POST", "/items/"+url.PathEscape(id)+"/report", body)
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

// readFile 读声明文件；"-" 读标准输入。
func readFile(c *client.Ctx, name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(c.Env.Stdin)
	}
	return os.ReadFile(name)
}
