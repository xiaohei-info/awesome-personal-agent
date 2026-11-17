// Package importwb 是带原始时间的导入的全流程验收（构建标签 e2e，见 *_test.go），替身版：真实 apa 二进制（-tags apa_testclock），
// 合成的计划文件，STUB_HARNESS 替身（简报取材）；不连任何真实账号，不调起浏览器，Web 步骤走 Web 页面调用的同一套 HTTP API
// （口令登录的会话 + CSRF，请求体 application/x-ndjson）。
//
//   - flow-import-apply：TestFlowImportApply（验收文档 ACT-IMP-01..11）。
//   - flow-import-no-automation：TestFlowImportNoAutomation（ACT-INA-01..05；OMAC-NOTES §3 I5 的逐个订阅方清单见该测试的 subscribers）。
//   - flow-import-grant：TestFlowImportGrant（ACT-IGR-01..08；授权单经 Web 登录的 API 签发）。
//
// 与验收文档的差别与覆盖边界：
//   - 计划文件里的时刻相对内核"现在"向前推（40 天前起，逐日一步），不写死 2026-10 的日期：测试在任何一天跑，历史都在过去。
//   - 事件订阅的游标参数是 `cursor`（API 附录 B.2），验收文档里写的 `after` 不是该路由的参数；断言按 `cursor` 做。
//   - 内核没有 project.created 事件：导入建项目写的是带导入标记的 project.updated（载荷 op 为 create），`at` 取输入的 created_at。
//   - 导入的 T-0002 已过期的截止由时钟扫描照常触发一次 item.deadline_passed（设计 §7.1 "这是真实情况，不屏蔽"）；它晚了一天才触发，
//     内核按"时钟滞后"另记一条 health.alert 与一条 web 兜底投递，二者属于这次真实触发，不属于导入事件，断言按"只许是这一件"点名放行。
//   - 久未推进只看 open 的任务：T-0002 是 waiting，不会进该组；ACT-INA-04 另用一条 open 的导入事项（T-0003）验证。
//   - 当天活动的断言用一批 at 落在简报窗口内的导入事件（acc-imp-recent）：T-0001 的历史在 40 天前，不在窗口里，放过滤与否都看不出差别。
//   - 订阅方的逐个断言：触达投递、规则触发、摘要起草、注入增量、对外事件流、久未推进、简报取材由 TestFlowImportNoAutomation 断言；
//     规则、触达、跟进三个订阅方另有 TestFlowImportNoAutomationSubscribers，每项都带"现场对照"（同类现场事件使它产生可见变化，导入的不产生）。
//     触达与跟进的对照把事件直接追加进已停下的账本（eventlog.Append，只在测试目录里，写法同内核单测），因为导入出来的事项不会处于"待决策"这类会被重算出投递的状态。
//     "统计"类订阅方（direct 统计、判断统计、健康的 handled）要本人之后的操作或 resolved_via 才受影响，导入夹具触达不到，只在节点级测试里覆盖，这里不声称逐个断言了它们；
//     direct_health 等表的行数不变只是附带检查。
//   - 简报页的显示由 web 组件测试覆盖；中途失败用 SIGKILL 杀 apa serve 模拟，属于测试目录里的故障注入。
package importwb
