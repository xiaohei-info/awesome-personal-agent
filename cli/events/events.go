// Package events 是 apa 的事件订阅命令（附录 B.1「可观测」，设计 §10.1）：apa events [--follow] [--cursor N]。
//
// 薄客户端：只调 GET /v1/events（长轮询），信封形状、登记类型过滤与授权全由内核完成。
// 游标由调用方自持：每条信封的 seq 就是处理完它之后应持有的游标，本命令不保存任何状态；
// 一条信封写出到 stdout 之后才推进下一次请求的游标（至少一次）。
package events

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventfeed"
)

func init() { client.Register("events", run) }

const (
	usageHint = "usage: apa events [--follow] [--cursor <seq>] [--limit <n>] [--pretty]"
	// followWait 是每次长轮询的等待秒数；小于 CLI HTTP 超时（60s）。
	followWait = 30
)

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

func run(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	follow := fs.Bool("follow", false, "")
	cursor := fs.Int64("cursor", 0, "")
	limit := fs.Int64("limit", 0, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments")
	}
	if *cursor < 0 {
		return usage("--cursor must be >= 0")
	}
	if *limit < 0 {
		return usage("--limit must be >= 0")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return stream(ctx, c, *follow, *cursor, *limit)
}

// stream 每次取一页并逐条输出。非 follow 只取一页；follow 持续长轮询，直到被中断、出错或已输出 limit 条（limit>0）。
func stream(ctx context.Context, c *client.Ctx, follow bool, cursor, limit int64) error {
	var sent int64
	for {
		q := url.Values{"cursor": {strconv.FormatInt(cursor, 10)}}
		if follow {
			q.Set("wait", strconv.Itoa(followWait))
		}
		if limit > 0 {
			q.Set("limit", strconv.FormatInt(min(limit-sent, 500), 10))
		}
		var page eventfeed.Page
		if err := c.Do(ctx, "GET", "/events?"+q.Encode(), nil, &page); err != nil {
			if ctx.Err() != nil { // 被中断：正常结束，不是错误
				return nil
			}
			return err
		}
		for _, e := range page.Envelopes {
			if err := emit(c, e); err != nil {
				return err
			}
			cursor = e.Seq
			sent++
		}
		cursor = max(cursor, page.Next) // 越过不可见事件，游标不卡在原地
		if !follow || (limit > 0 && sent >= limit) {
			return nil
		}
	}
}

func emit(c *client.Ctx, e eventfeed.Envelope) error {
	if !c.Pretty {
		return c.Print(e)
	}
	_, err := fmt.Fprintf(c.Env.Stdout, "%d  %s  %s  %s  %s\n", e.Seq, e.Type, e.Object, e.Actor.Principal, e.Title)
	return err
}
