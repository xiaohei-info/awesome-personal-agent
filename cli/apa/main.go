// apa 是 CLI 入口：只解析环境并交给 cli/client；子命令组由 cli/client 内各自的注册文件登记，入口不随之改动。
package main

import (
	"os"

	_ "github.com/xiaohei-info/awesome-personal-agent/cli/agents" // 登记 agents 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/backup" // 登记 backup 子命令
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/context" // 登记 context、persona 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/decisions"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/delegation" // 登记 executors、delegate、report、declare 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/events"     // 登记 events 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/ext"        // 登记 ext 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/followmsg"  // 登记 handback 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/guide"      // 登记 guide 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/health"     // 登记 health 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/items"      // 登记 item、propose、inbox 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/judgment"   // 登记 judgment 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/materials"  // 登记 material、blob 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/packs"      // 登记 pack、instance 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/projects"   // 登记 project 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/runs"       // 登记 run 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/serve"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/sources" // 登记 source 子命令
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/why"
)

func main() { os.Exit(client.Main(os.Args[1:], client.OSEnv())) }
