// Package judgment 是 apa 的判断记录命令组（附录 B.1「可观测」，设计 §6.16、§8.6、§8.10）：judgment show、spot-check、stats。
//
// 薄客户端：只调 GET /v1/judgments/{id}、/v1/judgments/spot-check、/v1/judgments/stats，授权（运行凭据只读目标事项的记录，范围外按不存在，退出码 5）
// 与字段都由内核决定；这里不做业务判定，也不直接访问存储。
package judgment

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
)

func init() { client.Register("judgment", run) }

const usageHint = "usage: apa judgment show <id> [--pretty] | spot-check | stats [--use U] [--template T] [--source S] [--since 30d]"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

func run(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no judgment subcommand given")
	}
	switch args[0] {
	case "show":
		return show(c, args[1:])
	case "spot-check":
		return spotCheck(c, args[1:])
	case "stats":
		return stats(c, args[1:])
	}
	return usage("unknown judgment subcommand " + args[0])
}

// spotCheck 只读列出抽检池；抽检结论只在 Web 填（设计 §8.10），这里没有写子命令。
func spotCheck(c *client.Ctx, args []string) error {
	if len(args) != 0 {
		return usage("apa judgment spot-check takes no arguments; results are submitted in the Web UI only")
	}
	var out json.RawMessage
	if err := c.Do(context.Background(), "GET", "/judgments/spot-check", nil, &out); err != nil {
		return err
	}
	return c.Print(out)
}

// stats 只读：各级调用次数与四项指标，筛选与窗口都由内核解释（GET /judgments/stats）。
func stats(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("judgment stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	q := url.Values{}
	for _, name := range []string{"use", "template", "source", "since"} {
		fs.Func(name, "", func(v string) error { q.Set(name, v); return nil })
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("apa judgment stats takes only --use, --template, --source and --since")
	}
	path := "/judgments/stats"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out json.RawMessage
	if err := c.Do(context.Background(), "GET", path, nil, &out); err != nil {
		return err
	}
	return c.Print(out)
}

func show(c *client.Ctx, args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usage("apa judgment show takes exactly one judgment record id")
	}
	var rec record.Record
	if err := c.Do(context.Background(), "GET", "/judgments/"+url.PathEscape(args[0]), nil, &rec); err != nil {
		return err
	}
	if !c.Pretty {
		return c.Print(rec)
	}
	_, err := fmt.Fprint(c.Env.Stdout, Render(rec))
	return err
}

// Render 是 --pretty 的人读格式：头一行是记录与定案者，其后逐级一行（后端、结果、是否定案）。
func Render(r record.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  %s  band %.2f..%.2f  decided by %s → %s", r.ID, r.Use, r.Template, r.Band.Low, r.Band.High, r.DecidedBy, r.Answer)
	if r.LowConfidence {
		b.WriteString(" (low confidence)")
	}
	if r.Shadow {
		b.WriteString(" [shadow]")
	}
	b.WriteByte('\n')
	for i, l := range r.Levels {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, LevelLine(l, r.Band))
	}
	return b.String()
}

// LevelLine 描述一级：端点、结果，分类端点说明概率与不确定带的关系，是否由这一级定案。
func LevelLine(l record.Level, band record.Band) string {
	var s strings.Builder
	s.WriteString(l.Backend)
	if l.Model != "" {
		fmt.Fprintf(&s, " (%s)", l.Model)
	}
	switch {
	case l.PYes != nil:
		fmt.Fprintf(&s, " p=%.2f", *l.PYes)
		switch {
		case *l.PYes >= band.High:
			fmt.Fprintf(&s, " ≥ high %.2f → yes", band.High)
		case *l.PYes <= band.Low:
			fmt.Fprintf(&s, " ≤ low %.2f → no", band.Low)
		default:
			s.WriteString(" in the uncertain band")
		}
	case l.Answer != "":
		fmt.Fprintf(&s, " answer=%s", l.Answer)
		if l.Sufficient != nil && !*l.Sufficient {
			s.WriteString(" (not sufficient)")
		}
	}
	s.WriteString(", " + l.Outcome)
	if l.ErrorCode != "" {
		s.WriteString(" " + l.ErrorCode)
	}
	if l.Outcome == record.OutcomeDecided {
		s.WriteString(" — decided")
	}
	if l.Reused {
		s.WriteString(", reused")
	}
	fmt.Fprintf(&s, ", %dms", l.DurationMs)
	return s.String()
}
