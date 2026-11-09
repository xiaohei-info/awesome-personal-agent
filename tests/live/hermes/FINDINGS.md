# GT-200 live 运行说明（taiyi，真实 Hermes）

命令：`go test -tags=live -count=1 -timeout=120m ./tests/live/hermes/...`（结果见交付说明与 `evidence/`）。
`APA_LIVE_CONTINUE=1` 仅用于取全证据（失败后继续跑后面的独立步骤，整体仍失败）；正式验收不设置，严格按“前一步未通过后续不得开始”。

## 与验收文字的差异（以设计 §14.5、附录 B.1 为准）
- 代答（ACT-CHAT-03）：`apa_decide` 工具带 `--evidence` 原话并设置 `APA_SESSION`；回执（ACT-DLG-04/06）带结果证据 `--pr/--link`，done 不带本人原话。
- ACT-CHAT-03 的“可一键撤销”：Web 的撤销接口（`POST /v1/items/{id}/revert`）作用于该次代答带来的事项变更；断言以该接口实际行为为准（见测试）。
- ACT-HCTX-10：内核指令版本为 1，CLI 不接受负数、0 表示不比对，造不出“更小的版本”，用版本 2 触发比对（内核按“不等于当前”判过期）。
- ACT-HCTX-11：已走真实 `hermes chat --in <检出目录> --resume <session_id>`（证据 act-hctx-11-resume-cli.txt 与 ACT-HCTX-11-resume-hook-call：钩子工作目录为检出目录，注入非空）。上一版的“钩子 cwd 为安装目录”缺陷已由主干 #237 修复。压缩走 Hermes 自己的 `_compress_context`（触发 post_compaction），需足够长的历史才会真实缩短，所以先发 30 轮带填充的对话。driver 在建 agent 前用 `set_session_cwd` 冻结会话目录，与网关对客户端会话的做法一致。

- ACT-DLG-03：项目信息、repos 来自 `--scope project`（设计 §7.6）；`--scope item` 只含事项条目，也没有验收文字里的“AGENTS.md 指引”。
- ACT-DLG-05：回执证据（PR 链接）不在注入信封里，`apa item show` 的 messages 也为空，只在 `delegation.reported` 事件里（`apa why <子事项>` 可见）；
  所以 agent 不会自发找到它，追问时明确指到 `apa why`（第一次回答是否已带证据记在 `ACT-DLG-05.first_answer_had_evidence`）。这是产品观察：若希望主会话“能说出证据”，注入或 `item show` 需要带上它。
- Web 步骤：taiyi 无 ego-browser，直接调用页面所用的 `/v1` 接口（Web 会话 + CSRF），未驱动浏览器。
- 提示词引导（测试选择，不是产品行为）：模型在终端里直接跑 `apa decide` 时没有 `APA_SESSION`，内核按第二档返回 23；
  所以 CHAT 流程在开场消息里要求“操作账本用 apa_* 工具”；DLG-06 的消息明确子事项 id 与命令格式（命令须以执行者脚本开头，插件按首词识别）。
  观察：agent 在终端里跑的 `apa` 没有会话身份，审计只到 token 粒度。

- ACT-HCTX-06：Web 点击与 Web 回答后投递状态为 acknowledged（#243 统一走 Confirm 后修复）。HTTP 不暴露已读投递的状态，测试只读打开 `ledger.db` 取证（证据 `ACT-HCTX-06-state-after-click`、`ACT-HCTX-06-state-after-web-answer`）。
- ACT-DLG-03：已在 acc-work 用 `apa material commit` 登记自有型材料（标题 DLG3-MAT-QZK…，正文含 MARKER-BODY-DLG3），断言项目上下文含标题、不含正文；材料与委派事项未建立关系（走项目作用域）。
