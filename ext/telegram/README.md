# Telegram 渠道 ext

APA 自己的独立进程（设计 §10.3）：长轮询领取投递，以投递 id 为幂等键发到 Telegram，回报送达，把本人在 Telegram 里的回答转达给内核。只经 HTTP 接入内核，不直接写存储，不复用也不依赖任何 harness 的网关，不监听端口（不需要公网地址）。

## 运行

```bash
APA_URL=http://127.0.0.1:<端口> APA_EXT_KEY=<Web 签发的渠道 ext 密钥> \
TG_BOT=<Bot token> TG_CHAT=<本人会话 id> apa ext run telegram
```

| 环境变量 | 含义 |
|---|---|
| `APA_URL`、`APA_EXT_KEY` | 内核地址、本渠道的 ext 密钥（必填） |
| `TG_BOT`、`TG_CHAT` | Bot token、本人的会话 id（必填）。ext 只向这个会话发送，只接受它里面的回复 |
| `TG_API` | Bot API 地址，缺省官方 |
| `APA_WEB_URL` | 内核给相对 Web 链接时的前缀，缺省 `APA_URL` |
| `APA_EXT_STATE` | 发送记录文件，缺省 `$XDG_STATE_HOME/apa/ext-telegram.json` |

本人的 harness 也连着 Telegram 时，`TG_BOT` 要用另一个 Bot：同一个 Bot 不能同时有多个长轮询消费者。回答人白名单（Telegram 用户名，没有用户名时是数字 id）在 Web 设置，由内核校验。

## 行为

- **幂等发送**：发出后立即把"投递 id → 消息"写入状态文件（临时文件 + fsync + 改名）。回执丢失、进程被杀、租约到期重领时，查到记录就只补报 ack（同一个 `external_ref`，形如 `<会话>:<消息id>`），不再发第二条。
- **失败**：Telegram 发送失败如实报 `fail`，由内核退避重试。ack 返回 409（版本已作废）后，该投递不再被报告。
- **回答**：点选项按钮，或回复那条消息（回复选项 id 当作选择，其他文字当作文字回答），转达为 `POST /deliveries/{id}/answer`。内核拒绝（web_only、不在白名单、已有结果等）时，原因和 Web 链接原样发给本人，不当作已回答。
- **web_only**：内核在 web_only 决策的投递内容里置 `web_only: true`（无选项、推荐、表单链接）。ext 只发标题、摘要和 Web 链接，不带按钮；本人在 Telegram 里回复时，ext 不转达，直接回 Web 链接。未置位却被内核拒绝的回答（纵深防御）同样把原因和 Web 链接发给本人。
- **Web 链接 / 表单链接**：每条消息都带 Web 链接；内核放进 `form_url` 的一次性表单链接原样转发。

## 已知限制

- 发送到写入状态文件之间（Telegram 响应返回后的几微秒到一次 fsync）若进程被杀，重启后会重发一次。Telegram 没有按幂等键查消息的接口，消除不了这个窗口。
- 发送记录保留 30 天。

## 选型：Bot SDK

用 [`github.com/go-telegram/bot`](https://github.com/go-telegram/bot)（v1.x）：成熟的 Telegram Bot API 封装，只依赖标准库，自带 `getUpdates` 长轮询（不需要公网 webhook），支持自定义 Bot API 地址，所以节点测试能指向本地替身（`fakebot/`）。自己实现 Bot API 的长轮询、重试和模型没有价值。

**换掉它要改哪里**：只改 `ext/telegram/ext.go`（`New`、`send`、`onButton`、`onReply` 里用到 `bot.*` 和 `models.*` 的几处）和 `fakebot/`（替身用它的 `models` 类型）。投递协议（`apa.go`）、发送记录（`store.go`）和内核都不变。换平台时在 `ext/` 下新增一个 ext，签发新密钥并配置回答人白名单。

## 自测与测试

- `apa ext check telegram`：协议一致性（领取、幂等、回报、版本冲突、回答）。`APA_EXT_CHECK=1` 时 ext 用 `fakebot` 和临时状态，不碰真实 Telegram 与真实状态。
- `go test ./ext/telegram/...`：节点测试用 `fakebot` 替身；真实 Telegram 由 e2e-telegram-live 证明。
