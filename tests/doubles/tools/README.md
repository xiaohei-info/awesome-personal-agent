# TOOL_STUB：来源工具录制样本替身

仅用于测试与验收，不进入发布构建。不连任何真实账号。

```sh
go build -o /tmp/tool-stub ./tests/doubles/tools/cmd/tool-stub
/tmp/tool-stub install "$TOOL_STUB_BIN" "$(pwd)"   # 以 himalaya gog mail-cli calendar-cli reminder-cli gh 的名字复制自己，并记下仓库根
/tmp/tool-stub case "$TOOL_STUB_BIN" mail-thread    # 写 $TOOL_STUB_BIN/case；也可直接 echo
```

- case 取自 `$TOOL_STUB_BIN/case`（不依赖环境变量穿过子进程白名单；该文件缺失时才读 `TOOL_STUB_CASE`）。
- 每次调用向 `$TOOL_LOG` 追加一行 JSON：`tool`、`case`、`argv`、环境变量**名**（不记值）。`TOOL_LOG` 不在子进程环境里时写 `$TOOL_STUB_BIN/tool.log`。
- 退出码：工具自身的 0/1；替身自己的 `99` 无 case 或未知 case，`98` 该 case 未录制此工具，`64` 非只读命令（gog create、gh auth login 等）。

| 工具 | case |
|---|---|
| himalaya | `mail-1.1.0` `mail-2.0.0`（回放 `sources/mail/testdata/<版本>`）、`mail-1.1.0-new`（加 `<mid-7@example.com>`）、`mail-thread-1/-2/mail-thread`（`<mid-t1..t3@example.com>`，按 `-H` 返回 References/In-Reply-To）、`mail-old-reply`/`mail-old-reply-2`（`<mid-o2/o3@example.com>`）、`mail-version-0.1`、`mail-auth-fail`（stderr `Invalid credentials`，退出 1）、`mail-garbled`（无 Message-ID 行）、`mail-garbled-json`（非 JSON） |
| gog | `cal-base`（`sources/calendar/testdata/0.9.0` 原样）、`cal-updated`、`cal-updated-2`、`cal-new`（派生规则见验收 ACT-CAL）、`cal-version-0.1`、`cal-auth-fail` |
| mail-cli / calendar-cli / reminder-cli | `pim-mail`、`pim-calendar`、`pim-reminders`（回放 `sources/applepim/testdata`）、`pim-mail-thread-1`、`pim-mail-thread` |
| gh | `gh-empty`、`gh-issue-opened`、`gh-issue-labeled`、`gh-issue-opened-2`、`gh-issue-opened-3`、`gh-pr-closed`、`gh-pr-updated-7`、`gh-agentsmd-changed`（录制在 `testdata/gh/`，仓库 `acc-org/acc-repo`）、`gh-auth-401`（stderr `Bad credentials (HTTP 401)`） |

已知限制：
- gh 样本的时间固定在 2026-11-10（09:00–14:00Z）；来源的 `window.to` 必须不早于对应时刻事件才会被读到（测试时钟推进即可）。
- `pim-mail-thread*` 的 `references`/`inReplyTo` 字段名是假设（`MEAS-MAIL-THREAD` 尚未实测），真实输出形态确认后改这里。
- 渠道侧不在这里实现：直接用 `ext/telegram/fakebot`。
