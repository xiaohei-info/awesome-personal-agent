// Package health 实现 `apa health`（设计 §15.2、附录 B.1）：只读，打印 GET /v1/health 的十类信号与修复入口。
// 薄客户端：判定在内核，这里不重算、不过滤；默认输出就是 API 响应本身，与 Web 读到的是同一份。
package health

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("health", run) }

type report struct {
	Healthy bool `json:"healthy"`
	Signals []struct {
		ID    string `json:"id"`
		OK    bool   `json:"ok"`
		Count int    `json:"count"`
		Why   string `json:"why"`
		Next  string `json:"next"`
	} `json:"signals"`
}

func run(c *client.Ctx, args []string) error {
	if len(args) != 0 {
		return domain.NewError(domain.CodeInvalidRequest, "apa health takes no arguments", "usage: apa health [--pretty]")
	}
	if !c.Pretty {
		var raw map[string]any // 原样透传，不经过窄结构，新增字段不会被吞掉
		if err := c.Do(context.Background(), "GET", "/health", nil, &raw); err != nil {
			return err
		}
		return c.Print(raw)
	}
	var rep report
	if err := c.Do(context.Background(), "GET", "/health", nil, &rep); err != nil {
		return err
	}
	_, err := fmt.Fprint(c.Env.Stdout, render(rep))
	return err
}

func render(rep report) string {
	var b strings.Builder
	if rep.Healthy {
		b.WriteString("healthy\n")
	} else {
		b.WriteString("unhealthy\n")
	}
	for _, s := range rep.Signals {
		if s.OK {
			fmt.Fprintf(&b, "  ok    %s\n", s.ID)
			continue
		}
		fmt.Fprintf(&b, "  FAIL  %s (%d)\n        why:  %s\n        next: %s\n", s.ID, s.Count, s.Why, s.Next)
	}
	return b.String()
}
