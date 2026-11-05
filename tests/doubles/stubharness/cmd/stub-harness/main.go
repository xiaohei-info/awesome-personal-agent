// stub-harness 是 STUB_HARNESS 的可执行入口，行为开关见 stubharness 包文档。
package main

import (
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/stubharness"
)

func main() {
	dir, _ := os.Getwd()
	os.Exit(stubharness.Run(os.Args[1:], os.Environ(), dir, os.Stdout))
}
