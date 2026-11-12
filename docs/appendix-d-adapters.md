# 附录 D：适配对象与实测结论

> 主文档只写类别，不写厂商和产品的名字（命名约定见主文档开头）。本附录列出本版随发行附带的适配对象、它们各自的专属约定和实测结论。这里出现的每个名字都在 APA 的通用契约之后，可替换、不唯一：换掉任何一个，都不改内核、能力包规则和 HTTP API。harness 一侧的细节在[附录 A](appendix-a-harness.md)。

## D.1 本版适配的对象

| 主文档里的称呼 | 本版适配 | 位置 | 所在的通用契约 |
|---|---|---|---|
| 首批宿主 | Hermes | `plugins/hermes` | harness 插件接口（主文档 §11.2、附录 A） |
| 即时通讯渠道 ext | Telegram，用成熟的 Bot SDK | `ext/telegram` | 渠道 ext 协议（主文档 §10.3） |
| 导出 ext | Markdown 目录 | `ext/export` | 事件订阅与只读 API（主文档 §10.3） |
| webhook 转发 ext | 通用的签名 POST | `ext/webhookforward` | 事件订阅（主文档 §10.3） |
| 代码托管平台来源 | GitHub（`source_type: github`） | `sources/` | Source 执行契约（主文档 §9.2） |
| 邮件 CLI | himalaya | `sources/` | Source 执行契约，自定义脚本（主文档 §9.7） |
| 日历 CLI | gog calendar（Google 日历） | `sources/` | 同上 |
| 桌面系统自带的邮件、日历、提醒事项应用 | apple-pim 的三个 CLI（macOS） | `sources/applepim` | 同上 |
| webhook 校验方案 | GitHub 类、GitLab 类、Slack 类，以及通用共享 token | `packs/inbound-triage` | 能力包的声明式校验方案（主文档 §9.5、附录 C.5） |

RSS/Atom、通用 HTTP JSON 轮询、本地目录监视三个拉取程序不对应具体产品，不在本表。

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
| Apple 应用 | 一个适配器 `apple-pim-source`，`config.kind` 选 `mail`、`calendar`、`reminders`，各登记一个来源（`source_type` 分别为 `mail`、`calendar`、`reminder`）。`mail` 的 `account` 是邮件应用里的账户名，每个账户一个来源；`calendar` 的 `calendars`（名或 ID，缺省全部）用来只收不与其他日历来源重复的日历；`reminders` 的 `lists` 缺省全部，取未完成且有到期日的 `upcoming` 与 `overdue`。事件类型：邮件 `mail.received`（id 为 Message-ID）；日历 `event.created` / `event.cancelled`（工具不给更新时间，id 为工具日程 id、实例日期与内容摘要，关联键去掉摘要）；提醒事项 `reminder.due`（id 为提醒 id 加内容摘要，关联键为提醒 id）。邮件用 `--engine=sqlite` 只读索引与邮件文件，不经应用，不改已读标记；邮件按 `--since` 最旧优先取，游标推进到已读部分。三个工具没有版本参数，输出契约靠必填字段校验（缺字段、`success` 为 false 返回 2），录制样本在 `sources/applepim/testdata`。`config.bin_dir` 指定工具目录（缺省按 `PATH`） |
| Apple 应用的系统授权 | 日历和提醒事项的系统授权归启动工具的应用，后台服务拉起时没有可授权的应用。`config.helper_app` 指定辅助应用，填上后适配器经 `open -W -n -a`（`-n`：多个来源并发时各开一个实例，否则运行中的辅助应用收不到参数）让辅助应用成为授权主体；因 `open` 不回传退出码，这条路径以 stdout 为空判失败，stderr 含授权拒绝字样返回 2。授权由本人在系统弹窗或系统设置里给，APA 不代办 |

邮件线程关联键（主文档 §9.7）在各邮件适配器里的取法：himalaya 与 `mail-cli` 都在读取正文的同一次调用里取 `References` 与 `In-Reply-To` 头；取不到时只给邮件自己的 Message-ID。这一取法属于 v0.2.0 的新增行为，随适配器的契约测试加录制样本（`MEAS-MAIL-THREAD`，见 D.3）。

## D.3 工具的实测结论

版本：himalaya v1.1.0（对测试 IMAP 邮箱）、gog v0.9.0（真实 Google 日历，只动带固定测试前缀的临时事件，测后删除）。每行先写被测命题。

| 项 | 结论 | 说明与实测版本 |
|---|---|---|
| `MEAS-MAIL-SEEN` | 有条件成立 | 命题：读正文不改已读标记。`message read --preview` 前后标记不变；不加 `--preview` 的 `message read` 在该测试邮箱上标记也没变，但其他 IMAP 服务器可能在非预览读取时置已读，所以适配器一律带 `--preview`。himalaya v1.1.0，仅一个邮箱服务实测 |
| `MEAS-MAIL-MSGID` | 不成立 | 命题：`envelope list` 的 JSON 带 Message-ID。字段只有 `id`（IMAP 序号）、`flags`、`subject`、`from`、`to`、`date`、`has_attachment`，没有 Message-ID；用 `message read --preview -H Message-ID <id>` 取，输出 `Message-ID: <…>` 后接空行和正文，与读正文是同一次调用。himalaya v1.1.0；2.x 未实测 |
| `MEAS-MAIL-AUTH` | 成立 | 命题：认证失败可识别。错误口令与服务不可达都以退出码 1 失败，没有专用退出码，区别只在 stderr：前者含 `Invalid credentials`，后者含 `Connection refused`。适配器按 stderr 文本识别并返回 2。himalaya v1.1.0 |
| `MEAS-CAL-INCR` | 不成立 | 命题：gog 有增量查询。`calendar events` 只有 `--from/--to`、`--days`、`--query`、`--max`、`--page` 一类窗口与分页参数，没有按更新时间或同步令牌取增量的选项；返回的 `nextPageToken` 只用于分页。做法是每次取前瞻窗口，内核按事件 id 去重。gog v0.9.0 |
| `MEAS-CAL-IDS` | 成立 | 命题：JSON 带 UID、RECURRENCE-ID 和更新时间。每个日程带 `iCalUID`、`updated`、`created`、`etag`；重复事件的实例另带 `recurringEventId` 和 `originalStartTime`，后者即 RECURRENCE-ID。gog v0.9.0 |
| `MEAS-CAL-RECUR` | 成立 | 命题：重复事件按窗口展开为实例。`FREQ=DAILY;COUNT=3` 的日程在窗口内列出 3 个实例，id 为主日程 id 加 `_<开始时间>`，`iCalUID` 相同，`originalStartTime` 各异；删除后窗口内不再出现。创建命令无法关闭日历默认提醒。gog v0.9.0 |
| `MEAS-CAL-AUTH` | 成立 | 命题：认证失败可识别。账号没有令牌：退出码 1，stderr `No auth for calendar <账号>`；keyring 口令不对：退出码 1，stderr 含 `integrity check failed`。没有专用退出码，适配器按 stderr 识别并返回 2。gog v0.9.0 |
| `MEAS-CAL-FLAGS` | 成立 | 命题：gog 没有只读开关。传入 `--readonly` 即以 `unknown flag` 失败；`--enable-commands` 只认顶层命令，写成 `calendar.events` 之类的子命令会连 `calendar` 一起禁用。API 命令要 `--account`（或 gog 自己的缺省账号），使用文件 keyring 时还要 `GOG_KEYRING_PASSWORD`。gog v0.9.0 |

尚未证实的条目，以及由谁证实：

| 项 | 需实测的结论 | 由谁证实 |
|---|---|---|
| `MEAS-MAIL-THREAD` | himalaya 与 `mail-cli` 在读取正文的同一次调用里能否取到 `References`、`In-Reply-To` 头 | v0.2.0。W1 的验收：两个邮件适配器各加录制样本，回信回到原事项；在真实邮件工具上的证实是本机升级验证 L11（主文档 §20.4） |
| `MEAS-ADAPT-GMAIL` | gog 邮件结果的粒度（线程或消息）及其增量查询方式 | 接入该适配器时的验收 |
| `MEAS-ADAPT-KHAL` | `khal list --json` 没有更新时间字段时如何识别更新；重复事件的展开 | 接入该适配器时的验收 |

## D.4 代码托管来源与 webhook 校验方案

- **拉取程序**：本版附带 GitHub 的拉取程序，产出 Issue、PR、通知三类事件，并在每次拉取时比对仓库默认分支根目录 AGENTS.md 的 blob sha（主文档 §12.5）。来源类型登记为 `github`，内置规则按它匹配（附录 C.14）。
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

其他渠道（企业协作工具、其他聊天应用、邮件发信）按同一份渠道 ext 协议实现，不改内核。

## D.6 服务商一侧的鉴权摩擦

Gmail、Google 日历、Google Drive 这类服务要用户自备 OAuth 客户端，测试模式下的授权约七天过期。鉴权交给工具后这个摩擦依然存在：配置按工具文档走，例如把 OAuth 应用发布出测试模式；认证失败时来源返回退出码 2，生成 `needs_input`。能用应用专用密码的服务也可经 IMAP 接入（主文档 §21.2 风险 9）。

## D.7 模型端点

内核只认两份协议：生成端点的对话补全兼容协议，判断端点的概率判断协议（主文档 §8.8）。端点由用户提供，本附录不推荐具体厂商或模型。以下结论由 v0.2.0 的 spike 与本机升级验证写回这里（主文档 §20.4、§21.3）：

| 项 | 写回什么 | 状态 |
|---|---|---|
| `SPIKE-STRUCT` | 被测生成端点的类别（本机推理服务、外部托管服务）、对 JSON Schema 结构化输出的支持方式、内置模板的通过情况 | 待本版证实 |
| `SPIKE-LOGPROB` | 被测端点是否返回首个输出 token 的对数概率、换算后的排序质量、`chat-logprobs` 是否开放 | 换算路径已在对话端点替身上验证（主文档 §8.8）；真实端点与排序质量待本机升级验证 L8，`chat-logprobs` 在此之前不开放 |
| `SPIKE-CLS-BATCH` | 被测分类端点的时延分布与批量上限，默认值是否调整 | 超时、超限、批量的处置已在分类端点替身上验证（主文档 §8.8）；时延分布与默认值待本机升级验证 L8 |

把现有的分类服务接到概率判断协议上，通常只需要一个转换程序：接收协议请求，调用分类服务，把结果换成每题一个 `p_yes`。转换程序是用户自己的部署，不在 APA 的发布物里；仓库里用于验收的分类端点替身可作起点（主文档 §18）。
