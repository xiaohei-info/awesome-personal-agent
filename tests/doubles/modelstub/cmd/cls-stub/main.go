// cls-stub 独立运行 CLS_STUB：cls-stub -addr 127.0.0.1:18081
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
	flag.Parse()
	s := modelstub.NewClsAt(*addr)
	defer s.Close()
	fmt.Println(s.URL())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c
}
