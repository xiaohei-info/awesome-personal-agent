# tests/live/injection

`flow-data-not-instructions` 的 live 验收。

```
go test -tags=live -count=1 -timeout=120m ./tests/live/injection/...
```

前置（taiyi 隔离环境，见该机 `/opt/apa-live/README.md`）：

- `web/dist` 已构建（`npm --prefix web ci && npm --prefix web run build`）。
- `APA_LIVE_HERMES_BIN`：真实 hermes 可执行文件（缺省 `/opt/apa-live/venv/bin/hermes`）。
- `APA_LIVE_HERMES_SEED`：只读种子目录，只复制其中的 `config.yaml` 与 `.env` 到本次临时 HERMES_HOME（缺省 `/opt/apa-live/hermes-home`）。
- `APA_LIVE_EVIDENCE_DIR`：证据输出目录（设置时写 `injection-evidence.json` 与各次 Hermes 会话输出，令牌已脱敏）。

所有状态（APA_HOME、HERMES_HOME）都在 `os.MkdirTemp` 的临时目录，测后删除；不读写 `~/.hermes`。
