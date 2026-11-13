// Package followup 是 flow-rule-modes、flow-follow-up、flow-health 的全流程验收（验收文档 ACT-RULE-01..16、ACT-FUP-01..15、ACT-HLT-01..07）：
// 真实 apa 二进制（-tags apa_testclock 构建）+ baseline 的替身环境（MODEL_STUB、CLS_STUB、无头运行替身、账本夹具与 v0.1 二进制），
// 没有任何进程内替身。跟进、截止与健康的时间推进用 baseline 的测试时钟；Web 步骤以本人会话请求 Web 页面调用的同一组 /v1 接口，
// 页面渲染由 web/ 组件测试覆盖。
//
// 运行：go test -tags=e2e -count=1 ./tests/e2e/followup/...（需要 web/dist 已构建）。
package followup
