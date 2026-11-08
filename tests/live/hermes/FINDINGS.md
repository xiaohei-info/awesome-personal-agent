# GT-200 live 运行发现（taiyi，真实 Hermes，取全证据一次完整运行）

4 个流程共 31 步：28 步通过，3 步因上游产品缺陷失败（`go test -tags=live ./tests/live/hermes/...` 退出码 1）。
取全证据用 `APA_LIVE_CONTINUE=1`（失败后继续跑各自独立的后续步骤，整体仍失败）；不设置时严格按“前一步未通过后续不得开始”。

| 流程 | 结果 |
|---|---|
| flow-hermes-context | 10/11 通过；失败 ACT-HCTX-09 |
| flow-chat-answer | 3/4 通过；失败 ACT-CHAT-03（代答本身成功，失败在“一键撤销”） |
| flow-delegate-recover | 10/11 通过；失败 ACT-DLG-06 |
| flow-capture | 全部通过（插件凭据用 WRK；用 DEL 见下） |

## 失败 1：ACT-HCTX-09 会话身份透传到审计
- `apa_propose` 等工具（propose/ask/delegate/report/declare）没有 `evidence` 参数；内核 `authz.needEvidence`（#166）要求 delegate 的代理写带本人原话，
  用 DEL 调用回 exit 2「delegate actions must carry on_behalf_of and the owner's own words as evidence」。只有 decide/accept/dismiss 工具有 evidence。
- 换 WRK 后工具成功，Event 的 actor 有 `session`，但 `harness` 为空：`cli/client` 只发 `X-APA-Session`，没有任何地方发 `X-APA-Harness`
  （`api/core.authenticate` 读它）。验收要求 actor 同时含 harness 和 session。

## 失败 2：ACT-CHAT-03 聊天代答可一键撤销
- 代答成功：`apa decide <id> --option B --evidence 就选方案 B`，`resolved_via=chat`，Event 带 `on_behalf_of=owner` 与原话。
- 但代答只产生 `decision.resolved` 事件（无 item.updated/phase_changed）；`POST /v1/items/{id}/revert` 对它回 400
  「only item updates and phase changes can be reverted」（`kernel/items/commands.go planRevert`）。设计 §8.6/§12 要求 Web“agent 变更”可一键撤销经聊天代答的决定。

## 失败 3：ACT-DLG-06 插件捕获回执
- 真实 Hermes 经 `apa_executor_call` 取得调用方式并在 terminal 里执行执行者后，apa-receipt 确实触发，运行 `apa report <id> --outcome done --summary …`（无 evidence、无 PR/commit/link）。
- 内核拒绝：DEL 缺本人原话 evidence；即使凭据对了，「delegation 进入 done 必须带证据」也会拒绝。插件把输出丢进 /dev/null，回执被静默吞掉，子事项一直 open。
- 同类：apa-capture 按安装步骤配 DEL 时 `POST /v1/capture` 回 400（同一条 evidence 规则；探测证据见 `evidence/flow-capture-evidence.json` 的 `ACT-CAP-00-del-probe`）。
  本流程改用 WRK 作插件凭据后 flow-capture 全部通过。

## 其他观察（未作失败）
- agent 在终端里直接跑 `apa …`（而不是用 apa_* 工具）时没有 `APA_SESSION`，审计只到 token 粒度。
- ACT-HCTX-10：内核指令版本为 1，CLI 不接受负数、0 表示不比对，造不出“更小的版本”，用版本 2 触发比对（内核按“不等于当前”判过期）。
- 直接用 `hermes chat --resume` CLI 时，钩子子进程的工作目录是 Hermes 安装目录而不是检出目录，`--scope cwd` 解析不到项目；
  ACT-HCTX-11 的恢复因此走进程内同一加载路径（`DRIVER_RESUME`，与 `--resume` 同一 `_load_resume_target`），压缩用 Hermes 自己的 `fire_post_compaction` 事件。
- ACT-DLG-03：项目信息/repos 来自 `--scope project`（设计 §7.6）；`--scope item` 只含事项条目，也没有验收文字里的“AGENTS.md 指引”。
- ACT-DLG-05：证据（PR 链接）不在注入信封里，agent 需要自己查账本；偶尔第一次回答没带证据，追问“证据是什么？”后给出（记在 `ACT-DLG-05.first_answer_had_evidence`）。
- taiyi 无 ego-browser：Web 步骤直接调用页面所用的 `/v1` 接口（Web 会话 + CSRF），未驱动浏览器。
