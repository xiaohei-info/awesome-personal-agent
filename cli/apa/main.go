// apa 是 CLI 入口：只解析环境并交给 cli/client；子命令组由 cli/client 内各自的注册文件登记，入口不随之改动。
package main

import (
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/decisions"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/delegation" // 登记 executors、delegate、report、declare 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/guide"      // 登记 guide 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/items"      // 登记 item、propose、inbox 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/projects"   // 登记 project 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/serve"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/why"
)

func main() { os.Exit(client.Main(os.Args[1:], client.OSEnv())) }
