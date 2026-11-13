// Package agents 是 flow-agent-cascade（ACT-CAS-01..10）、flow-outcall-limits（ACT-OUT-01..08）以及 v0.1 替身版不回退的
// flow-delegate-recover（ACT-DLG-02..10）、flow-brief（ACT-BRF-01..06）、flow-guideline-draft（ACT-DRF-01..04）、
// flow-agents-md（ACT-AGM-01..10）、flow-material-hosted（ACT-HST-01..08）的全流程验收（build tag: e2e）。
//
// 后五条流程在替身环境上用测试构建的真实二进制执行：真实宿主换 STUB_HARNESS，代码托管换本地仓库（LOCAL_REPO）加
// TOOL_STUB 的录制事件，"等待几分钟"换测试时钟。离不开真实依赖的断言由本机升级验证 L9（宿主）、L11（邮件日历）、
// L12（远程访问与动作链接）承接。
package agents
