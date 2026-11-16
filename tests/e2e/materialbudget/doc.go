// Package materialbudget 是资料预算与裁剪留痕的全流程验收（构建标签 e2e，见 *_test.go），替身版：
// 真实 apa 二进制（-tags apa_testclock）、桩来源脚本 SRC_STUB、分类端点替身 CLS_STUB、对话端点替身 MODEL_STUB
// 与结构版本 7 的合成账本夹具；不连任何真实账号，不调起浏览器，Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
//
//   - flow-material-budget：TestFlowMaterialBudget（ACT-BUD-01…08）
package materialbudget
