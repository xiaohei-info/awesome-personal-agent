// Package backlog 是积压放开与放回旧级联忽略的事件两条全流程的验收（flow-backlog-release、flow-backlog-restore，
// ACT-BKL-01..20、ACT-RST-01..14）：在 flow-env-baseline 的替身环境上，用测试构建的真实 apa 二进制，对升级后的合成账本夹具逐步执行。
// 只在合成夹具上验流程与每日上限，不对不确定带起始值下结论（归 L5）；不使用真实模型端点、宿主或渠道账号。
package backlog
