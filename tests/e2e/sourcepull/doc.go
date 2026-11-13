// Package sourcepull 是来源契约与附带拉取程序的全流程验收（构建标签 e2e，见 *_test.go）。
//
// v0.1 不回退，在 baseline 替身环境上用测试构建的真实二进制逐条重跑：
//   - flow-source-contract：TestFlowSourceContract（ACT-SRC-02…10）
//   - flow-source-rss：TestFlowSourceRSS（ACT-RSS-01、02）
//   - flow-source-http-json：TestFlowSourceHTTPJSON（ACT-HTTP-01、02）
//   - flow-source-dir-watch：TestFlowSourceDirWatch（ACT-DIR-01、02）
package sourcepull
