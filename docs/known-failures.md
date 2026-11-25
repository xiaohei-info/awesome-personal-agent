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
| `tests/e2e/workbench` `TestFlowWebWorkbench/ACT-WBN-16` | 前两处断言过期（已修），余下为真 bug | 否 | 部分已修（`8d525e43`）；360 像素溢出待修 |

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

**6c. 360 像素下今天页与事项页横向溢出（真 bug，待修）**：`页面 today 的 scrollWidth = 401，超出 360`、`页面 items 的 scrollWidth = 401`，`这些页面在 360 宽度下有控件超出视口: [today(1 个控件超出视口) items(12 个控件超出视口)]`。实测撑宽的是行内操作按钮组：`.act-row > .actions` 里五个按钮（排时间 69 + 交给 Agent 97 + 完成 55 + 记进展 69 + 更多 55，加 4 个 6px 间距共 369px），而 360 宽度下 `main` 左右各 16px 内边距只剩 328px。

  - **根因**：`web/src/app/styles.css` 的手机断点（`@media (max-width: 820px)`）里 `.act-row > .actions { flex-wrap: nowrap }`、按钮 `white-space: nowrap`，注释写的是"行上四个快捷操作……占满一行并排放下，不收进「更多」"。`RowActions` 后来加了第五个按钮「更多」（`web/src/features/items/RowActions.tsx`），四个变五个后一行放不下，溢出的 41px 把整页撑宽。今天页（`TodayRow`）与事项列表共用这套行操作，所以两页都超。
  - **影响面**：360 像素宽度下今天页、事项列表出现横向滚动，与界面设计 §4.1/§web-ui 的"全部页面 360 宽度可读可操作、无横向滚动"不符；桌面与平板宽度不受影响，触控操作本身可用。
  - **最小修法建议**：在窄断点（例如 `@media (max-width: 480px)`）把 `.act-row > .actions` 改成 `flex-wrap: wrap`，或把「更多」收进收起菜单／图标，并把 `styles.css` 的注释一起改。属界面口径，需先确认手机上五个动作怎么排，不要直接改 820px 断点影响平板。
  - **是否阻塞上线**：否。不影响数据、安全与桌面主路径，冒烟清单里这条步骤（`workbench` 包的浏览器走查）在本机与 CI 无 `ego-browser` 时本来也会跳过。
  - **处理**：已记录不修（代码不动）。状态标"仍失败（已知）"。

## 维护约定

- 新增条目必须有：用例路径与名称、复现命令、失败现象（关键断言原文）、定性（真 bug / 断言过期 / 环境问题）与依据、是否阻塞上线、处理状态与提交。
- 定性为断言过期或环境问题的，修掉测试或环境后把状态改成"已修"并保留一个发布周期，随后删除该条；真 bug 修好后同样删除。
- 不把"暂时不想修"写成条目；那要么修，要么在相应设计章节里改口径。
