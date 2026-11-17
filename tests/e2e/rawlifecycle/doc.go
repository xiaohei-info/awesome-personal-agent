// Package rawlifecycle 是原文保留期、物理删除与附件取回的全流程验收（构建标签 e2e，见 *_test.go），替身版：
// 真实 apa 二进制（-tags apa_testclock，测试时钟）、桩来源脚本 SRC_STUB（含 fetch 模式）与判断端点替身；
// 不连任何真实账号，不调起浏览器，Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
//
//   - flow-raw-retention：TestFlowRawRetention（ACT-RET-01…03）
//   - flow-raw-purge：TestFlowRawPurge（ACT-PRG-01…03）
//   - flow-attachment-fetch：TestFlowAttachmentFetch（ACT-FET-01…04；ACT-FET-02 的页面部分由 web/src/features/trace 组件测试覆盖）
package rawlifecycle
