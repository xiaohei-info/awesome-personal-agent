// telegram-ext 是 Telegram 渠道 ext 的进程入口，配置见 ext/telegram/run.go。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := telegram.Main(ctx, os.Getenv, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "telegram-ext:", err)
		os.Exit(1)
	}
}
