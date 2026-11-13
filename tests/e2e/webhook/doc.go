// Package webhook 是 webhook 入站的全流程验收（构建标签 e2e，见 *_test.go）。
//
// v0.1 不回退（GT-328），在 baseline 替身环境上用测试构建的真实二进制逐条重跑：
//   - flow-webhook：本包 TestFlowWebhook（ACT-HOOK-01…13）
//   - flow-event-subscribe：tests/e2e/events TestFlowEventSubscribe（ACT-EVT-01…04）
//   - flow-channel-protocol：tests/e2e/channel TestCHP01…TestCHP11（ACT-CHP-01…11）
//   - flow-chain-depth：tests/e2e/bgguards TestFlowChainDepth（ACT-DEP-01…03）
package webhook
