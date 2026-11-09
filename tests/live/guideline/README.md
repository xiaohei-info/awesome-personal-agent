# tests/live/guideline

`flow-agents-md`、`flow-no-repo-guideline`、`flow-guideline-draft` 的 live 验收（ACT-AGM-01..10、ACT-NRG-01..03、ACT-DRF-01..04）。

```
set -a; . /opt/apa-live/creds/live.env; set +a
go test -tags=live -count=1 -timeout=120m ./tests/live/guideline/...
```

前置（taiyi，见 `/opt/apa-live/README.md`）：

- `web/dist` 已构建（`npm --prefix web ci && npm --prefix web run build`）。
- 真实 Hermes：测试自动选 venv 里的 `hermes`（`APA_LIVE_HERMES_BIN` 若指向把 `HERMES_HOME` 固定为共享目录的 `bin/hermes` 包装脚本，会被换成 `../venv/bin/hermes`，否则隔离的 HERMES_HOME 与 apa 插件不生效）。种子目录 `APA_LIVE_HERMES_SEED`（缺省 `/opt/apa-live/hermes-home`，只复制 `config.yaml` 与 `.env`）。
- `APA_E2E_TEST_REPO`：专用可写测试仓库（缺省 `https://github.com/xiaohei-info/apa-live-test.git`）。ACT-AGM-09 会向它的默认分支推一个 AGENTS.md 提交，测完撤回；`git` 用本机已登录的 `gh` 作凭据助手，`github-source` 的凭据只放在来源自己的目录。
- 其余仓库（ACT-AGM-08、ACT-DRF）用本机 bare 仓库作远程。
- 缺规范检测周期由数据目录下的同名覆盖包改为 1 分钟（附录 C.1）；Web 上只把规则切 auto。
- `APA_LIVE_EVIDENCE_DIR`：证据输出目录（令牌已脱敏）；`APA_LIVE_DRAFT_WAIT`：Hermes 起草的等待上限（缺省 20m）。

Web 步骤走 Web 页面调用的同一套 HTTP 接口（已登录会话 + CSRF），与 `tests/live/{injection,github,mail}` 一致；taiyi 上没有 ego-browser。
