package conn

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock/wall"
)

// 收听保活（§2.7）：断开后按 5 秒到 5 分钟退避重连；连续断开超过 NotListeningAfter 报告一次没在收听。
var (
	ListenBackoffMin  = 5 * time.Second
	ListenBackoffMax  = 5 * time.Minute
	NotListeningAfter = 2 * time.Minute
)

// Keep 维持连接 t 的收听直到 ctx 结束；插件不实现 Listener 时立即返回。
// report 在断开超过 NotListeningAfter 时调用一次（false），之后重新连上时调用一次（true），由调用模块写连接状态；
// Keep 本身不写账本。一次收听持续超过 NotListeningAfter 才算稳定：之后再断开，退避与计时从头算。
func (c *Caller) Keep(ctx context.Context, t Target, in Inbound, report func(listening bool)) {
	p, ok := c.plugins[t.Type]
	if !ok {
		return
	}
	wait := ListenBackoffMin
	var downSince time.Time
	reported := false
	for ctx.Err() == nil {
		started := wall.Now()
		err := c.listenOnce(ctx, p, t, in, func() {
			if reported {
				reported = false
				report(true)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err == errNotListener {
			return
		}
		if wall.Now().Sub(started) > NotListeningAfter {
			wait, downSince = ListenBackoffMin, time.Time{}
		}
		if downSince.IsZero() {
			downSince = wall.Now()
		}
		slog.Warn("connection listen stopped", "connection", t.ID, "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !reported && wall.Now().Sub(downSince) >= NotListeningAfter {
			reported = true
			report(false)
		}
		wait = min(wait*2, ListenBackoffMax)
	}
}

var errNotListener = fmt.Errorf("connector does not listen")

func (c *Caller) listenOnce(ctx context.Context, p Plugin, t Target, in Inbound, up func()) (err error) {
	sec, err := c.secretsOf(ctx, t, p.Decl, nil)
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("connector panicked while listening", "type", t.Type, "connection", t.ID, "panic", scrub(fmt.Sprint(r), sec))
			err = Fail(ProblemTemporary, "the connector crashed")
		}
	}()
	ch, err := p.New(Config{Settings: t.Settings, Secrets: sec})
	if err != nil {
		return clean(err, sec)
	}
	l, ok := ch.(Listener)
	if !ok {
		return errNotListener
	}
	up()
	return clean(l.Listen(ctx, in), sec)
}
