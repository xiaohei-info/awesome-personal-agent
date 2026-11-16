// Package rawingest 是来源原文入账、上限、webhook 与读取范围的全流程验收（构建标签 e2e，见 *_test.go），替身版：
// 真实 apa 二进制（-tags apa_testclock）、桩来源脚本 SRC_STUB（tests/doubles/srcstub）与结构版本 7 的合成账本夹具；
// 不连任何真实账号，不调起浏览器，Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
//
//   - flow-raw-full-ingest：TestFlowRawFullIngest（ACT-RAW-01…06）
//   - flow-raw-limits：TestFlowRawLimits（ACT-RLIM-01…04）
//   - flow-raw-webhook：TestFlowRawWebhook（ACT-WHK-01、02）
//   - flow-raw-read-scope：TestFlowRawReadScope（ACT-RSC-01…07，含升级前事件 legacy 可读）
package rawingest
