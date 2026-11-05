// Package runs 是 apa 的运行记录命令组（附录 B.1「可观测」，设计 §6.11）：run list、run show。
//
// 薄客户端：只调 GET /v1/runs 与 GET /v1/runs/{id}，筛选、授权与字段裁剪（不含命令规格）全由内核完成；
// 这里不做业务判定，也不直接访问存储。
package runs

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("run", run) }

const usageHint = "usage: apa run list [--state <s>] [--category <c>] [--target <id>] [--limit <n>] | show <id> [--pretty]"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no run subcommand given")
	}
	switch args[0] {
	case "list":
		return list(c, args[1:])
	case "show":
		return show(c, args[1:])
	}
	return usage("unknown run subcommand " + args[0])
}

func list(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("run list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vals := map[string]*string{}
	for _, n := range []string{"state", "category", "target", "limit"} {
		vals[n] = fs.String(n, "", "")
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments")
	}
	q := url.Values{}
	fs.Visit(func(f *flag.Flag) { q.Set(f.Name, *vals[f.Name]) })
	path := "/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out struct {
		Runs []domain.Run `json:"runs"`
	}
	if err := c.Do(context.Background(), "GET", path, nil, &out); err != nil {
		return err
	}
	if !c.Pretty {
		return c.Print(out)
	}
	if len(out.Runs) == 0 {
		_, err := fmt.Fprintln(c.Env.Stdout, "no runs")
		return err
	}
	var b strings.Builder
	for _, r := range out.Runs {
		b.WriteString(line(r))
		b.WriteByte('\n')
	}
	_, err := fmt.Fprint(c.Env.Stdout, b.String())
	return err
}

func show(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("a run id is required")
	}
	if len(args) != 1 {
		return usage("apa run show takes exactly one run id")
	}
	var r domain.Run
	if err := c.Do(context.Background(), "GET", "/runs/"+url.PathEscape(args[0]), nil, &r); err != nil {
		return err
	}
	if !c.Pretty {
		return c.Print(r)
	}
	_, err := fmt.Fprint(c.Env.Stdout, detail(r))
	return err
}

// line 是 list --pretty 的一行：id、类别（后台运行带 runner）、状态、目标。
func line(r domain.Run) string {
	cat := string(r.Category)
	if r.Runner != "" {
		cat += "/" + r.Runner
	}
	s := fmt.Sprintf("%s  %-9s %-10s %s", r.ID, r.State, cat, r.Target)
	if r.ExitCode != nil {
		s += fmt.Sprintf("  exit %d", *r.ExitCode)
	}
	return s
}

// detail 是 show --pretty：首行同 list，其后缩进列出执行者、授权依据和时间；缺失的字段不打印，不猜测。
func detail(r domain.Run) string {
	var b strings.Builder
	b.WriteString(line(r) + "\n")
	add := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "  %s: %s\n", k, v)
		}
	}
	add("harness", r.Harness)
	add("session", r.SessionID)
	add("access", r.Access)
	add("launch", r.Launch)
	if r.Basis != nil {
		basis := fmt.Sprintf("policy v%d", r.Basis.PolicyVersion)
		if r.Basis.RuleID != "" {
			basis += " rule " + r.Basis.RuleID
		}
		add("basis", basis)
	}
	if r.ChainDepth > 0 {
		add("depth", fmt.Sprint(r.ChainDepth))
	}
	add("timeout", map[bool]string{true: fmt.Sprintf("%ds", r.TimeoutSec)}[r.TimeoutSec > 0])
	if r.StartedAt != nil {
		add("started", r.StartedAt.Format(time.RFC3339))
	}
	if r.FinishedAt != nil {
		add("finished", r.FinishedAt.Format(time.RFC3339))
	}
	return b.String()
}
