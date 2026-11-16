# 附录 D：适配对象与实测结论

> 主文档只写类别，不写厂商和产品的名字（命名约定见主文档开头）。本附录列出本版随发行附带的适配对象、它们各自的专属约定和实测结论。这里出现的每个名字都在 APA 的通用契约之后，可替换、不唯一：换掉任何一个，都不改内核、能力包规则和 HTTP API。harness 一侧的细节在[附录 A](appendix-a-harness.md)。

## D.1 本版适配的对象

| 主文档里的称呼 | 本版适配 | 位置 | 所在的通用契约 |
|---|---|---|---|
| 首批 harness（界面称 Agent） | Hermes | `plugins/hermes` | harness 插件接口（主文档 §11.2、附录 A） |
| 即时通讯渠道 ext | Telegram，用成熟的 Bot SDK | `ext/telegram` | 渠道 ext 协议（主文档 §10.3） |
| 推送渠道（内置连接插件） | Bark（官方服务或自建服务器） | `connectors/bark` | 渠道接口（主文档 §10.3）；字段映射与实测项见 D.5 |
| 导出 ext | Markdown 目录 | `ext/export` | 事件订阅与只读 API（主文档 §10.3） |
| webhook 转发 ext | 通用的签名 POST | `ext/webhookforward` | 事件订阅（主文档 §10.3）；可选 `APA_WEBHOOK_TYPES`（逗号分隔的事件类型）只转发所列类型，缺省转发全部，未选中的事件不投递但游标照常推进 |
| 代码托管平台来源 | GitHub（`source_type: github`） | `sources/` | Source 执行契约（主文档 §9.2） |
| 邮件 CLI | himalaya | `sources/` | Source 执行契约，自定义脚本（主文档 §9.7） |
| 日历 CLI | gog calendar（Google 日历） | `sources/` | 同上 |
| 桌面系统自带的邮件、日历、提醒事项应用 | apple-pim 的三个 CLI（macOS） | `sources/applepim` | 同上 |
| webhook 校验方案 | GitHub 类、GitLab 类、Slack 类，以及通用共享 token | `packs/inbound-triage` | 能力包的声明式校验方案（主文档 §9.5、附录 C.5） |

RSS/Atom、通用 HTTP JSON 轮询、本地目录监视三个拉取程序不对应具体产品，不在本表。

**来源的接入声明与显示名**（主文档 §9.1）：每个来源接入都必须有名字。仓库自带的每个来源程序在自己的目录里带一份 `source.yaml`，写明它登记出的来源叫什么；登记（Web、`apa source add`、`sources.json`）不带名字时取声明的名字，带了以带的为准，都没有就拒绝登记。声明随 `apa` 主程序嵌入（`sources/declarations.go`），发布物不另带文件；名字只用于显示，不经 stdin 交给来源程序，适配器不需要处理它。

| 来源程序 | 声明的名字（中文 / 英文） | 同一程序多个实例怎么区分 |
|---|---|---|
| `apple-pim-source` | 按 `config.kind`：`mail` 为"`{account}` 邮件 / `{account}` Mail"（如"iCloud 邮件""QQ 邮件"），`calendar` 为"Apple 日历 / Apple Calendar"，`reminders` 为"提醒事项 / Reminders"；邮件没有 `account` 时为"Apple 邮件" | 邮件按账户名；同类日历或提醒事项接多个时登记时起名 |
| `mail-himalaya` | "`{account}` 邮件 / `{account}` Mail"（`account` 是邮件工具里的账户名，如 `gmail` 得到"gmail 邮件"）；没有 `account` 时为"邮件 / Mail" | 按账户名 |
| `calendar` | "Google 日历 / Google Calendar" | 登记时起名 |
| `github-source` | "GitHub"；`source_type` 为 `github` 的 webhook 来源同名 | 登记时起名 |
| `rss-source` | "RSS 订阅 / RSS feed" | 登记时起名 |
| `http-json` | "JSON 接口 / JSON endpoint" | 登记时起名 |
| `dir-watch` | "文件夹 / Folder" | 登记时起名 |

**发件人是否可信**（主文档 §9.1）：同一份 `source.yaml` 里的 `sender` 段。界面对本人只显示"可信"或"需确认"一个结论，本人不能改。

| 来源程序 | 结论 | 按事件类型细分 | 依据 |
|---|---|---|---|
| `github-source`；`source_type` 为 `github` 的 webhook | 可信，格式提示"GitHub 用户名"，能推出本人身份（`identity` 模式取 `GET /user` 的 `login`） | `*.opened`、`*.commented`、`pr.review_commented`、`pr.reviewed` 可信；`*.updated`、`*.closed`、`*.merged`、`notification.*` 不带发件人 | 作者取自接口返回的 `user.login`（webhook 取验签后的 `sender.login`），由平台填写。聚合出的更新与关闭事件不知道动作者，通知接口不给动作者 |
| `mail-himalaya` | 需确认 | — | 取不到可信的认证结果信头（D.3 `MEAS-MAIL-AR`） |
| `apple-pim-source` | 需确认 | 提醒事项不带发件人 | 邮件的认证结果信头能否取到待实测（主文档 §21.3 `MEAS-MAIL-DMARC`）；日历组织者由对方写入 |
| `calendar` | 需确认 | — | 组织者由对方的邀请写入 |
| `rss-source`、`http-json` | 需确认 | — | 作者与字段由对方写 |
| `dir-watch` | 需确认 | 全部不带发件人 | 没有发件人 |

写法：

```yaml
sender:
  trust: verified            # 或 unverified
  types: {"issue.updated": none, "notification.*": none}   # 可选；值 verified、unverified、none
  hint: {zh: GitHub 用户名, en: GitHub username}         # trust 为 verified 时必填
  self: identity             # 可选：程序支持 identity 模式（主文档 §9.2）
```

"登记时起名"的程序再登记第二个实例而不带名字时，声明出的名字已被占用，返回 409，提示起名；不会自动加序号。`hosted-reader` 是托管文档读取器，不登记为来源，没有声明。

**来源程序作者**：新增或换掉一个来源程序，在它的目录里写 `source.yaml`，声明显示名是接入的必填项。字段：`program`（`command` 第一个词的文件名部分）、`default`（中英各一份的名字，不含占位，必填）、`name`（可选的名字模板，`{字段}` 取 `config` 里同名的字符串值，取不到值时用 `default`）、`by` 与 `names`（可选：按 `config` 的 `by` 字段取值选模板，优先于 `name`）、`webhook_source_type`（可选：同一 `source_type` 的 webhook 来源也按本声明取名）。例：

```yaml
program: apple-pim-source
by: kind
names:
  mail: {zh: "{account} 邮件", en: "{account} Mail"}
  calendar: {zh: Apple 日历, en: Apple Calendar}
  reminders: {zh: 提醒事项, en: Reminders}
default: {zh: Apple 邮件, en: Apple Mail}
```

`sources/declarations_test.go` 要求 `sources/*/cmd/*` 下每个来源程序都有声明，且声明带 `sender` 段。仓库之外的来源程序没有声明位置，登记时必须带名字（`sources.json` 的 `name`、`apa source add --name` 或 Web 的名字一栏）。账本只存一种语言的名字，取中文；Web 新登记表单按界面语言预填，本人可改。

**升级补名**：升级前登记的来源没有名字，`apa serve` 启动时先按上面的声明补；取不到声明的（自定义脚本等）为了兼容按 `source_type` 起名（`cli/serve/assemble.go:sourceKindNames`：`mail` 邮件、`calendar` 日历、`reminder` 提醒事项、`github` GitHub；表里没有的按来源类型称"定时拉取""自定义脚本""外部推送"），同名加序号，并在健康视图留一条"建议起名"的说明。

## D.2 邮件与日历的工具适配

共同约定见主文档 §9.7，这里是各工具的取数方式与专属约定。

| 类别 | 范围 | 工具 | 取数方式 | 鉴权（归工具） |
|---|---|---|---|---|
| 邮件 | 本版 | himalaya（IMAP、JMAP、Gmail 等） | `himalaya envelope search`、`message read`，加 `--json` | 口令由工具配置里的口令命令取得；OAuth 由外部的令牌工具提供，工具的 2.x 版本不含 OAuth 流程 |
| 日历（Google） | 本版 | gog calendar | `gog --no-input --json --enable-commands=calendar --account=<账号> calendar events` | `gog auth`，自行刷新令牌；无桌面环境用它的加密文件后端，口令经 `credentials_path` 交给适配器 |
| 邮件、日历、提醒事项（macOS 自带应用） | 本版（适配器 `apple-pim-source`） | apple-pim 的 `mail-cli`、`calendar-cli`、`reminder-cli` | `mail-cli messages`、`mail-cli get`、`calendar-cli events`、`reminder-cli items`，加 `--format=json` | 系统授权（邮件的自动化与索引读取、日历、提醒事项），不需要邮箱口令；授权归"启动工具的应用"，后台服务拉起时经辅助应用 |
| 邮件（Google） | 后续 | gog | `gog --no-input --json --enable-commands=gmail --account=<账号> gmail search` | 同 gog calendar |
| 日历（CalDAV 等） | 后续 | vdirsyncer + khal | `vdirsyncer sync` 同步到本地，再 `khal list --json <字段>` | vdirsyncer 配置（口令命令或 OAuth） |

本版日历 CLI 只覆盖 Google 日历；其他日历服务等后续适配器，或由用户按主文档 §9.2 自写脚本接入。

**各工具的专属约定**：

| 约定 | 内容 |
|---|---|
| gog 凭据文件 | `credentials_path` 指向的 `KEY=VALUE` 文件与 gog 的 `keyring.env` 写法兼容。适配器只取 `GOG_KEYRING_PASSWORD`（必填）和 `GOG_KEYRING_BACKEND`（缺省 `file`），其余键忽略。文件可以是指向 gog 自己 `keyring.env` 的符号链接。未登记 `credentials_path` 时不注入，适合 gog 用系统钥匙串的桌面环境 |
| gog 账号 | 登记在来源 `config.account`（gog 已登录的邮箱，见 `gog auth list`），适配器以 `--account` 传入 |
| gog 只读 | gog v0.9 没有只读开关，`--enable-commands` 只能限制到顶层命令（如 `calendar`，其下的 `create`、`delete` 仍可用）。日历适配器带 `--enable-commands=calendar` 挡住其他服务，只读由适配器只调用 `calendar events` 保证 |
| Google 的时间精度 | 创建时间只到秒，更新时间带毫秒，所以"更新时间按秒晚于创建时间"才算 `event.updated` |
| himalaya 版本 | JSON 标志在不同版本分别是 `--output json` 和 `--json`；适配器运行时比对 `--version` |
| Apple 应用 | 一个适配器 `apple-pim-source`，`config.kind` 选 `mail`、`calendar`、`reminders`，各登记一个来源（`source_type` 分别为 `mail`、`calendar`、`reminder`；建议带上 `name`，如"Apple 日历"，以便与 Google 日历的来源区分）。`mail` 的 `account` 是邮件应用里的账户名，每个账户一个来源；`calendar` 的 `calendars`（名或 ID，缺省全部）用来只收不与其他日历来源重复的日历；`reminders` 的 `lists` 缺省全部，取未完成且有到期日的 `upcoming` 与 `overdue`。事件类型：邮件 `mail.received`（id 为 Message-ID）；日历 `event.created` / `event.cancelled`（工具不给更新时间，id 为工具日程 id、实例日期与内容摘要，关联键去掉摘要）；提醒事项 `reminder.due`（id 为提醒 id 加内容摘要，关联键为提醒 id）。邮件用 `--engine=sqlite` 只读索引与邮件文件，不经应用，不改已读标记；邮件按 `--since` 最旧优先取，游标推进到已读部分。三个工具没有版本参数，输出契约靠必填字段校验（缺字段、`success` 为 false 返回 2），录制样本在 `sources/applepim/testdata`。`config.bin_dir` 指定工具目录（缺省按 `PATH`） |
| Apple 应用的系统授权 | 日历和提醒事项的系统授权归启动工具的应用，后台服务拉起时没有可授权的应用。`config.helper_app` 指定辅助应用，填上后适配器经 `open -W -n -a`（`-n`：多个来源并发时各开一个实例，否则运行中的辅助应用收不到参数）让辅助应用成为授权主体；因 `open` 不回传退出码，这条路径以 stdout 为空判失败，stderr 含授权拒绝字样返回 2。授权由本人在系统弹窗或系统设置里给，APA 不代办 |

邮件线程关联键（主文档 §9.7）在各邮件适配器里的取法：事件的 `correlation_keys` 含 `thread:<线程根 Message-ID>`，线程根取 `References` 的第一项，没有则取 `In-Reply-To`，都没有则是邮件自己的 Message-ID；取不到线程头时只给自己的，回信按普通事件处理。himalaya（`sources/mail`，1.x 范围）在读取正文的同一次调用里加 `-H Message-ID -H References -H In-Reply-To`（仍带 `--preview`），输出的头部块按名取值、支持折行；2.x 范围未实测线程头，只给自己的 Message-ID。apple-pim（`sources/applepim`，`config.kind=mail`）取 `mail-cli get` 返回的 `message.references`、`message.inReplyTo`，并按自己 `messageId` 的写法（带不带尖括号）对齐，使原信与回信的键相等。录制样本：`sources/mail/testdata/1.1.0/thread/`（原信、带 References 与 In-Reply-To 的回信、只带 In-Reply-To 的回信、多级回复），`sources/applepim/testdata/messages-thread.json` 与 `get-mid-p*.json`。

## D.3 工具的实测结论

版本：himalaya v1.1.0（对测试 IMAP 邮箱）、gog v0.9.0（真实 Google 日历，只动带固定测试前缀的临时事件，测后删除）。每行先写被测命题。

| 项 | 结论 | 说明与实测版本 |
|---|---|---|
| `MEAS-MAIL-SEEN` | 有条件成立 | 命题：读正文不改已读标记。`message read --preview` 前后标记不变；不加 `--preview` 的 `message read` 在该测试邮箱上标记也没变，但其他 IMAP 服务器可能在非预览读取时置已读，所以适配器一律带 `--preview`。himalaya v1.1.0，仅一个邮箱服务实测 |
| `MEAS-MAIL-MSGID` | 不成立 | 命题：`envelope list` 的 JSON 带 Message-ID。字段只有 `id`（IMAP 序号）、`flags`、`subject`、`from`、`to`、`date`、`has_attachment`，没有 Message-ID；用 `message read --preview -H Message-ID <id>` 取，输出 `Message-ID: <…>` 后接空行和正文，与读正文是同一次调用。himalaya v1.1.0；2.x 未实测 |
| `MEAS-MAIL-AR` | 不成立 | 命题：能在不改已读标记的前提下取到收件服务加的认证结果信头。`message read --preview -H Authentication-Results` 遇到多条同名信头只输出**最后一条**，即最靠近发件人、可被发件人伪造的那条，收件服务加在最上面的那条取不到；`message export --full` 能取到全部原始信头，但会把信置为已读。所以邮件来源声明为需确认。himalaya v1.1.0，在合成的本地邮箱上实测（一条收件方结果在上、一条伪造结果在下），未对真实邮箱运行；IMAP 后端未测 |
| `MEAS-MAIL-AUTH` | 成立 | 命题：认证失败可识别。错误口令与服务不可达都以退出码 1 失败，没有专用退出码，区别只在 stderr：前者含 `Invalid credentials`，后者含 `Connection refused`。适配器按 stderr 文本识别并返回 2。himalaya v1.1.0 |
| `MEAS-CAL-INCR` | 不成立 | 命题：gog 有增量查询。`calendar events` 只有 `--from/--to`、`--days`、`--query`、`--max`、`--page` 一类窗口与分页参数，没有按更新时间或同步令牌取增量的选项；返回的 `nextPageToken` 只用于分页。做法是每次取前瞻窗口，内核按事件 id 去重。gog v0.9.0 |
| `MEAS-CAL-IDS` | 成立 | 命题：JSON 带 UID、RECURRENCE-ID 和更新时间。每个日程带 `iCalUID`、`updated`、`created`、`etag`；重复事件的实例另带 `recurringEventId` 和 `originalStartTime`，后者即 RECURRENCE-ID。gog v0.9.0 |
| `MEAS-CAL-RECUR` | 成立 | 命题：重复事件按窗口展开为实例。`FREQ=DAILY;COUNT=3` 的日程在窗口内列出 3 个实例，id 为主日程 id 加 `_<开始时间>`，`iCalUID` 相同，`originalStartTime` 各异；删除后窗口内不再出现。创建命令无法关闭日历默认提醒。gog v0.9.0 |
| `MEAS-CAL-AUTH` | 成立 | 命题：认证失败可识别。账号没有令牌：退出码 1，stderr `No auth for calendar <账号>`；keyring 口令不对：退出码 1，stderr 含 `integrity check failed`。没有专用退出码，适配器按 stderr 识别并返回 2。gog v0.9.0 |
| `MEAS-CAL-FLAGS` | 成立 | 命题：gog 没有只读开关。传入 `--readonly` 即以 `unknown flag` 失败；`--enable-commands` 只认顶层命令，写成 `calendar.events` 之类的子命令会连 `calendar` 一起禁用。API 命令要 `--account`（或 gog 自己的缺省账号），使用文件 keyring 时还要 `GOG_KEYRING_PASSWORD`。gog v0.9.0 |

尚未证实的条目，以及由谁证实：

| 项 | 需实测的结论 | 由谁证实 |
|---|---|---|
| `MEAS-MAIL-THREAD` | himalaya 与 `mail-cli` 在读取正文的同一次调用里能否取到 `References`、`In-Reply-To` 头 | **录制样本部分（W1 开发验收）已有结论**：两个适配器的契约测试都断言回信（含只带 `In-Reply-To` 的）的 `correlation_keys` 含原信的 `thread:<Message-ID>`，见 D.2 与 flow-source-mail 的 ACT-MAIL-08/10/12/13。样本版本：himalaya 1.1.0（头部块沿用 1.1.0 实录的 `Message-ID: …` 输出形态，References/In-Reply-To 行按同一形态合成，非真实邮箱实录）；mail-cli 无版本参数，`message.references`/`inReplyTo` 字段为按设计合成。**真实邮件工具上的证实由本机升级验证 L11 承接**（主文档 §20.4），在此之前不得把"能取到"当作已证实 |
| `MEAS-MAIL-DMARC` | `mail-cli get --include-source` 在 `--engine=sqlite` 下能否返回原始信头、是否改已读标记、字段名；收件服务的标识按账户配置还是按服务推定；`mail-cli accounts` 是否带邮箱地址。`mail-cli auth-check` 只做 DKIM 与 SPF、自带一份发件人名单，不用它 | 把 apple-pim 邮件声明为可信之前的验收（主文档 §21.3） |
| `MEAS-ADAPT-GMAIL` | gog 邮件结果的粒度（线程或消息）及其增量查询方式 | 接入该适配器时的验收 |
| `MEAS-ADAPT-KHAL` | `khal list --json` 没有更新时间字段时如何识别更新；重复事件的展开 | 接入该适配器时的验收 |

## D.4 代码托管来源与 webhook 校验方案

- **拉取程序**：本版附带 GitHub 的拉取程序，产出 Issue、PR、评论、通知四类事件，并在每次拉取时比对仓库默认分支根目录 AGENTS.md 的 blob sha（主文档 §12.5）。来源类型登记为 `github`，内置规则按它匹配（附录 C.14）。
- **评论**：按仓库另取 `GET /repos/{owner}/{repo}/issues/comments?since=` 与 `GET /repos/{owner}/{repo}/pulls/comments?since=`，每条评论一个事件（`issue.commented`、`pr.commented`、`pr.review_commented`；issues/comments 里 PR 上的普通评论按 `html_url` 认作 `pr.commented`），id 只含评论 id，编辑后再读到的由去重吸收；`sender` 为评论的 `user.login` 并已核实，关联键取所属 issue 或 PR 的 `owner/repo#编号`，因此挂到已有事项上成为挂靠消息。两类评论各有自己的游标流，与 issue 列表共用每次运行的条数上限。Issue 与 PR 的 `updated`、`closed`、`merged` 由列表的时间字段推出，不知道动作者，不带 `sender`。webhook 侧 `inbound-triage` 包另有三个映射：`github-issue-comments`（`issue_comment` 的 `created`，一律 `issue.commented`，因为载荷里 issue 与 PR 的评论同形）、`github-pr-reviews`（`pull_request_review` 的 `submitted`，`pr.reviewed`）、`github-pr-review-comments`（`pull_request_review_comment` 的 `created`，`pr.review_commented`）；编辑与删除不映射；`sender` 取 `$.sender.login`，验签通过才算已核实。
- **仓库简写**：托管型材料的只读读取器把 `owner/repo` 简写展开成 GitHub 地址；默认站点只在读取器里认定，内核只保存登记的写法（主文档 §12.3）。
- **校验方案**（`inbound-triage` 包的声明式数据，附录 C.5）：

| 方案 | 做法 | 适用 |
|---|---|---|
| `hmac-sha256-header` | 对请求体做 HMAC-SHA256，签名在 `X-Hub-Signature-256` 头，投递 id 取 `X-GitHub-Delivery` | GitHub 类 |
| 平台 token 头 | 比对固定请求头里的共享 token | GitLab 类 |
| `slack-style` | 对 `v0:{时间戳}:{请求体}` 做 HMAC-SHA256，并同步应答 URL 验证握手 | Slack 类 |
| 通用共享 token | 请求头或查询参数里的共享 token | 其他系统 |

## D.5 即时通讯渠道

本版的渠道 ext 是 Telegram：独立进程，长轮询领取投递，用成熟的 Bot SDK 收发。本人的 harness 也连着 Telegram 时，ext 用单独的 Bot，因为同一个 Bot 不能同时有多个长轮询消费者。渠道在 `/settings/policy` 的 `channels` 段登记为 `telegram`。

- **回答人**：ext 一律以 Telegram 分配的数字用户 ID（`from.id`）作为回答人，不用可改、可被他人重新注册的用户名。
- **识别 ID**：本人在"设置 › 渠道"点"识别我的 ID"，页面给出口令；本人在与 Bot 的私聊里发 `/start <口令>`（只发口令也行），ext 把 `from.id` 与 `@用户名` 经 `POST /channels/identify` 上报，回一句"已收到，回网页确认"，本人在网页点"加上"才写入名单。口令无效时 Bot 不回话；私聊之外的会话不处理识别。识别只在内核内存里，10 分钟有效，内核重启即作废。
- 旧版本写着用户名的回答人条目升级后不再匹配，按主文档 §16.5 识别一次。

其他渠道（企业协作工具、其他聊天应用、邮件发信）按同一份渠道 ext 协议实现，不改内核。

**内置渠道：Bark**（`connectors/bark`，主文档 §10.3）。推送到苹果设备，只用标准库调用 Bark 推送服务的 HTTP 接口：`POST <服务器>/push`（JSON），自检用 `GET <服务器>/ping`。服务端以 `bark-server`（MIT）为准，官方公共服务是默认地址，自建服务器开了登录验证时按 HTTP Basic Auth 带登录名与密码。

| 项 | 内容 |
|---|---|
| 能力 | 都为假，只能看：决策提醒带一次性表单链接，web_only 的决策只带 Web 链接 |
| 配置 | 服务器地址（默认官方服务）、等你决定的提醒是否用时效性通知（默认是）；高级里是通知分组（默认 `APA`）、登录名 |
| 凭据 | 设备密钥；登录密码（自建服务器开了登录验证时） |
| 字段映射 | `title` ← 标题；`body` ← 摘要与推荐（截断到 600 字）；`group` ← 通知分组；`url` ← 表单链接，没有就用 Web 链接；`level` ← 决策且开了时效性通知时为 `timeSensitive`；`id` ← 幂等键（投递 id + 版本）。没有值的字段不发 |
| 问题代码 | 401、403 → `auth_failed`；429 → `rate_limited`；5xx 与连不上 → `unreachable`；其他非 2xx 或返回体 `code` 不是 200 → `rejected`（多是设备密钥不对） |
| 已知限制 | 只有苹果设备；服务端回 200 只表示已交给苹果的推送服务；标题与摘要经过推送服务器和苹果的推送通道，自建服务器也一样（首版不做端到端加密推送）；表单链接要求手机能打开 APA 的外部地址 |
| 未实测 | `SPIKE-PUSH-PARAMS`、`SPIKE-PUSH-FORM`（主文档 §21.3）。仓库里的契约测试只对本地替身服务，不连真实服务 |

## D.6 服务商一侧的鉴权摩擦

Gmail、Google 日历、Google Drive 这类服务要用户自备 OAuth 客户端，测试模式下的授权约七天过期。鉴权交给工具后这个摩擦依然存在：配置按工具文档走，例如把 OAuth 应用发布出测试模式；认证失败时来源返回退出码 2，生成 `needs_input`。能用应用专用密码的服务也可经 IMAP 接入（主文档 §21.2 风险 9）。

## D.7 模型端点

生成端点按本人选定的生成协议调用（主文档 §8.1，对照表见下）；判断端点按协议分适配器（主文档 §8.8），本版内置原生判断接口 `system-one` 与自建服务的通用协议 `probability-judgment/1`。端点由用户提供。以下结论由 v0.2.0 的 spike 与本机升级验证写回这里（主文档 §20.4、§21.3）：

| 项 | 写回什么 | 状态 |
|---|---|---|
| `SPIKE-STRUCT` | 被测生成端点的类别（本机推理服务、外部托管服务）、对 JSON Schema 结构化输出的支持方式、内置模板的通过情况 | 待本版证实 |
| `SPIKE-LOGPROB` | 被测端点是否返回首个输出 token 的对数概率、换算后的排序质量、`chat-logprobs` 是否开放 | 换算路径已在对话端点替身上验证（主文档 §8.8）；真实端点与排序质量待本机升级验证 L8，`chat-logprobs` 在此之前不开放 |
| `SPIKE-CLS-BATCH` | 被测分类端点的时延分布与批量上限，默认值是否调整 | 超时、超限、批量的处置已在分类端点替身上验证（主文档 §8.8）。`system-one` 在官方接口上做过一次冒烟（3 道中文题一批，`jev-latest` → `jev-1.14.0`）：冷调用约 9.7 秒（含建立连接，贴近 `timeout_ms` 默认的 10000），热调用约 0.2 秒；时延分布与默认值待本机升级验证 L8 |

**`system-one` 的已知实现**（2026-11 核对官方文档）：

| 项 | 内容 |
|---|---|
| 服务 | TypeSafe 的 System One 接口，模型 Jev；文档 `https://docs.typesafe.ai/llms.txt` |
| 预填值 | 地址 `https://api.typesafe.ai/v1`，模型 `jev-latest`（别名，响应里给实际版本，如 `jev-1.14.0`；需要固定版本时填版本号） |
| 经网关接入 | 地址改成网关的接口根（如 `http://<网关>/v1`），模型名按网关的命名（如 `typesafe/jev-latest`） |
| 限制 | 题数不设上限；每请求 64k token（state 加全部题）、每题 32k token；请求体至多 32 MiB；限流按组织计，超出回 429，可带 `retry-after`；过载回 529 |
| 地区 | 官方接口对部分地区回 451（`Typesafe is not available in your region.`），内核记 `region_unavailable`，"测试连接"提示改用可访问的网关地址或代理 |
| 超时 | 冷调用约 9.7 秒，Web 新配时 `timeout_ms` 预填 30000 |

**生成协议与它们对应的接口**（2026-11 核对官方文档）：

| 协议 | Web 显示名 | 对应的接口 | 本版的取法 |
|---|---|---|---|
| `chat-completions` | OpenAI 兼容 | OpenAI Chat Completions，及讲同一形状的本机推理服务与网关 | 见主文档 §8.1 |
| `messages` | Anthropic 兼容 | Anthropic Messages API（`/v1/messages`），及讲同一形状的网关 | 版本头 `anthropic-version: 2023-06-01`，密钥放 `x-api-key`；`max_tokens` 固定 4096；输出工具名 `output` |
| `responses` | OpenAI Responses | OpenAI Responses API（`/v1/responses`） | `instructions` 放系统段，`input` 放用户段，`store: false` |

**消息接口里结构化输出与思考的处理**。依据：[Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking)、[Thinking in tool and multi-turn workflows](https://platform.claude.com/docs/en/build-with-claude/thinking-tool-workflows)、[Define tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools)、[Claude Haiku 5.5 迁移指南](https://platform.claude.com/docs/en/models/haiku-5-5/migration-guide)。

- 显式开启的手动思考（`thinking: {type: "enabled"}`）不接受强制工具调用（`tool_choice` 为 `any` 或指名工具），会返回错误。内核因此不带 `thinking` 字段。
- Claude 5 系列多数模型默认开着自适应思考。自适应思考下，Claude Haiku 5.5 等模型接受强制工具调用，回复直接从工具调用开始，不带思考块；Claude Opus 5.5、Sonnet 5.5、Fable 5.1/5.2、Mythos 5.1/5.2 对强制工具调用一律返回 400。这几种模型请选另一种协议（经网关时选网关的对话补全或响应接口），"测试连接"会显示 400 和端点给的原因。
- 内核按块的 `type` 取结果，不按位置：`thinking`、`redacted_thinking` 与工具调用前的 `text` 块都忽略，只取名为 `output` 的 `tool_use` 块的 `input`。`stop_reason` 为 `max_tokens` 时按截断处理，不使用可能不全的工具参数。
- 不用消息接口的 JSON 输出（`output_config.format`）：它也是原生方式，但网关对这个字段的转发支持不一，工具调用的兼容面更广。

**经网关接入时选哪种协议**。网关在对话补全接口与消息接口之间转换时，`response_format` 没有对应字段，结构化约束会丢掉，思考内容也可能混进正文；这正是"起草建议的输出不合格"的常见原因。选网关对外讲的、与上游模型原生一致的接口：

- 上游是 Claude 模型、网关对外提供 `/v1/messages`（New API、Magpie 这类网关都提供）：选「Anthropic 兼容」，地址填网关的 `…/v1`。上游模型对强制工具调用返回 400 时（见上），改选「OpenAI 兼容」或「OpenAI Responses」并确认网关为它保留结构化约束。
- 上游是 OpenAI 模型：选「OpenAI Responses」或「OpenAI 兼容」。
- 本机推理服务一般只讲对话补全接口：选「OpenAI 兼容」。

无论选哪种，先点"测试连接"：它用起草建议模板的真实约束发一次请求，结论写明协议与原生结构化输出方式；不合格时写明返回内容的形态。

自建的分类服务讲 `probability-judgment/1` 即可接入；仓库里用于验收的分类端点替身（同时讲两种协议）可作参照（主文档 §18）。
