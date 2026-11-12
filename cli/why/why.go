// Package why 实现 `apa why <id>`（设计 §15.1、附录 B.1）：回放事项或运行的来龙去脉。
// 薄客户端：只调 GET /v1/why/{id}，内容按凭据裁剪由内核完成，这里不做任何裁剪或推断。
package why

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/cli/judgment"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kwhy "github.com/xiaohei-info/awesome-personal-agent/kernel/why"
)

func init() { client.Register("why", run) }

func run(c *client.Ctx, args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return domain.NewError(domain.CodeInvalidRequest, "apa why takes exactly one item or run id", "usage: apa why <id> [--pretty]")
	}
	var tl kwhy.Timeline
	if err := c.Do(context.Background(), "GET", "/why/"+url.PathEscape(args[0]), nil, &tl); err != nil {
		return err
	}
	if !c.Pretty {
		return c.Print(tl)
	}
	_, err := fmt.Fprint(c.Env.Stdout, render(tl))
	return err
}

// render 是 --pretty 的人读格式：每步一行信封，正文（原因、载荷、证据）缩进其后；被裁掉的内容只标注，不猜测。
func render(tl kwhy.Timeline) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", tl.ID)
	if tl.ScreeningPending {
		b.WriteString("  (web_only screening pending: details are only visible in Web)\n")
	}
	for _, s := range tl.Steps {
		fmt.Fprintf(&b, "  #%d %s  %s  %s/%s via %s", s.Seq, s.At, s.Type, s.Actor.Type, s.Actor.Principal, s.Actor.Via)
		if s.Actor.OnBehalfOf != "" {
			fmt.Fprintf(&b, " on behalf of %s", s.Actor.OnBehalfOf)
		}
		if s.Basis != nil {
			fmt.Fprintf(&b, "  policy v%d", s.Basis.PolicyVersion)
			if s.Basis.RuleID != "" {
				fmt.Fprintf(&b, " rule %s", s.Basis.RuleID)
			}
		}
		if s.ChainDepth > 0 {
			fmt.Fprintf(&b, "  depth %d", s.ChainDepth)
		}
		if s.ResolvedVia != "" {
			fmt.Fprintf(&b, "  resolved via %s", s.ResolvedVia)
		}
		if s.Grant != "" {
			fmt.Fprintf(&b, "  grant %s", s.Grant)
		}
		if s.Tombstone {
			b.WriteString("  [erased]")
		}
		if s.Redacted {
			b.WriteString("  [redacted]")
		}
		b.WriteByte('\n')
		if s.Reason != "" {
			fmt.Fprintf(&b, "      reason: %s\n", s.Reason)
		}
		if len(s.Payload) > 0 && string(s.Payload) != "null" {
			fmt.Fprintf(&b, "      payload: %s\n", s.Payload)
		}
		if s.Evidence != "" {
			fmt.Fprintf(&b, "      evidence: %s\n", s.Evidence)
		}
		if j := s.Judgment; j != nil {
			renderJudgment(&b, j)
		}
	}
	return b.String()
}

// renderJudgment 写"判断"一行：记录在就逐级列出；过了保留期只写命令 Event 里的结论。
func renderJudgment(b *strings.Builder, j *kwhy.Judgment) {
	if j.Record == nil {
		fmt.Fprintf(b, "      judgment %s: %s, decided by %s (record past retention, only the event's conclusion remains)\n", j.ID, j.Answer, j.DecidedBy)
		return
	}
	r := j.Record
	fmt.Fprintf(b, "      judgment %s: %s → %s, decided by %s\n", r.ID, r.Template, r.Answer, r.DecidedBy)
	for i, l := range r.Levels {
		fmt.Fprintf(b, "        %d. %s\n", i+1, judgment.LevelLine(l, r.Band))
	}
}
