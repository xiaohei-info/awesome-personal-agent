// Package delivery 是触达与打扰控制的全流程验收（构建标签 e2e，见 *_test.go）：
// 验收文档 flow-delivery-control，本包负责 ACT-DLV-01..07（v0.1 继承 action）。
// ACT-DLV-08 起（推迟判定、日上限与静音）由 delivery-defer-judge、delivery-cap-mute 节点各自验收。
//
// 运行前需先构建 Web 前端（测试起的真实 apa serve 要内嵌它）：
//
//	npm --prefix web ci && npm --prefix web run build
//	go test -tags=e2e -count=1 ./tests/e2e/delivery/...
package delivery
