# SRC_STUB：桩来源脚本

仅用于测试与验收，不进入发布构建。不连任何真实账号。

```sh
go build -o "$SRC_STUB" ./tests/doubles/srcstub/cmd/src-stub
SRC_CASE=mail-like "$SRC_STUB" < pull.json                 # 标准事件 JSONL，末行游标
echo '{"event_id":"e","attachment":{"ref":"att-0"}}' | "$SRC_STUB" fetch   # 回固定字节
```

- case 取自 `$SRC_CASE`（case 名；或指向文件：内容为 case 名，或以 `{` 开头的原样 JSONL，供追加）；缺省读可执行文件同目录的 `case`。
- 内置 case：`big-body`（正文 204800 字节）、`big-body-reply`、`big-body-update`（同关联键 `thread-big`）、`ten-big`（10×200 KiB）、`mail-like`（headers/attachments 带 ref/original HTML；`SRC_IMG_URL` 改 `<img src>`）、`chat-two-things`、`chat-long`（60×100 字符）、`too-many-msgs`（201 条）、`oversize-mid`（第 2 条约 1.5 MiB，`SRC_OVERSIZE_BYTES` 可改）、`partial`（missing: original、attachments）、`mid-body`（3000 字符）、`digest-30k`（2×30000 字符）。
- fetch 行为取自 `$SRC_FETCH` 或同目录 `fetch` 文件：空/`ok` 固定 4096 字节（由 ref 决定）；`fail[:码]` 非零退出码加 stderr 一句原因；`big` 输出 33 MiB；`size:N`。
- 接入声明：`source.yaml`（`content.attachments: fetch`）与 `source-nofetch.yaml`（无 `content` 段）。
- 注：`chat-*`/`too-many-msgs` 的消息数组用 §9.2 的 `items` 字段（Web 里显示为 `messages`）。
