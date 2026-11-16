// Package briefwb 是简报资料扩展的全流程验收（构建标签 e2e，见 *_test.go），替身版：真实 apa 二进制（-tags apa_testclock）、
// 桩来源脚本 SRC_STUB（tests/doubles/srcstub）、分类与对话端点替身与 STUB_HARNESS 替身（brief 模式：读资料文件与原文文件并写回）；
// 不连任何真实账号，不调起浏览器，Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
//
//   - flow-brief-material：TestFlowBriefMaterial（OMAC-NOTES §3 B3；步骤名里的 ACT-BRF-xx 是验收文档的动作编号）。
//     夹具把测试时钟推进 15 天，之后的"今天"都以内核时间为准；到期与等待六类（今天、明天、已过期、回音、跟进到期、久未推进）、
//     待决策三条（普通、web_only、初筛未返回）、三条建议（一条把握低，各带推荐与理由）、导入事件与四种外发范围的知会都在夹具里。
//   - TestBriefEmptySegments：账本为空时各段都是空数组。
//
// 与验收文档的差别与覆盖边界：
//   - 决策段：设计 §7.3 只排除"初筛未返回"的；web_only 的照列，带 web_only 标记，所以断言是普通与 web_only 两条、不含初筛未返回的。
//   - "导入的事件"没有走导入命令（I3 不在本节点的上游），而是在内核停下时直接以 eventlog.Append 写入带批次标记的事件，
//     与内核单测同一写法；只在测试目录里，不进发布构建。
//   - 日历来源只用 source_type=calendar 的 SRC_STUB：内核按来源的 source_type 取日程，与来源程序无关；apple-pim-source 的日历接入
//     （config.kind=calendar）由 tests/e2e/sourcesv2 覆盖，这里不重放它的录制样本（样本日期固定，不在测试时钟的窗口里）。
//   - 简报页的显示由 web-brief-sections 的组件测试覆盖；运行凭据读来源事件 403（ACT-BRF-08）由 rawingest 包的 ACT-RSC 覆盖。
//   - 原文文件的只读以运行目录里 source-event-*.txt 无写权限位为准；instruction.txt 内核写成 0600，不在验收的只读范围内。
package briefwb
