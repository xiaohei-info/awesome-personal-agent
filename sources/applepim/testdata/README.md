# 录制样本

apple-pim 的 mail-cli、calendar-cli、reminder-cli 没有版本参数，样本是对真实输出（0.1.x 时期）去除个人内容后的最小保留：
字段名、嵌套与取值形态与实测一致，内容为占位。放宽对输出形态的假设前先在这里加样本。

- `messages.json` / `get-<n>.json`：`mail-cli messages --engine sqlite`、`mail-cli get --engine sqlite`
- `events.json`：`calendar-cli events`（一个重复实例、一个已取消、一个全天）
- `reminders-upcoming.json` / `reminders-overdue.json`：`reminder-cli items --filter upcoming|overdue`（日期可为纯日期、RFC3339 或不带时区的 "日期 时:分"）
- `messages-thread.json` / `get-mid-p<n>.json`：线程样本（原信、带 References 与 In-Reply-To 的回信、只带 In-Reply-To 的回信）。`get` 的 `message.references`、`message.inReplyTo` 字段是按设计合成的，不是 mail-cli 的实录；真实工具上的证实由本机升级验证 L11 承接（`MEAS-MAIL-THREAD`）。
