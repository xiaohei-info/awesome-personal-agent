// model-stub 独立运行 MODEL_STUB：model-stub -addr 127.0.0.1:18080 [-wb]
// 编排：POST /_stub/script；查看：GET /_stub/requests；清空：POST /_stub/reset。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	wb := flag.Bool("wb", false, "预载工作台脚本样例（two-refs 等，按请求体含用例名命中）")
	flag.Parse()
	s := modelstub.NewAt(*addr)
	if *wb {
		s.Script(modelstub.WBScript(modelstub.WBParams{}))
	}
	defer s.Close()
	fmt.Println(s.URL())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}
