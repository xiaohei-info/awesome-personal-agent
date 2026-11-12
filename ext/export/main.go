// export 是导出 ext（设计 §10.3）：用只读的导出 ext 密钥订阅 GET /v1/events，按需读 /items、/materials，
// 单向写成本地目录里的 Markdown，并把游标自持在该目录。独立进程，只经 HTTP，不写账本、不依赖任何笔记软件或 harness。
//
// 环境变量：APA_URL（内核地址）、APA_EXT_KEY（导出 ext 密钥，只读）、APA_EXPORT_DIR（目标目录，缺省取配置文件的 root）、
// APA_EXPORT_CONFIG（版式配置 YAML，可选，见 README.md）、APA_PUBLIC_URL（配置文件没写 public_url 时的后备）。
//
// 至少一次：一页事件全部导出成功后才把 next 写入游标；导出是按对象 id 覆盖写，重放无害。启动时先按事项列表对账。
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
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
}
