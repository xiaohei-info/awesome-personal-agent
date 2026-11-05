// apa 是 CLI 入口：只解析环境并交给 cli/client；子命令组由 cli/client 内各自的注册文件登记，入口不随之改动。
package main

import (
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/items"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/serve"
)

func main() { os.Exit(client.Main(os.Args[1:], client.OSEnv())) }
