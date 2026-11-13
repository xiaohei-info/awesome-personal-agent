// Package workbench 是工作台、久未推进与第一档的全流程验收（构建标签 e2e，见 *_test.go）：
// flow-web-workbench（不装宿主、不配模型也能在 Web 和 CLI 上记事、安排、推进和回顾，手机宽度可用）、
// flow-stale（久未推进的任务与承诺合并问一次去留，系统不自动改状态）、
// flow-run-tiers（只有内核的第一档自成可用产品）。
// 用 flow-env-baseline 的真实 apa 二进制（测试构建），Web 步骤经与页面相同的 HTTP API 驱动；
// 页面渲染与 360 像素宽度由 ego-browser 在真实浏览器里验证（本机没有 ego-browser 时对应子测试注明原因并跳过）。
package workbench
