// Package followmsg 是 apa 的跟进消息命令（附录 B.1、设计 §9.4）：handback。
//
// 只把参数搬成 POST /v1/messages/{id}/handback 的请求；归属（4）、理由必填（2）、同一消息只能交回一次（6）
// 都由内核判定，错误体与退出码原样透出。不直接访问存储。确认消息走 `apa report --ack-cursor`（cli/delegation）。
package followmsg

import (
	"context"
	"flag"
	"io"
	"net/url"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("handback", handback) }

const handbackUsage = "usage: apa handback <message> --reason <text> [--idempotency-key <k>]"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, handbackUsage) }

func handback(c *client.Ctx, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage("a message id is required first")
	}
	fs := flag.NewFlagSet("handback", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reason, key := fs.String("reason", "", ""), fs.String("idempotency-key", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for handback")
	}
	if strings.TrimSpace(*reason) == "" {
		return usage("--reason is required")
	}
	var hdr map[string]string
	if *key != "" {
		hdr = map[string]string{"Idempotency-Key": *key}
	}
	var out any
	if err := c.DoHeaders(context.Background(), hdr, "POST", "/messages/"+url.PathEscape(args[0])+"/handback", map[string]string{"reason": *reason}, &out); err != nil {
		return err
	}
	return c.Print(out)
}
