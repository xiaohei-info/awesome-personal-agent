// Package briefwb 是简报资料扩展的全流程验收（构建标签 e2e，见 *_test.go），替身版：真实 apa 二进制（-tags apa_testclock）、
// 桩来源脚本 SRC_STUB（tests/doubles/srcstub）与 STUB_HARNESS 替身（brief 模式：读资料文件与原文文件并写回）；
// 不连任何真实账号，不调起浏览器，Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
//
//   - flow-brief-material：TestFlowBriefMaterial（OMAC-NOTES §3 B3；对应验收 ACT-BRF-02…06，含 ACT-BRF-07 只写 period 的一支；
//     简报页的显示由 web-brief-sections 的组件测试覆盖，运行凭据读来源事件 403 由 rawingest 包的 ACT-RSC 覆盖）
//
// 账本夹具里"导入的事件"没有走导入命令（I3 不在本节点的上游），而是在内核停下时直接以 eventlog.Append 写入带批次标记的事件，
// 与内核单测同一写法；只在测试目录里，不进发布构建。
package briefwb
