# v0.2.0 发布门槛 1、3、4 证据（GT-364）

基线：`main` @ `4ff6ab8e8`。环境：真实 sqlite3 3.53、python3 + pytest 9、go 1.26、已构建的 `web/dist`、完整历史的 git 检出；不使用 shim；本节点没有新增、过滤或注释任何 e2e 用例，仓库里原有的跳过与相对 v0.1.1 移除的测试见下文“v0.1 不回退的例外”（`himalaya` 的 PATH 隔离由 e2e 自身完成，GT-373）。

## 门槛 1：各组验收与 v0.1 不回退

| 命令 | 结果 |
|---|---|
| `npm --prefix web ci` / `run build` / `run test -- --run src/app` | 0 / 0 / 0（16 个测试通过） |
| `go build ./...` | 0 |
| `go test ./...` | 0（见下“已知波动”） |
| `go test -tags e2e -count=1 -timeout 30m ./tests/e2e/...` | 0，29 个包全部 ok，含 v0.1 不回退流程（ledger、decisions、mapping、sourcepull、webhook、materials、bgguards、agents、channel 等包） |
| `go run ./tools/redline` | `redline: ok` |
| `APA_CONFORMANCE_STRICT=1 go test ./tests/conformance/ ./cli/serve/` | 0：附录 B、C.14 规划标记已清零，`TestNoPlannedMarkersInStrictMode` 通过 |
| `go test ./tests/conformance/ ./cli/serve/` | 0 |

**已知波动（未掩盖）：** `ext/telegram` 的 `TestExtCheckPassesForTelegramExt`（`answer` 一步 30 秒超时）在整套 `go test ./...` 并行负载下于本机 3 次整套运行中失败 2 次、单包运行与第 3 次整套运行通过。2 核主机上的时序波动，非本节点范围；建议单独登记。早先几轮 e2e 中 `mapping`、`authz`、`bgguards`、`followup` 各有一次负载下失败，隔离重跑均通过，且最后一次完整 e2e 无失败。

## 门槛 3：§21.3 开发验收条目

| 条目 | 结论 |
|---|---|
| `SPIKE-LOGPROB` / `SPIKE-CLS-BATCH` | 开发验收部分（对话端点、分类端点替身验证换算、超时、超限、批量处置）已通过（`e2e/judge`、`e2e/judgment`）；真实端点部分归 L8 |
| `ACT-BKL` | 放开四步流程在合成夹具上通过（`e2e/backlog`）；起始值与带内比例归 L5 |
| `SPIKE-STRUCT` | 开发验收验证不符合 schema 的退化（§8.7）；真实端点归 L8、L10 |
| `SPIKE-SESSION-KIND` | 宿主替身与插件上报通过（`e2e/authz`、`e2e/hostplugin`）；真实宿主归 L9 |
| `SPIKE-ROUTINE` | `create_item` 与 `instruction_path` 写入通过（`tests/e2e` 中的例行任务流程）；`run` 模式归 L12 |
| `SPIKE-WEB-MOBILE`、`SPIKE-DONE-AUTHZ` | 文档已标已关闭/已证实；`e2e/workbench`、`e2e/authz` 通过 |
| `MEAS-HOST-CTX` | 替身与插件契约测试通过（`e2e/hostplugin`）；真实运行归 L9 |
| `MEAS-MAIL-*`、`MEAS-CAL-*` | 录制样本验收通过（`e2e/sourcesv2`）；`MEAS-MAIL-THREAD` 真实工具归 L11 |
| `MEAS-ALL-*`、首批宿主专项 | 文档已证实 |
| `MEAS-ADAPT-*`、`MEAS-H2-*` | 接入时验收，不在本版 |

## 门槛 4：缺陷账本

- `.omac/apa-v02.yaml`：35 条全部 `fixed`，inbox 与 pending 为空。
- `.omac/apa.yaml`：开始时 17 fixed、3 needs-owner、1 queued。经 `omac defect resolve` 关闭（均以已合并 PR 为据）：
  - `dfx-5dd4c0c9998f`（ACT-CAL-03）→ #201；替身版 `e2e/sourcesv2` 日历流程通过
  - `dfx-1699a21c5f70`（ACT-CAL-02）→ #200；同上
  - `dfx-5410a4c832ad`（ACT-WAK-01/04）→ #205；`e2e/hostplugin` 唤醒租约流程通过
  - `dfx-80defa39c0dd`（ACT-HCTX-09）→ #212；`e2e/hostplugin` 的 ACT-HCTX-09 通过
- 两条“已修复但修复节点 abandoned”的缺陷，逐条对应实际合入的修复 PR 与替身测试：
  - `dfx-27e69d9e5716`（ACT-RES-04，abandoned 节点 `fix.hermes-plugin-headless.27e69d`）→ #204（GT-209，按退出码与输出文本判定会话丢失）；替身测试 `tests/e2e/bgguards/resume_test.go` ACT-RES-04 通过。真实环境复核：L9（首批宿主的无头续接）。
  - `dfx-992917373ac0`（ACT-DLG-06，abandoned 节点 `fix.hermes-plugin-receipt.992917`）→ #224（GT-228，回执提取与补发）；替身测试 `tests/e2e/agents/delegate_test.go` ACT-DLG-06 通过。真实环境复核：L9（委派并等回执）。
- 另外四条经 `omac defect resolve` 关闭的缺陷，真实环境复核分别为：日历 ACT-CAL-02/03 → L11；唤醒租约 ACT-WAK-01/04、插件上下文 ACT-HCTX-09 → L9。
- 账本状态有可重复的检查：`go test ./tests/conformance/ -run TestDefectLedgersHaveNoOpenEntries` 读取两份 `.omac/*state/_dag.yaml`，任何非 fixed 且无人负责的条目、或 fixed 却没有修复节点与外部修复 PR 的条目都会使其失败。§21.3 的结论检查是 `TestRealMeasurementRowsHaveConclusionOrChecklistNumber`。

## v0.1 不回退的例外（相对 v0.1.1）

v0.2.0 把 v0.1 的判断引擎语义（置信度阈值、`escalate_to` 级联、follow_up 的 `do[]`）换成判断型能力包，设计依据是 `docs/appendix-c-capability-pack.md` C.7、C.11 与 `OMAC-NOTES` §2.4（`flow-agent-cascade`、`flow-follow-up` 改写）。据 `git ls-tree`/`grep '^func Test'` 比较 `v0.1.1` 与本基线的测试函数名，共 34 个按名称不再存在（下表按文件计数，有一个名称在两个文件里出现，合计 35 行；“覆盖”一栏是按设计文档对应关系的说明，未逐条回放）：

| 包 | 数量 | 说明 |
|---|---|---|
| `kernel/proactive/verify` | 7 | 回执核实的置信度与 yes/no 提示（C.7 改为门控题，由 `delivery-verify-gate` 与 `e2e/delivery` 覆盖） |
| `kernel/proactive/followup` | 6 | 旧 follow_up 的 remind/wait/abandon 置信度语义（由 `followup-ladder` 与 `e2e/followup` 覆盖） |
| `tests/packs/follow-up` | 5 | 同上，包测试 |
| `kernel/inbound` | 5 | 旧入站判断与级联（由入站六步与 `e2e/inbound`、`e2e/screening` 覆盖） |
| `kernel/agents/direct` | 4 | 旧 `escalate_to` 级联（由 `cascade-engine` 与 `e2e/judge` 覆盖） |
| `tests/packs/inbound-triage` | 2 | 旧置信度级联（由 `e2e/inboundrules`、`e2e/judge` 覆盖） |
| 其余各 1 | 6 | `api/health`、`cli/serve`、`kernel/attention`、`kernel/screening`、`tests/e2e/followup`（`TestFlowFollowUpModelDown`）、`tests/packs/delegation-verify`，分别被健康视图十三项、能力包 v2 装配、判断型流程取代 |

仓库里原有的 `t.Skip`（本节点未新增）：

| 位置 | 原因 | 覆盖 |
|---|---|---|
| `tests/e2e/agents/cascade_test.go:62`、`:104` | 无条件跳过：v0.1 判断引擎语义已被 C.7/C.11 移除 | `e2e/judge`、`e2e/inbound` 的新级联流程 |
| `tests/e2e/bgguards/allowlist_test.go:252` | 无条件跳过：同上，ACT-FWD-07 与 `tests/e2e/channel/fwd_test.go:230` 重复 | `e2e/channel` |
| `tests/e2e/workbench/browser_test.go:23` | 条件跳过：本机无 ego-browser，页面渲染与手机宽度走查 | 本机升级验证 L10 |
| `tests/e2e/agents/outcall_test.go:332` | 条件跳过：重放时没有排队的定时拉取 | 同包其余外呼用例 |
| `tests/e2e/judgment/judgment_test.go:537`、`:566` | 条件跳过：抽检池不足 | L5/L10 的真实积压 |

## 门槛 5（不作为收口条件）：本机升级验证清单

L1 升级预检只读；L2 升级本身；L3 回退；L4 外发范围与登录；L5 积压放开与不确定带起始值；L6 补救旧级联误丢的事件；L7 第一档只有内核；L8 第二档加模型端点；L9 第三档加首批宿主；L10 接入后的一个工作日；L11 续保；L12 例行任务与远程访问。通过标准见 `docs/solution-design.md` §20.4。
