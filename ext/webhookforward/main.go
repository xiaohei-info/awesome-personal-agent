// webhookforward 是 webhook 转发 ext（设计 §10.3）：用只读的 ext 密钥订阅 GET /v1/events，
// 把每条事件信封签名 POST 给外部系统。出站连接由本进程发起，内核不外呼。独立进程，只经 HTTP。
//
// 环境变量：APA_URL（内核地址）、APA_EXT_KEY（只读 ext 密钥）、APA_WEBHOOK_URL（接收方）、
// APA_WEBHOOK_SECRET（HMAC-SHA256 签名密钥）、APA_WEBHOOK_STATE（游标文件路径）；可选 APA_WEBHOOK_TYPES（逗号分隔的事件类型，只转发这些；缺省转发全部，未选中的事件游标照常推进）。
//
// 至少一次：一条事件被接收方以 2xx 确认后才推进游标；失败按指数退避重试同一条，接收方按事件 id（seq）幂等。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := ConfigFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webhookforward:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "webhookforward:", err)
		os.Exit(1)
	}
}
