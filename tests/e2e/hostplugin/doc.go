// Package hostplugin 是宿主插件上下文与唤醒租约的全流程验收（替身与插件契约测试版，构建标签 e2e，见 *_test.go）：
// 验收文档 flow-hermes-context、flow-wake-lease，本包是两者的唯一全流程 owner。
//
// 只用替身：真实 apa 二进制（测试构建）、STUB_HARNESS 扮演插件、HTTP 调用扮演网关插件、Python 插件契约测试
// （宿主上下文对象与内核替身）。真实 Hermes 上的注入、钩子放行、恢复与压缩后重新注入、网关里的注入回合由本机升级验证 L9 承接。
//
// 运行前需先构建 Web 前端（测试起的真实 apa serve 要内嵌它），并装有 python3 与 pytest：
//
//	npm --prefix web ci && npm --prefix web run build
//	go test -tags=e2e -count=1 ./tests/e2e/hostplugin/...
package hostplugin
