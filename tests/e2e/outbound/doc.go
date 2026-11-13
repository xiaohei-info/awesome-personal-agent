// Package outbound 是全流程验收 flow-outbound-scope 的端到端测试（验收文档 ACT-OBD-01..19）。
// 真实 apa 二进制（apa_testclock 测试构建）+ tests/e2e/baseline 环境脚本；来源是登记在内核里的验收脚本 OBD_SRC、OBD_RSS，
// 两个模型端点是 CLS_STUB（判断端点）与 MODEL_STUB（生成端点）。断言以两个替身收到的请求体为准。
package outbound
