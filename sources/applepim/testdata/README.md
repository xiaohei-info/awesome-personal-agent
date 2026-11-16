# 录制样本

apple-pim 的 mail-cli、calendar-cli、reminder-cli 没有版本参数，样本是对真实输出（0.1.x 时期）去除个人内容后的最小保留：
字段名、嵌套与取值形态与实测一致，内容为占位。放宽对输出形态的假设前先在这里加样本。

- `messages.json` / `get-<n>.json`：`mail-cli messages --engine sqlite`、`mail-cli get --engine sqlite`
- `events.json`：`calendar-cli events`（一个重复实例、一个已取消、一个全天）
- `reminders-upcoming.json` / `reminders-overdue.json`：`reminder-cli items --filter upcoming|overdue`（日期可为纯日期、RFC3339 或不带时区的 "日期 时:分"）
- `messages-thread.json` / `get-mid-p<n>.json`：线程样本（原信、带 References 与 In-Reply-To 的回信、只带 In-Reply-To 的回信）。`get` 的 `message.references`、`message.inReplyTo` 字段是按设计合成的，不是 mail-cli 的实录；真实工具上的证实由本机升级验证 L11 承接（`MEAS-MAIL-THREAD`）。
- `messages-long-html.json` / `get-long-html.json`、`messages-long-legacy.json` / `get-long-legacy.json`：长邮件样本（case `mail-long-html`、`mail-long-legacy`）。`get` 的 `message.headers`（`{name,value}` 数组）、`htmlContent`、`attachments`（`{name,mimeType,size,inline}`）与 `source`（原始邮件全文，工具只给原始邮件时由适配器用标准库解析）是按设计合成的，不是 mail-cli 的实录；缺哪一项适配器就把对应部分以 `partial` 自报。真实工具由本机升级验证 L15、L16 承接（`MEAS-RAW-MAIL`）。`mail-cli` 取回附件的方式未实测（`MEAS-RAW-ATTACH`），所以 `source.yaml` 不写 `content.attachments: fetch`，附件只有元数据。
- `rem/<case>/{all,upcoming,overdue}.json`：`rem-undated`、`rem-undated-renamed`、`rem-undated-dated`。`--filter=all` 是没有专门筛选时的取法（`MEAS-REM-UNDATED`），`creationDate` 字段名按合成；真实工具由 L23 承接。
