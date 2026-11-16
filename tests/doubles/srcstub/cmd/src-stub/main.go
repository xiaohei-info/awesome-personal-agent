// src-stub 是 SRC_STUB 的可执行入口：不带参数为拉取，`fetch` 为取回附件，`identity` 不支持。
package main

import (
	"os"
	"path/filepath"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/srcstub"
)

func main() {
	self, _ := os.Executable()
	os.Exit(srcstub.Run(os.Args[1:], os.Environ(), filepath.Dir(self), os.Stdin, os.Stdout, os.Stderr))
}
