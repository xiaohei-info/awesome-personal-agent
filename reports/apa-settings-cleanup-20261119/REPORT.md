# APA 设置页遗留收口

基线：`feat/settings-cleanup2026`，`38609a3d5`。只改工作区内的 Web 代码、测试、相关设计与验收清单；没有部署、重启实际实例、访问真实实例数据、修改 `~/.apa`、提交或推送。

## 1. 独立设置页下线，导出与转发密钥保留

**原设计**：基线中的 `docs/solution-design.md:3313`（§20.3 W25.4）要求随内置输出一起下线「扩展程序」页，旧地址改写到「导出与转发」配置页，已签发密钥可在新页查看、吊销；替代方案包括输出接口、推送器、Markdown 目录导出和**出站** webhook 转发插件，不是新增一个提醒渠道，也不是入站 webhook 接收密钥。原设计明确规定内置输出未就绪前不单独删页。核查时，附录 D.1 仍把两种输出列为独立程序（`docs/appendix-d-adapters.md:13–14`），当前只注册了一个内置推送渠道（`cli/serve/assemble.go:144`）；附录 B.2 列的是仅 Web 会话可用的密钥 API（`docs/appendix-b-api-cli.md:213,234`），不是替代页面。旧功能的注释和测试也只指向导出与转发密钥管理。

**本次取舍**：按任务要求下线独立页，先同步 §4.1、§20.3 和决定 53（`docs/solution-design.md:520,528,3315,3452`）；W25.4 内置输出仍按既定方案，本次不实现。现在从 **设置 › 连接 › 提醒渠道 › 导出与转发密钥** 进入（`/settings/channels`）。与自写程序接入归在同一页，便于找对外连接的密钥；单独折叠、不混列提醒渠道的密钥，也不误归到 Agent 身份或权限。桌面、手机设置导航共用分组，移除旧项见 `web/src/app/registry.ts:33–36`；新折叠区见 `web/src/features/settings-channels/SettingsChannelsPage.tsx:70–73`，中英文案见 `web/src/features/settings-channels/strings.ts:12,129`。

**兼容性与验证**：保留已有导出和转发密钥的列表、签发（含原有权限范围与备注）和吊销；明文仅在签发后显示，关闭提示或收起区块即清除（`web/src/features/settings-channels/OutputKeys.tsx:14–55,62–95`）。原 `GET`／`POST`／`DELETE` API、权限和存储不变（`api/settings/credentials/credentials.go:212–214`），既有密钥不用重签，不需迁移。按任务要求彻底移除旧 Web 路由，不保留别名；旧书签需换为 `/settings/channels`（`docs/appendix-b-api-cli.md:514`）。删除旧路径仍能打开独立页的断言，因为它不再是有效入口；替换为提醒渠道寻址和设置菜单断言（`web/src/app/app.test.tsx:207,309–316`）。原密钥的 5 个行为用例迁到 `web/src/features/settings-channels/output-keys.test.tsx:59–135`，补了从设置首页进入和收起后不再显示明文的覆盖；旧目录的测试路径、节点、门禁与产物引用同步迁移（`.omac/apa.yaml:2438–2490`）。

## 2. 运行日志：两个字段已实现，无需改动

| 核查点 | 证据 |
| --- | --- |
| 页面位置 | `web/src/features/runs/index.tsx:4` 注册「诊断 › 运行日志」，路径 `/runs`；不在设置目录。 |
| 数据来源 | `web/src/features/runs/RunsPage.tsx:14–19,128–135` 读取 `GET /runs`，客户端统一前缀 `/v1`（`web/src/app/api.ts:44`）；`api/runs/runs.go:130–156` 从 `proc.List` 读取并返回 `Run`，`kernel/domain/entities.go:239–244` 的 JSON 字段就是 `runner` 和 `launch`。后台运行写入这两项（`kernel/background/background.go:449–452`）。 |
| 默认展示 | 点日志行展开，有 Agent 名称的记录显示 `runner` 为「主角色」（`main_session`）或「普通任务」（`task`），`launch` 为「已有对话」（`wake`）或「独立会话」（`headless`），见 `web/src/features/runs/RunsPage.tsx:71–79` 和 `web/src/features/runs/strings.ts:6`。 |
| 历史记录及原始值 | 没有 Agent 名称的旧记录不显示上述摘要行；打开「设置 › 通用 › 显示原始值」后，展开详情仍能看到记录中实际存在的 `runner`、`launch`（`web/src/features/runs/RunsPage.tsx:90`）。来源等不适用的记录不创造这两个值（`docs/solution-design.md:979`）。 |

结论：**不是缺字段**，列表展开后已有等价表达；原始键也已接通。不改日志列或「类型／只看失败」筛选。

## 3. 笔记库：用一句话解释用途

| 位置 | 改动前 | 改动后 |
| --- | --- | --- |
| 设置 › 笔记库页首 | 登记文档所在的库，点链接回到笔记应用。 | 把库里的文档挂到事项上，点链接回笔记应用读原文。 |

APA 对每个登记库的用法相同，用页首的一句话说明用途，不用每库重复填写、不增加数据字段。原来的「打开方式」不变。中英文案见 `web/src/features/settings-vaults/strings.ts:9,34`，页面展示见 `web/src/features/settings-vaults/SettingsVaultsPage.tsx:51`，空列表也显示说明的测试见 `web/src/features/settings-vaults/settings-vaults.test.tsx:43–46`；设计同步见 `docs/solution-design.md:554`。

## 4. 全局残留检查

```sh
rg --hidden -n -i '(/settings-credentials\b|web-settings-credentials|SettingsCredentialsPage|settings-credentials\.test|title: "Extensions"|给导出、转发程序的钥匙|导出与转发用的 ext 密钥|设置.{0,12}[「“"]扩展程序)' \
  -g '!.git' -g '!BRIEF-LEFTOVERS.md' -g '!reports/apa-settings-cleanup-20261119/REPORT.md'
rg --hidden -n '扩展程序|/settings/credentials\b' \
  -g '!.git' -g '!BRIEF-LEFTOVERS.md' -g '!reports/apa-settings-cleanup-20261119/REPORT.md'
```

- 第一条：**零命中**（`rg` 退出码 `1`）。包含隐藏的 `.omac` 清单；排除任务书和本报告本身，忽略依赖、生成物和 Git 历史。
- 第二条：仅有 ext 的类型词条（`web/src/app/vocab.ts:115,178`：事件来源和凭据种类，不是导航文案）及服务器包 `api/settings/credentials` 的 Go 导入、测试、验收清单和历史测试证据。保留这些引用不会产生旧页入口。
- 另查构建产物：`rg -n '(/settings-credentials\b|web-settings-credentials|SettingsCredentialsPage|settings-credentials\.test|给导出、转发程序的钥匙)' web/dist`，零命中。

## 5. 其它遗留项（只报告，不改）

| 遗留项 | 现状 |
| --- | --- |
| 旧「后台权限」`/settings/access`、旧「宿主接入」`/settings-exec` | 都是跳到 `/settings/agents` 的兼容地址，不是独立页或死链接（`web/src/features/settings-agents/index.tsx:8–12`，`settings-agents.test.tsx:255`）。 |
| 旧「临时授权」`/settings/grants` | 兼容地址；链接带授权单锚点时能打开相应记录。Agent 页底部「以前的临时授权」可查看并撤销旧单，并不是空页（`web/src/features/settings-agents/settings-agents.test.tsx:243–251`，`SettingsAgentsPage.tsx:489–538`）。 |
| 旧验收命令（不在 Web 导航） | `.omac/apa.yaml:3233,3248,15138,15155` 仍指向已移除的 `settings-exec` 和 `settings-grants` 测试目录，对应功能已归到 `settings-agents`。本次不改这些独立验收项。 |

核对页面注册、设置分组、页面内链接和窄屏返回入口（`web/src/app/Shell.tsx:150–155,178–193,246–264`、`SettingsHome.tsx:28–43`）：未发现其他在当前设置菜单中可点击却落空的页面，也未发现标为「旧版」的独立导航项。

## 6. 构建与测试

仓库使用 `web/package-lock.json` 和 npm，按 CI 的方式安装依赖并构建，前端测试运行全量而不仅是 `src/app`。

```text
$ npm --prefix web ci
added 334 packages in 2s

$ npm --prefix web run test -- --run
 ✓ src/features/runs/runs.test.tsx (8 tests) 1211ms
 ✓ src/features/settings-channels/output-keys.test.tsx (6 tests) 760ms
 ✓ src/features/settings-vaults/settings-vaults.test.tsx (8 tests) 1001ms
 Test Files  50 passed (50)
      Tests  576 passed (576)
   Duration  14.11s

$ npm --prefix web run build
✓ 441 modules transformed.
dist/index.html                   0.39 kB │ gzip:   0.28 kB
dist/assets/index-D3AhdqY6.css   32.07 kB │ gzip:   7.57 kB
dist/assets/index-CQinGUcF.js   753.14 kB │ gzip: 248.13 kB
✓ built in 1.51s

$ go build ./...
（无输出，退出码 0）

$ go run ./tools/redline
redline: ok

$ .github/e2e/pkgs.sh check && git diff --check
（无输出，退出码 0）
```

前端构建有 Vite 包体积提示；测试有 React Router 未来行为提示，均不阻断。与 Go 验证并行时，未改动的来源页用例有一次等待超时；单独重跑全量通过。未进行真实浏览器、测试标签下的冒烟 e2e 或实机验收。

macOS 上 `go test ./...` 仅 `TestPromptHangChildStaysAliveAndNeverReceipts` 失败，原因是该用例读取 Linux 的 `/proc/<pid>/stat`（`tests/doubles/stubharness/stubharness_test.go:222–225`），当前平台没有此路径。不跳过测试，在无网络、独立 `APA_HOME` 的 Linux 容器用同版本 Go 复验；该用例单独重跑通过：

```text
=== RUN   TestPromptHangChildStaysAliveAndNeverReceipts
--- PASS: TestPromptHangChildStaysAliveAndNeverReceipts (0.61s)
PASS
ok   github.com/xiaohei-info/awesome-personal-agent/tests/doubles/stubharness  0.766s
```

Linux 首次默认并行跑全量时，三个未改动的后端用例失败：`cli/serve:TestBacklogRestoreOnUpgradedFixture`（替身模型不可用）、`kernel/inbound:TestCaptureRejudgedAfterProjectScopeChosen`（事件仍待处理）、`kernel/webhook:TestTransformLargeOutput`（等待转换超时）。在同一隔离环境用内存临时目录、清除代理变量并限制并行度后，**Go 全量测试通过**，未改动或跳过上述用例：

```text
$ go version
 go version go1.26.5 linux/arm64
$ GOMAXPROCS=4 go test -p 4 ./...
 ok   github.com/xiaohei-info/awesome-personal-agent/cli/serve                 53.730s
 ok   github.com/xiaohei-info/awesome-personal-agent/kernel/inbound            13.515s
 ok   github.com/xiaohei-info/awesome-personal-agent/kernel/webhook             2.754s
 ok   github.com/xiaohei-info/awesome-personal-agent/tests/conformance          1.184s
 ok   github.com/xiaohei-info/awesome-personal-agent/tests/doubles/stubharness  1.400s
 ok   github.com/xiaohei-info/awesome-personal-agent/tools/release              44.793s
 退出码 0
```
