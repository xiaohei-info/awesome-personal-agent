# tests/live/hermes

`flow-hermes-context`、`flow-chat-answer`、`flow-delegate-recover`、`flow-capture` 的 live 验收（ACT-HCTX/CHAT/DLG/CAP）。

```
go test -tags=live -count=1 -timeout=120m ./tests/live/hermes/...
```

前置（taiyi 隔离环境，见该机 `/opt/apa-live/README.md`；不要在 Mac mini 上跑）：

- `web/dist` 已构建（`npm --prefix web ci && npm --prefix web run build`）。
- `APA_LIVE_HERMES_BIN`（缺省 `/opt/apa-live/venv/bin/hermes`）、`APA_LIVE_HERMES_SRC`（缺省 `/opt/apa-live/src/hermes-agent`）、
  `APA_LIVE_HERMES_SEED`（只复制其中的 `config.yaml` 与 `.env`，缺省 `/opt/apa-live/hermes-home`）。
- `APA_LIVE_EVIDENCE_DIR`：设置时写入各会话的问答/工具调用与各流程的 `*-evidence.json`（令牌已脱敏）。

真实：`apa` 二进制、内核、SQLite 账本、Web 接口、Hermes（真实模型端点）、`plugins/hermes` 的 core/inject/tools/receipt/capture/headless 插件。
替身只有 `MODEL_STUB`（内核自己的判断型/生成型模型调用，验收文档要求它记录并脚本化回复）。

`driver.py` 在一个常驻进程里按 `hermes chat` 同样的方式构造 `AIAgent`（真实配置、真实插件加载与钩子），
让同一会话跑多轮——ACT-HCTX-04 的“同一会话里下一条消息带增量”需要这个。`apa` 垫片（PATH 最前）只记录调用，不改结果。

`APA_LIVE_CONTINUE=1`：失败后继续跑后续独立步骤，仅用于取全证据。运行差异与已知缺口见 `FINDINGS.md`。

所有状态（APA_HOME、HERMES_HOME、垫片日志）都在临时目录，测后清理；不读写 `~/.hermes`。
