# 录制样本

apple-pim 的 mail-cli、calendar-cli、reminder-cli 没有版本参数，样本是对真实输出（0.1.x 时期）去除个人内容后的最小保留：
字段名、嵌套与取值形态与实测一致，内容为占位。放宽对输出形态的假设前先在这里加样本。

- `messages.json` / `get-<n>.json`：`mail-cli messages --engine sqlite`、`mail-cli get --engine sqlite`
- `events.json`：`calendar-cli events`（一个重复实例、一个已取消、一个全天）
- `reminders-upcoming.json` / `reminders-overdue.json`：`reminder-cli items --filter upcoming|overdue`（日期可为纯日期、RFC3339 或不带时区的 "日期 时:分"）
