# 已知失败清单（e2e）

**上线时只拦截清单外的新失败；清单内条目需定期清理，清单不是垃圾桶。**

本清单只收已经定性过的失败：写清现象、复现命令、定性依据、是否阻塞上线、处理状态与对应提交。定性为"真 bug"的条目只记录不改代码，修法建议写在条目里。断言过期与环境问题修掉后把状态改成"已修"并保留一个发布周期，之后删掉整条。

## 复现前的准备

e2e 起真实的 `apa serve`，需要先构建 Web（CI 的 `e2e-smoke` 作业也是这么做的），并装 `sqlite3` CLI（账本断言用）：

```sh
npm --prefix web ci && npm --prefix web run build
```

`tests/e2e/workbench` 的两条浏览器步骤在本机没有 `ego-browser` 时跳过；要在 CI 风格的机器上强制暴露浏览器失败，设 `APA_E2E_REQUIRE_BROWSER=1`。

同一 `TestFlow*` 里的步骤前后依赖，不能只跑其中一步：`-run 'TestFlowX/ACT-…'` 会跳过前置步骤，后面的断言拿不到数据。按用例粒度跑，例如：

```sh
go test -tags e2e -count=1 -timeout 15m -run 'TestFlowWebWorkbench$' ./tests/e2e/workbench/
go test -tags e2e -count=1 -timeout 15m -run 'TestFlowSourceMail$' ./tests/e2e/sourcesv2/
go test -tags e2e -count=1 -timeout 20m -run 'TestFlowCapture$|TestFlowInboundPipeline$|TestFlowTriageFailure$' ./tests/e2e/inbound/
```

## 总览

| 用例 | 定性 | 阻塞上线 | 状态 |
|---|---|---|---|
| `tests/e2e/workbench` `TestFlowWebWorkbench/ACT-WBN-03` | 断言过期 | 否 | 已修（`8d525e43`） |
| `tests/e2e/sourcesv2` `TestFlowSourceMail/ACT-MAIL-04` | 断言不稳（依赖时序） | 否 | 已修（`8d525e43`） |
| `tests/e2e/inbound` `TestFlowCapture/ACT-CAP-01` | 断言过期 | 否 | 已修（`8d525e43`） |
| `tests/e2e/inbound` `TestFlowInboundPipeline/ACT-INP-08` | 断言过期 | 否 | 已修（`8d525e43`） |
| `tests/e2e/inbound` `TestFlowTriageFailure/ACT-TRI-02`（含 ACT-TRI-07 同一处） | 断言过期 | 否 | 已修（`8d525e43`） |
| `tests/e2e/workbench` `TestFlowWebWorkbench/ACT-WBN-16` | 前两处断言过期（`8d525e43`），余下为真 bug（`1a8f717d`） | 否 | 已修（`8d525e43`、`1a8f717d`） |
| `tests/e2e/agents` `TestFlowOutcallProc/ACT-OUT-06`；`tests/e2e/mapping` `TestInheritedFlows/flow-outcall-limits`（见第 7 条） | 断言过期 | 否 | 已修（`e20864242`） |
| `tests/e2e/followup`、`tests/e2e/ops` `TestFlowHealth` 的「造出：外呼超时」「长时运行与短任务各自排队」（见第 8 条） | 断言过期 | 否 | 已修（`e20864242`） |
| `tests/e2e/followup`、`tests/e2e/ops` `TestFlowHealth/ACT-HLT-01`、`ACT-HLT-02`（见第 9 条） | 断言不稳（依赖时序） | 否 | 已修（`d8085e45c`） |
| `tests/e2e/followup` `TestFlowHealthWarnings/ACT-HLT-06`（见第 10 条） | 断言不稳（依赖时序） | 否 | 已修（`d4c33da02`） |
| `tests/e2e/bgguards` `TestFlowBgGuards/ACT-BGG-05`（见第 11 条） | 用例隔离（依赖时刻） | 否 | 已修（`b359aa4e0`） |
| `tests/e2e/ops` `TestFlowPurge/ACT-PRG-01`；`tests/e2e/materials` `TestOpsFlows/flow-purge`（见第 12 条） | 断言过期 | 否 | 已修（`0d22b9df0`） |
| `tests/e2e/ops` `TestFlowRetention/*/ACT-RET-01`、`ACT-RET-02`；`tests/e2e/materials` `flow-retention`（见第 13 条） | 断言过期 | 否 | 已修（`802ba9463`、`b26f131e4`、`d880f8433`、`bcda9fdb4`） |
| `tests/e2e/upgrade` `TestSchema8Upgrade/ACT-S8-01`（见第 14 条） | 断言过期 | 否 | 已修（`fdf60c821`） |
| `tests/e2e/ledger` `TestFlowBootstrap/ACT-BOOT-03`、`ACT-BOOT-05`（见第 15 条） | 断言过期 | 否 | 已修（`8848a1600`、`dbc7d583e`） |
| `tests/e2e/inbound` `TestFlowCapture/ACT-CAP-06`、`TestFlowInboundPipeline/ACT-INP-10`（见第 16 条） | 断言过期（模型替身串扰） | 否 | 已修（`9e3a6aa95`） |
| `tests/e2e/extractalign` `TestFlowExtractAlign/ACT-XAL-04`、`ACT-XAL-06`（见第 17 条） | 断言过期 | 否 | 已修（`4ce8b3fae`） |
| `tests/e2e/followup`、`tests/e2e/ops` `TestFlowHealth` 的「长时运行与短任务各自排队」；`tests/e2e/agents` `TestFlowGuidelineDraft/ACT-DRF-04`；`tests/e2e/automation` `TestFlowRoutine/ACT-RTN-02`（见第 18 条） | 用例隔离（依赖时刻） | 否 | 已修（`ec4acf532`、`1db9ac5fb`） |
| `tests/e2e/automation` `TestFlowDigestAlert/ACT-DGA-15`（见第 19 条） | 断言不稳（测试替身竞态） | 否 | 已修（`1db9ac5fb`） |

## 明细

### 1. `TestFlowWebWorkbench/ACT-WBN-03` 事项详情 Markdown 渲染：列表项文字

- **复现**：`go test -tags e2e -count=1 -timeout 15m -run 'TestFlowWebWorkbench$' ./tests/e2e/workbench/`（需要 `ego-browser`）。
- **现象**：`workbench_test.go:103: 列表项 = [周五出发\n航司]，期望 [周五出发]`。正文是 `## 行程` / `- 周五出发` / `[航司](https://example.com)`，链接行紧跟列表项、中间没有空行。
- **定性**：断言过期。CommonMark 里这行是该列表项的续行（lazy continuation），渲染结果是 `<li>周五出发<br>航司</li>`（`remark-breaks` 把软换行变成 `<br>`），所以列表项的 `textContent` 是「周五出发」换行「航司」。前端 `web/src/features/items/Markdown.tsx` 没有做错；同一条正文的 Web 单测（`web/src/features/items/items-edit.test.tsx`）一直按这个结构断言，e2e 的期望是后来加链接行时漏改的。
- **是否阻塞上线**：否。
- **处理**：已修。期望改为真实文字，并在用例里写清为什么链接在列表项内。没有改正文与渲染实现。

### 2. `TestFlowSourceMail/ACT-MAIL-04` 工具未安装：其他来源照常运行

- **复现**：`go test -tags e2e -count=1 -timeout 15m -run 'TestFlowSourceMail$' ./tests/e2e/sourcesv2/`。
- **现象**：`mail_test.go:142: 其他来源应照常运行`。`src_mail` 失败的那一小段窗口内，`src_other` 的 `stats.runs` 没有增加。
- **定性**：断言不稳（依赖时序），不是产品缺陷。加日志复现可见 `src_other` 确实照常运行（`runs` 由 0 变 1、游标推进到 `c1`），只是它的定时节拍与邮件来源失败不在同一秒：原断言在邮件失败后立刻比较次数，窗口短于另一来源的下一个节拍。两个来源都走 `proc` 的短队列（`max_short_subprocesses` 缺省 2），互不阻塞。
- **是否阻塞上线**：否。
- **处理**：已修。改为等另一来源在下一个节拍再跑一次（`k.until`），不再当场比次数；邮件来源失败、游标不动、认证或配置失败的断言不变。

### 3. `TestFlowCapture/ACT-CAP-01` 默认关闭时不捕获

- **复现**：`go test -tags e2e -count=1 -timeout 20m -run 'TestFlowCapture$' ./tests/e2e/inbound/`。
- **现象**：`capture_test.go:84: 应返回 200 {captured:false, reason}: 200 {Captured:true Reason:}`。
- **定性**：断言过期。对话捕获已改为默认开启：新建项目 `Capture: true`，启动迁移 `SeedCapture` 把已有项目逐个补开（§6.5 的项目字段表、§16.5 的默认值变更）。用例名与期望都停留在"默认关闭"。
- **是否阻塞上线**：否。
- **处理**：已修。步骤改为「关掉项目的捕获后不捕获」：先经 Web 把该项目的 `capture` 置 false，再断言不捕获且原因是 `capture_disabled`；下一步照旧把它打开，后续步骤不受影响。

### 4. `TestFlowInboundPipeline/ACT-INP-08` 起草失败不挡路：建议的项目

- **复现**：同上（`-run 'TestFlowInboundPipeline$'`）。
- **现象**：`pipeline_test.go:206: project = prj_inbox，期望 `（空）。
- **定性**：断言过期。未指定项目的非 `brief`/`routine` 事项现在自动落入内置 Inbox（决定 92、§6.5）。
- **是否阻塞上线**：否。
- **处理**：已修。期望改为 `prj_inbox`。

### 5. `TestFlowTriageFailure/ACT-TRI-02` 手动转建议：建议的项目

- **复现**：同上（`-run 'TestFlowTriageFailure$'`）。
- **现象**：`triage_test.go:131: project = prj_inbox，期望 `（空）。ACT-TRI-07（`triage_test.go:264`）是同一处，因前一步失败没有跑到。
- **定性**：断言过期，原因同第 4 条（内置 Inbox 兜底）。
- **是否阻塞上线**：否。
- **处理**：已修。两处期望都改为 `prj_inbox`。

### 6. `TestFlowWebWorkbench/ACT-WBN-16` 360 像素宽度逐页走查

这条不在最初的五条里：前面的 ACT-WBN-03 一直失败，这条步骤在主线里从未被执行到。逐条查完有三个问题，前两个是断言过期、已修，第三个是真 bug、只记录。

- **复现**：同第 1 条的用例（`ego-browser` + 360×780 设备模拟）。

**6a. 批量按钮找不到（已修）**：`page.waitForSelector timed out: loc=role:button[name='按推荐处理']`。按钮的可访问名带条数（「按推荐处理 12 条」），`loc=role:` 的 `name=` 带引号是精确匹配，只有 `name*=` 才是子串。改按子串找。

**6b. 今天页分组标题过期（已修）**：`今天页四组 = [要你处理 今天的事（1）]，期望 [计划今天或更早（0） 今天到期（1） 等你决定（1） 久未推进（0）]`。今天页已按界面设计 §5 改为「要你处理 / Agent 交回的 / 今天的事（n） / 提醒（n）」，计划今天或更早与今天到期合成「今天的事」，久未推进收在折叠的 `details` 里（不是 `main section h2`）。期望改为按 `/v1/today` 的计数断言「今天的事（n）」与（有等你决定时）「要你处理」。

**6c. 360 像素下今天页与事项页横向溢出（已修，`1a8f717d`）**：`页面 today 的 scrollWidth = 401，超出 360`、`页面 items 的 scrollWidth = 401`，`这些页面在 360 宽度下有控件超出视口: [today(1 个控件超出视口) items(12 个控件超出视口)]`。实测撑宽的是行内操作按钮组：`.act-row > .actions` 里五个按钮（排时间 69 + 交给 Agent 97 + 完成 55 + 记进展 69 + 更多 55，加 4 个 6px 间距共 369px），而 360 宽度下 `main` 左右各 16px 内边距只剩 328px。

  - **根因**：`web/src/app/styles.css` 的手机断点（`@media (max-width: 820px)`）里 `.act-row > .actions { flex-wrap: nowrap }`、按钮 `white-space: nowrap`，注释写的是"行上四个快捷操作……占满一行并排放下，不收进「更多」"。`RowActions` 后来加了第五个按钮「更多」（`web/src/features/items/RowActions.tsx`），四个变五个后一行放不下，溢出的 41px 把整页撑宽。今天页（`TodayRow`）与事项列表共用这套行操作，所以两页都超。
  - **影响面**：360 像素宽度下今天页、事项列表出现横向滚动，与界面设计 §4.1/§web-ui 的"全部页面 360 宽度可读可操作、无横向滚动"不符；桌面与平板宽度不受影响，触控操作本身可用。
  - **处理**：已修（`1a8f717d`）。按用户口径在窄屏（`≤ 820px`）保持主操作（「排时间」、「完成」）直接可见，将次要动作（「交给 Agent」、「记进展」、「删除…」）收进「更多」弹出菜单（`role="menu"`，支持方向键/Home/End/Esc 键盘操作、点外部关闭、44px 触控高度），桌面宽屏保持原五按钮布局不变；同时在 `ACT-WBN-16` 补充了展开「更多」菜单时的 360px 边界与 Esc 关闭断言。重跑 `TestFlowWebWorkbench` 全部 17 个子步骤通过，14 页 `scrollWidth` 均为 `360`，无其它断言问题。

### 7. `tests/e2e/agents` `TestFlowOutcallProc/ACT-OUT-06`；`tests/e2e/mapping` `TestInheritedFlows/flow-outcall-limits`

- **复现**：按上文"复现前的准备"，跑 `TestFlowOutcallProc`（`tests/e2e/agents`）、`TestInheritedFlows`（`tests/e2e/mapping`） 所在包的整个用例。
- **现象**：`outcall_test.go:529: 等待超时（1m30s）：Run 超时`
- **定性**：断言过期。任务型运行的时限取策略 `background_run_timeout_sec`（默认 4 小时），模板 `timeout` 不再生效（`f18efed04`）。
- **是否阻塞上线**：否。
- **处理**：已修（`e20864242`）。超时后拨测试时钟越过默认时限，去掉"不足 20 秒即过早"的旧假设。

### 8. `tests/e2e/followup`、`tests/e2e/ops` `TestFlowHealth` 的「造出：外呼超时」「长时运行与短任务各自排队」

- **复现**：按上文"复现前的准备"，跑 `TestFlowHealth`（两个包） 所在包的整个用例。
- **现象**：`外呼超时进入健康视图（等待 1m0s 仍未出现）`；挂起的运行占住唯一长时额度时，下一步凑不出「1 在跑、2 排队」。
- **定性**：断言过期。原因同第 7 条。
- **是否阻塞上线**：否。
- **处理**：已修（`e20864242`）。把策略时限临时调到下限 900 秒、拨 901 秒，再改回默认值。

### 9. `tests/e2e/followup`、`tests/e2e/ops` `TestFlowHealth/ACT-HLT-01`、`ACT-HLT-02`

- **复现**：按上文"复现前的准备"，跑 `TestFlowHealth`（两个包） 所在包的整个用例。
- **现象**：`信号 unjudged_backlog 应显示为异常并带原因 … OK:true Count:0`
- **定性**：断言不稳（依赖时序）。拨测试时钟越过重判退避后内核重判积压（§9.3），重判中的事件短暂回到 pending；前面步骤修好后全流程变快，读信号恰落在这个窗口。
- **是否阻塞上线**：否。
- **处理**：已修（`d8085e45c`）。读信号前等未判定积压落回。

### 10. `tests/e2e/followup` `TestFlowHealthWarnings/ACT-HLT-06`

- **复现**：按上文"复现前的准备"，跑 `TestFlowHealthWarnings` 所在包的整个用例。
- **现象**：`health_test.go: 65 分钟后 clock_lag 恢复（等待 30s 仍未出现）`
- **定性**：断言不稳（依赖时序）。失败时的 `health.alert` 审计记录显示每轮都新记迟到约 5.5–7.5 分钟：用例每 300 毫秒拨 4 分钟，而测试构建的拨表通知（`clock.Changed()`）是单个单缓冲 channel，`6735903d7` 起保留期清理也在接收；被它取走时时钟循环要等 1 秒的 tick，几次拨表合并成一次扫描。发布构建里该 channel 恒为 nil，不受影响。
- **是否阻塞上线**：否。
- **处理**：已修（`d4c33da02`）。拨表步进改为 1.5 秒。测试构建的通知改为广播属产品目录内代码，本次未改。

### 11. `tests/e2e/bgguards` `TestFlowBgGuards/ACT-BGG-05`

- **复现**：按上文"复现前的准备"，跑 `TestFlowBgGuards` 所在包的整个用例。
- **现象**：`guards_test.go: 等待超时（20s）：运行中`
- **定性**：用例隔离（依赖时刻）。失败时的运行清单显示：拨表 4 小时后多出一个在跑的任务型运行（到点的定时安排，替身处于挂起模式），占住 `ACT-BGG-03b` 设下的唯一长时额度，本步运行停在 queued。是否拨过定时点取决于跑用例的时刻。
- **是否阻塞上线**：否。
- **处理**：已修（`b359aa4e0`）。`ACT-BGG-03b` 结束时恢复默认额度并重启内核。

### 12. `tests/e2e/ops` `TestFlowPurge/ACT-PRG-01`；`tests/e2e/materials` `TestOpsFlows/flow-purge`

- **复现**：按上文"复现前的准备"，跑 `TestFlowPurge`（`tests/e2e/ops`）、`TestOpsFlows`（`tests/e2e/materials`） 所在包的整个用例。
- **现象**：`purge_test.go:113: open …/host/serve.sock: no such device or address`
- **定性**：断言过期。`77d4344c2` 起数据目录里有 unix socket，搜索辅助函数逐个读文件。
- **是否阻塞上线**：否。
- **处理**：已修（`0d22b9df0`）。跳过非普通文件。

### 13. `tests/e2e/ops` `TestFlowRetention/*/ACT-RET-01`、`ACT-RET-02`；`tests/e2e/materials` `flow-retention`

- **复现**：按上文"复现前的准备"，跑 `TestFlowRetention`（`tests/e2e/ops`）、`TestOpsFlows`（`tests/e2e/materials`） 所在包的整个用例。
- **现象**：`retention_test.go: 期外请求体被清理（等待 1m40s 仍未出现）`
- **定性**：断言过期。清理改为启动时一次加每天 04:10，运行目录默认保留期由 14 天改为 30 天（`6735903d7`）。`ACT-RET-02` 此前执行不到：拨时钟后到点的简报也拉起任务型运行，按次数认运行会多数一个。
- **是否阻塞上线**：否。
- **处理**：已修（`802ba9463`、`b26f131e4`、`d880f8433`、`bcda9fdb4`）。`ACT-RET-01` 拨完账本时间后重启内核；`ACT-RET-02` 拨测试时钟越过 04:10，并按提示词模板认本步的运行。

### 14. `tests/e2e/upgrade` `TestSchema8Upgrade/ACT-S8-01`

- **复现**：按上文"复现前的准备"，跑 `TestSchema8Upgrade` 所在包的整个用例。
- **现象**：`apa version 的 schema.ledger = 10，期望 9`
- **定性**：断言过期。`13aa41570` 引入账本结构版本 10。
- **是否阻塞上线**：否。
- **处理**：已修（`fdf60c821`）。期望值取迁移登记表的最新版本。

### 15. `tests/e2e/ledger` `TestFlowBootstrap/ACT-BOOT-03`、`ACT-BOOT-05`

- **复现**：按上文"复现前的准备"，跑 `TestFlowBootstrap` 所在包的整个用例。
- **现象**：`Web 包里没有页面 "决策待办"`；修掉后露出 `建事项: 23 … tier_closed`
- **定性**：断言过期。页面标题随术语改版更换（`2017557d3`、`f8290308f`）；未归属 Agent 的钥匙只能整理，`apa guide` 的退出码 20 写作 `hard line`（决定 84）。
- **是否阻塞上线**：否。
- **处理**：已修（`8848a1600`、`dbc7d583e`）。按现行标题检查；改建待确认事项；跟上退出码说明。

### 16. `tests/e2e/inbound` `TestFlowCapture/ACT-CAP-06`、`TestFlowInboundPipeline/ACT-INP-10`

- **复现**：按上文"复现前的准备"，跑 `TestFlowCapture`、`TestFlowInboundPipeline`（`tests/e2e/inbound`） 所在包的整个用例。
- **现象**：`disposition = proposed，期望 ignored`；`起草请求的结构化输入里应有含 T2 的候选清单`
- **定性**：断言过期（模型替身串扰）。`29a5a0328` 起请求带 `active_proposals`（含早先步骤的标题），模型替身按子串首个命中。
- **是否阻塞上线**：否。
- **处理**：已修（`9e3a6aa95`）。匹配串改为只出现在本事件正文里的内容。

### 17. `tests/e2e/extractalign` `TestFlowExtractAlign/ACT-XAL-04`、`ACT-XAL-06`

- **复现**：按上文"复现前的准备"，跑 `TestFlowExtractAlign`（`tests/e2e/extractalign`） 所在包的整个用例。
- **现象**：`清单外项目应丢弃，实际 "prj_inbox"`；`没有等到：对话增量触发预筛请求`
- **定性**：断言过期。内置 Inbox 兜底（`13aa41570`）；预筛问句改写（`29a5a0328`）。
- **是否阻塞上线**：否。
- **处理**：已修（`4ce8b3fae`）。期望改为 `prj_inbox`；按问句开头认预筛请求。

### 18. 默认定时安排在用例运行中到点

- **用例**：`tests/e2e/followup`、`tests/e2e/ops` `TestFlowHealth` 的「长时运行与短任务各自排队」；`TestFlowGuidelineDraft/ACT-DRF-04`；`TestFlowRoutine/ACT-RTN-02`。
- **复现**：内核时钟（真实时间加测试时钟偏移）在用例运行中跨过工作日 08:30（本机时区，CI 为 UTC）时跑对应整个用例；run 45782997702、45783003732。
- **现象**：`长时运行 1 个在跑、2 个排队…（等待 1m0s 仍未出现）`；`没有新的运行 = 2，期望 1`；`MODEL_LOG 出现 routine 执行路径上的模型请求`（事项标题 `brief 2026-11-26 08:30`）。
- **定性**：用例隔离。开箱默认安排（简报 `30 8 * * 1-5`、复盘 `30 17 * * 5`，§7.3）在每个验收环境里启用，到点后多出任务型运行、简报事项与模型请求。把默认简报改到用例运行当中到点，三处全部复现（run 45787011945）；同样条件下停用默认安排后通过（run 45787097461）。
- **是否阻塞上线**：否。
- **处理**：已修（`ec4acf532`、`1db9ac5fb`）。`baseline.Env.IssueCredentials` 停用默认安排，要测定时安排的用例自己建实例；`TestDefaultSchedulesPaused` 拨过一周验证，再启用后反向验证。不经 `IssueCredentials` 建的环境仍保留默认安排。

### 19. `TestFlowDigestAlert/ACT-DGA-15` 读到未写全的运行记录

- **复现**：跑 `TestFlowDigestAlert`（`tests/e2e/automation`），偶发；run 45787097461。
- **现象**：`digestalert_test.go:377: 不是预期的 JSON: unexpected end of JSON input`
- **定性**：断言不稳（测试替身竞态）。替身 `accharness` 先建记录目录、后写 `materials.json`，用例看到目录就读文件。
- **是否阻塞上线**：否。
- **处理**：已修（`1db9ac5fb`）。输入写进点开头的临时目录后整体改名，`RunRecords` 跳过点开头的目录。

## 维护约定

- 新增条目必须有：用例路径与名称、复现命令、失败现象（关键断言原文）、定性（真 bug / 断言过期 / 环境问题）与依据、是否阻塞上线、处理状态与提交。
- 定性为断言过期或环境问题的，修掉测试或环境后把状态改成"已修"并保留一个发布周期，随后删除该条；真 bug 修好后同样删除。
- 不把"暂时不想修"写成条目；那要么修，要么在相应设计章节里改口径。
