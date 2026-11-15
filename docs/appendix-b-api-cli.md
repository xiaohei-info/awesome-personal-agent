# 附录 B：API 与 CLI 清单

> 主文档 §17 的逐条清单，分组级语义见主文档，字段级契约在实现时随代码落地。路径省略版本前缀 `/v1`；`/ingest/` 不带版本。v0.2.0 的增改都是加法：已有命令、路由和字段的含义不变，例外逐条写在 B.6。
>
> **规划标记**：说明开头带 `【规划中（v0.2.0）】` 的条目（本附录 B.1、B.2、B.4，以及附录 C.14）属于 v0.2.0 设计、尚未实现。对照测试（`tests/conformance`、`cli/serve`）对它们只放行"文档有、实现没有"；条目一旦实现，测试要求在同一个变更里去掉标记。没有标记的条目要求文档与实现一致。发布前标记必须清零，以严格模式（环境变量 `APA_CONFORMANCE_STRICT=1`）运行对照测试验证（主文档 §20.4 门槛 1）。

## B.1 CLI

"最低凭据"指能调用该命令的最低级别；运行凭据按 worker 处理，并受 §14.3 的范围限制。

| 命令 | 作用 | 最低凭据 | 关键退出码 |
|---|---|---|---|
| **入门** | | | |
| `apa init [--reset-password]` | 建立数据目录，设置 Web 登录口令（不少于 12 个字符）；`--reset-password` 只重设口令，账本不动（主文档 §14.13） | 主机本地 | |
| `apa serve` | 启动内核与 Web | 主机本地 | |
| `apa login` | 保存 APA 地址与 token | 任意 token | 3 |
| `apa guide [主题] [--template]` | 输出使用说明，只写机制；`--template` 输出常驻指令模板 | 无 | |
| `apa declare <文件>` | 提交插件声明（无头运行；旧插件的 `executors[]` 仍被接受，确认后并入执行者名录），本人在 Web 确认后生效 | worker | 4 |
| `apa version` | 版本号、构建提交、各模块数据结构版本与各协议版本 | 无 | |
| `apa doctor [--json] [--upgrade-check]` | 自检（主文档 §15.5）；`--upgrade-check` 是升级前的只读预检（主文档 §16.5） | 主机本地；内核一侧的检查需 observer | 1 |
| `apa backup create/verify/restore` | `create [--out <目录>] [--no-secrets]` 备份；`verify <备份>` 校验；`restore <备份> --home <目录>` 恢复到空目录（主文档 §16.5） | 主机本地 | 1、6 |
| **上下文** | | | |
| `apa context [--session] [--scope project/cwd/item/all] [--item <id>] [--format inject]` | 账本增量投影，含路由为注入的投递和挂靠消息；`--scope item` 见主文档 §7.6 | observer | 3、5、7 |
| `apa persona [--lang zh\|en]` | 读取当前人格，供插件在新会话开始时注入；本人没写时是内置默认语气（主文档 §11.2）；语言：`--lang` > Web 里选的助手语言 > `LC_ALL`、`LC_MESSAGES`、`LANG` > 英文 | observer | 3、7 |
| `apa inbox` | 列出待决和建议；web_only 初筛未返回的决策请求不列出 | observer | |
| `apa today` | 今天的清单：计划今天或更早且未完成的、今天到期的、待决的、久未推进的（主文档 §4.1） | observer | |
| **事项** | | | |
| `apa propose` | 以 `proposed` 新建 | observer | |
| `apa item show/list` | 读事项；`show` 带挂靠消息（按转发白名单过滤）与进展；`list` 可带 `--q <全文>`、`--sort updated/deadline/planned_for/priority`、`--planned-for <日期>` | observer | 5 |
| `apa item add/update/transition/relate [--evidence \| --grant]` | 增改事项、转阶段、关联（`relate --to` 可以是事项或材料；`relate --key <键> [--domain <去重域>] [--remove]` 登记或移除关联键，主文档 §9.4）；`add`、`update` 可带 `--planned-for <日期>` | worker（自己的事项）；delegate（绑定的事项和以 `proposed` 新建不要求依据；其他事项附 `--evidence` 或 `--grant`） | 2、4、6、23 |
| `apa item progress <id> --text [--on <日期>]` | 追加一条进展，不改阶段；`--on` 不能晚于今天 | worker（自己的事项）；delegate（绑定的事项不要求依据；其他事项附 `--evidence` 或 `--grant`） | 2、4、23 |
| **决策** | | | |
| `apa ask <id>` | 写入决策请求并转 `needs_input` | worker | 4 |
| `apa decide <id> --option [--evidence \| --grant]` | 代答决策；本人原话或授权单二选一 | delegate | 2、4、6、20、22、23 |
| `apa accept <id> [--evidence \| --grant]` | 接受建议；依据同上 | delegate | 2、4、20、23 |
| `apa dismiss <id> [--reason] [--evidence \| --grant]` | 丢弃建议；依据同上；理由选填，缺省时事件理由为 `proposal dismissed` | delegate | 2、4、23 |
| `apa attach <id> --to <事项> [--reason] [--evidence \| --grant]` | 把建议补到已有事项；依据同上；补到之后不能逐条撤销（来源事件已改挂） | delegate | 2、4、5、6、23 |
| `apa proposals apply (--recommended \| --file <处置清单>) [--source] [--project] [--evidence \| --grant]` | 批量处理建议：按推荐，或逐条给出处置；每条各自成败，输出逐条结果 | delegate | 2、4、23 |
| `apa grant list` | 列出调用者能用的有效授权单：id、范围、动作、有效期。delegate 是凭据名下的单加上签给凭据所属 Agent 的单；运行凭据只有签给本次运行所属 Agent 且允许后台使用的单。签发和撤销只在 Web | delegate、运行凭据 | 4 |
| **委派** | | | |
| `apa executors [--project]` | 列出执行者名录（主文档 §6.17），带项目时标明是否允许 | observer | |
| `apa delegate --parent --assignee` | 建子事项，校验项目允许的执行者。`--executor <JSON 文件或 ->`（`kind`、`host`、`label`、`handle`）在同一事务里登记或更新执行者，`harness` 取 `APA_HARNESS`；`assignee` 不必先在名录里；身份字段冲突或名录已满返回 6 | worker（自己的事项）；delegate（任意） | 2、4、5、6、21 |
| `apa dispatch add <子事项> [--session] [--resumable] [--cwd] [--repo] [--handle <JSON 文件或 ->] [--continues <记录>] [--idempotency-key]` | 在委派子事项上写一条委派记录，输出该记录；`harness` 取 `APA_HARNESS`，必填；`--resumable` 要有 `--session`；同一子事项上已有运行中、harness 与会话都相同的记录时不新增，返回已有记录；句柄规则见主文档 §6.17 | 同 `apa report` | 2、4、5、6、23 |
| `apa dispatch update <记录> [--state ended\|lost] [--resumable true\|false]` | 推进委派记录；`lost` 同时置 `resumable` 为假，终态不可改 | 同 `apa report` | 2、4、5、6、23 |
| `apa report <id> --outcome --summary [--pr --commit --branch --link] --ack-cursor` | 回执：进展、完成、阻塞，确认挂靠消息；`done` 必须带结果证据（`--pr`、`--commit`、`--branch`、`--link` 至少一项），附不出只能报 `progress` 或 `blocked` | 绑定该 assignee 的 worker、该运行的运行凭据、发起该委派的 delegate；其他 delegate 附 `--evidence` 或 `--grant` | 2、4、6、23 |
| `apa handback <消息> --reason` | 把挂靠消息交回一次 | 同上 | 4、6 |
| **项目** | | | |
| `apa project list/show` | 读项目，含 repos 与允许的执行者 | observer | |
| `apa project create/update` | 建项目、改基本信息。`allowed_executors` 不能改 | delegate | 20 |
| `apa project repos add/remove` | 增改本机检出（worker）或远程地址（delegate） | worker | 4 |
| **材料与规范** | | | |
| `apa material add --repo --path [--project] [--kind]` | 登记托管型；project 缺省取登记了该仓库的项目，kind 缺省 `design` | worker | |
| `apa material commit <id> --from --reason [--kind]` | 提交自有型修订 | worker | 6 |
| `apa material list` | 列出材料，可按项目、kind、标签筛选 | observer | |
| `apa material show <id> [--rev]` | 读当前或指定修订；托管型只回位置 | observer | 5 |
| `apa material search <查询>` | 检索，只回 id、标题、片段 | observer | |
| `apa blob put/get` | 附件 | worker / observer | |
| `apa agents check` | 比对工作区 AGENTS.md | observer | 10、11 |
| `apa agents sync` | 写入工作区；漂移时不覆盖并返回 6，本人选择"覆盖"后才写入 | observer | 6 |
| **来源** | | | |
| `apa source list/show` | 来源与统计 | observer | |
| `apa source add` | 登记附带的拉取程序或 webhook source，可带 `--default-project`、`--name`（显示名；不带时取来源程序接入声明的名字，输出里给出；程序没有声明又不带时退出码 2；重名退出码 6）；自定义命令（含邮件、日历工具的适配命令）与外发范围只在 Web | delegate | 20 |
| `apa source auth <id>` | 以该来源登记的 `credentials_path` 启动附带拉取程序的认证模式（在本机运行 `<来源命令> auth <credentials_path>`，继承标准输入输出），凭据写在该路径；不是 pull 类型或未登记 `credentials_path` 的来源返回 2，工具适配的来源用工具自己的认证命令（主文档 §9.7） | 主机本地，经 API 读来源登记，需 observer | 2、5 |
| `apa source rename <id> <名字>` | 改显示名（主文档 §9.1）；空串改回接入声明的名字（没有声明时退出码 2）；重名退出码 6；记 `source.renamed` | delegate | 6 |
| `apa source enable/disable` | 启停 | delegate | |
| `apa source webhook create/rotate/revoke` | 创建、轮换、吊销 webhook 接收密钥 | delegate | |
| `apa source ingests` | 查看原始 payload、校验结果、转换结果 | observer | |
| `apa source rejects <id> [--after N] [--limit N]` | 查看没有入账的输出行及原因（`next_after` 作为下一次的 `--after`） | observer | |
| `apa source replay <入站记录>` | 重放 | delegate | |
| `apa source backlog list [--source] [--sort score]` | 列出积压（未判定与已到期），旧原因 `undecided`、空原样返回；`--sort score` 按影子分数从高到低排（各来源合并后统一排序，同分保持来源顺序），每条带 `score`，没有分数的排最后 | observer | |
| `apa source backlog rules [--source] --evidence` | 积压只跑规则：重走归属与确定性规则，能挂靠的挂靠、规则有明确出口的按出口处置，其余原样；不调模型，不受 `unjudged_rejudge_days` 限制，输出 `attached`、`disposed`、`remaining`（主文档 §9.3） | delegate | 23 |
| `apa source backlog score [--source] --evidence` | 影子打分：用判断端点给积压按批（每批不超过判断端点的 `max_batch`）打分，只写带 `shadow` 的判断记录，不改处置、不建事项、不问对话端点；输出 `scored`、`skipped`（外发范围不允许送出的，或判断端点没有给出概率的）；没有判断端点时返回 6（说明没有判断端点），不写记录；模型已暂停时返回 6（`models_paused`），什么也不做（主文档 §8.7、§9.3） | delegate | 23 |
| `apa source backlog rejudge [--source] [--limit N] --evidence` | 放开重判：剩余积压（含已 `expired` 与旧原因）按分数从高到低（没有判断端点时按落盘顺序，由对话端点逐条回答同一道初筛题）走完整流水线；进入建议流的数量受每日上限 `backlog_release_per_day` 约束，被忽略的不占额度，当天用完只推迟、次日继续；`--limit N` 把本次进入建议流的数量再压到 N 以内；输出 `requeued`、`deferred`；模型已暂停时返回 6（`models_paused`），不放回任何事件（主文档 §8.7、§9.3） | delegate | 23 |
| `apa source backlog restore --from <时间> --to <时间> [--source] [--dry-run] --evidence` | 把时间段内被旧级联的判断忽略、没有判断记录的事件放回积压（未判定原因 `restored`），之后只经上面三步重判（主文档 §9.3）；`--dry-run` 只返回命中条数与按来源的分布，不写入，不需要 `--evidence` | delegate（`--dry-run` 为 observer） | 23 |
| **对外** | | | |
| `apa events --follow [--cursor]` | 跟随事件流 | observer | |
| `apa ext list` | 列出仓库提供的 ext | 无 | |
| `apa ext run <名>` | 本机启动 ext | ext 密钥（由 ext 读取） | |
| `apa ext check` | 协议一致性自测 | 无 | |
| **自动化** | | | |
| `apa instance add/list/enable/disable` | 能力包实例 | delegate（`list` 为 observer） | |
| `apa pack list` | 列出已加载能力包 | observer | |
| `apa pack validate <目录>` | 本地校验能力包 | 无 | |
| **可观测** | | | |
| `apa why <id>` | 回放事项或运行 | observer，按权限裁剪 | 5 |
| `apa health` | 健康视图 | observer | |
| `apa run list/show` | 运行记录 | observer | |
| `apa judgment stats [--use] [--template] [--source] [--since]` | 各级调用次数与升级率、未定率、接受率、误丢率（主文档 §8.10） | observer | |
| `apa judgment show <id>` | 一条判断记录的逐级结果 | observer，按权限裁剪 | 5 |
| `apa judgment spot-check` | 列出待抽检的条目；抽检结论只在 Web 填 | observer | |

**只在 Web**：策略、信任级、token、ext 密钥、web_only 分类、两份白名单、项目允许的执行者、移除执行者名录条目、规则切 `auto`、运行权限上限、判断端点与生成端点、不确定带与影子模式、抽检结论、打扰上限与静音、来源与对话捕获的外发范围、登录口令与在线会话、可执行命令登记（另可由本人写主机配置文件，§14.12）、撤销 agent 变更、物理删除、规范生效、签发和撤销授权单。CLI 没有对应的写命令；相关命令（如 `apa project update`、`apa source add`）带上这些字段时返回退出码 20。

**代理写命令清单**：delegate 调用写命令时按动作归属分三档（主文档 §14.5）。其他凭据与 Web 本人不经这张表；只读命令不在清单内。

| 档 | 依据 | 缺失时 |
|---|---|---|
| 一：agent 自己的工作 | 凭据身份 + 对象绑定，不要求本人原话；带了 `--evidence` 照常记录 | 绑定不成立的回执、交回和事项修改按第二档判定；`delegate.report` 报 `done` 缺结果证据为退出码 2 |
| 二：替本人做决定 | 二选一：`--evidence`（`X-APA-Evidence`）本人原话 + `APA_SESSION`（`X-APA-Session`）会话引用，且会话类型 `APA_SESSION_KIND`（`X-APA-Session-Kind`）为 `direct`；或 `--grant <id>`（`X-APA-Grant`）授权单。同时携带为退出码 2 | 退出码 23，不写入；`open`、`waiting` 的事项转 `needs_input`，其余阶段不变；见主文档 §14.5 |
| 三：高风险与设置类 | 只在 Web，见上文"只在 Web"；web_only 决策同样在这一档 | 退出码 20 |

| 模块 | 第一档 | 第二档（可用授权单） | 第二档（只接受本人原话） |
|---|---|---|---|
| 事项 | `item.create`（以 `proposed` 新建）；`item.update`、`item.transition`、`item.relate`、`item.progress`（绑定的事项） | `item.create`（其他阶段）；`item.update`、`item.transition`、`item.relate`、`item.progress`（其他事项） | |
| 对话捕获 | `inbound.capture` | | |
| 决策 | `decision.request` | `decision.answer`、`proposal.accept`、`proposal.attach`、`proposal.dismiss`、`proposal.batch` | |
| 材料 | `material.commit`、`material.add`、`blob.put` | `revision.confirm`、`revision.reject` | |
| 委派 | `delegate.create`；`delegate.report`、`delegate.dispatch`、`delegate.dispatch_update`、`followmsg.handback`（绑定的事项）；`executor.declare` | `delegate.report`、`delegate.dispatch`、`delegate.dispatch_update`、`followmsg.handback`（其他事项） | |
| 项目 | `project.checkout.set`、`project.checkout.remove` | | `project.create`、`project.update`、`project.repo.add`、`project.repo.remove` |
| 来源与能力包 | | | `source.register`、`source.update`、`source.set_enabled`、`source.webhook_key`、`source.webhook_revoke`、`source.replay`、`inbound.backlog_rules`、`inbound.backlog_score`、`inbound.backlog_rejudge`、`inbound.backlog_restore`、`instance.create`、`instance.update`、`instance.toggle`、`instance.run` |

授权单的 `actions` 只能取"可用授权单"一列的命令。运行凭据只能使用签给本次运行所属 Agent（`grantee.agent` 等于运行记录的 harness）且允许后台使用的单，`apa grant list` 与 `GET /grants` 对运行凭据只列出这些单（凭据归属某个 Agent 的 delegate 另能看到签给该 Agent 的单），对象须同时在运行范围内，触发不可信时为退出码 23（`untrusted_trigger`）；`grantee.agent` 式的单放行 `proposal.accept`、`proposal.attach` 时，建议须来自可信发件人（`untrusted_origin`，主文档 §14.5）。

**会话身份**：环境变量 `APA_SESSION`、`APA_HARNESS`、`APA_SESSION_KIND` 由接入方（harness 插件）设置，CLI 随每次请求以 HTTP 头 `X-APA-Session`、`X-APA-Harness`、`X-APA-Session-Kind` 发出。前两项让 Event actor 审计到会话粒度；`X-APA-Session-Kind` 取 `direct`、`group`、`unknown`，缺失等同 `unknown`，只有 `direct` 时内核接受凭本人原话的第二档命令（主文档 §11.2、§14.5、§17.4）。

## B.2 HTTP API

| 分组 | 方法与路径 | 说明 | 凭据 |
|---|---|---|---|
| 事项 | `GET/POST /items`，`GET/PATCH /items/{id}` | 查询、创建、更新。`GET /items` 可带 `q`（全文）、`sort`（`updated`、`deadline`、`planned_for`、`priority`）、`phase`、`project`、`tag`、`assignee`、`planned_for` | 按 §14.3；导出 ext 密钥可 `GET` |
| | `GET /today` | 今天的清单，四组：`planned`、`due`、`pending`、`stale` | observer |
| | `POST /items/{id}/progress` | 追加进展 `{text, on?}`；`GET /items/{id}` 返回 `progress[]` | worker（自己的事项）；delegate 按 §14.5 |
| | `POST /items/{id}/correlation-keys`，`DELETE /items/{id}/correlation-keys` | 登记或移除关联键 `{key, domain?}`（经 `item.relate`；`GET /items/{id}` 返回 `correlation_keys[]`） | 同 `relations` |
| | `GET /items/{id}/messages` | 事项的全部挂靠消息 | observer；运行身份和无人值守事项按转发白名单过滤；ext 密钥不可用 |
| | `POST /items/{id}/transition`，`POST /items/{id}/relations` | 转阶段、关联 | 同上 |
| | `POST /items/{id}/revert` | 撤销 agent 变更 | 只有 Web 登录 |
| 决策 | `POST /items/{id}/ask` | 写入决策请求 | worker |
| | `POST /items/{id}/decide`，`/accept`，`/dismiss` | 回答或处理建议；delegate 带 `X-APA-Evidence` 或 `X-APA-Grant`，二选一 | Web 登录、delegate |
| | `POST /items/{id}/attach` | 把建议补到已有事项，请求体 `{to, reason?}`；依据同上 | Web 登录、delegate |
| | `POST /proposals/batch` | 批量处理建议，见下 | Web 登录、delegate |
| | `GET /grants` | 调用者能用的有效授权单（同 `apa grant list`） | delegate、运行凭据 |
| 委派 | `GET /executors` | 执行者名录：`id`、`kind`、`host`、`harness`、`label`、`handle`、`created_at`；带 `project` 时另带 `allowed` | observer |
| | `DELETE /executors/{id}` | 移除名录条目，已有事项与委派记录里的 id 原样保留 | 只有 Web 登录 |
| | `POST /delegations` | 建子事项 | worker |
| | `POST /items/{id}/report` | 回执 | worker |
| | `POST /items/{id}/dispatches` | 写委派记录 `{session, resumable?, cwd?, repo?, handle?, continues?}`，返回 201 与记录 | 同回执 |
| | `PATCH /dispatches/{id}` | 推进委派记录 `{state?, resumable?}` | 同回执 |
| | `GET /dispatches` | 列委派记录，可带 `executor`、`item`、`resumable`、`limit` | observer；运行凭据限范围内的事项 |
| | `POST /messages/{id}/handback` | 交回挂靠消息 | worker |
| 项目 | `GET/POST /projects`，`GET/PATCH /projects/{id}` | 项目 | observer 读，delegate 写 |
| | `POST /projects/{id}/repos` | repos 属性 | worker、delegate |
| | `GET /projects/resolve?path&host` | 当前目录到项目的映射 | observer |
| 材料 | `GET/POST /materials`，`GET /materials/{id}` | 材料；单个材料带与事项的类型化关系 `relations` | observer 读，worker 写；导出 ext 密钥可 `GET` |
| | `GET /materials/{id}/content` | 托管型正文，异步读取 | 只有 Web 登录 |
| | `POST /materials/{id}/changes` | 对托管型发起修改，生成委派子事项 | Web 登录、delegate；worker、运行凭据须带自己范围内的 `parent` |
| | `GET/POST /materials/{id}/revisions` | 修订 | observer 读，worker 写 |
| | `POST /materials/{id}/revert` | 回退一次 `direct` 修订 | 只有 Web 登录 |
| | `POST /revisions/{id}/confirm`，`/reject` | 确认或拒绝 proposed 修订 | Web 登录、delegate（规范生效只有 Web） |
| | `GET /materials/search` | 检索 | observer |
| | `PUT/GET /blobs/{hash}` | 附件 | worker 写，observer 读 |
| 规范 | `GET /agents-md?repo` | 取渲染结果，带最近一次检查状态 | observer |
| | `POST /agents-md/check` | 上报文件 hash，返回三态 | observer |
| 上下文 | `GET /context` | 账本增量投影 | observer |
| | `GET /context/persona` | 当前人格，前面带内核固定声明；本人没写时是内置默认；语言：查询参数 `lang`（`zh` 或 `en`）> 策略的 `persona_lang` > 查询参数 `locale`（调用方的区域设置，`zh` 开头为中文）> 英文；另带 `default`，即该语言的默认原文（不含声明） | observer |
| | `POST /capture` | 提交对话增量 `{text, project?, path?, host?}`；运行身份下、项目未开启捕获时不生效，返回 200 `{"captured": false, "reason"}`；候选在提交后异步抽取 | worker |
| 插件声明 | `POST /declarations` | 提交无头运行声明（旧插件可带 `executors[]`），进 Web 待确认 | worker |
| 来源 | `GET/POST /sources`，`GET/PATCH /sources/{id}` | 来源登记 | observer 读，delegate 写；自定义命令只有 Web |
| | `GET /sources/declared-name?type&command&source_type&config` | 来源程序接入声明的显示名（中英），新登记表单预填 | observer |
| | `POST /sources/{id}/webhook` | 创建或轮换接收密钥 | delegate |
| | `DELETE /sources/{id}/webhook` | 吊销该来源全部接收密钥，同时删除 HMAC 密钥文件 | delegate |
| | `GET /sources/{id}/ingests`，`POST /ingests/{id}/replay` | 入站记录与重放 | observer、delegate；运行凭据不能读入站记录（返回 403，原始请求体未经转发白名单） |
| | `GET /sources/{id}/events?disposition&sort&after&limit` | 来源事件，`disposition` 可逗号分隔多个；内部来源 `capture:<项目>` 只有 Web 登录能读 | observer；运行凭据不可用（返回 403，挂靠消息经转发白名单读取） |
| | `GET /sources/{id}/rejects?after&limit` | 被拒收的输出行及原因 | observer；运行凭据不可用（返回 403，原始行是未过滤的外部输出） |
| | `POST /source-events/{id}/propose` | 把未判定事件或 digest 事件转为建议 | Web 登录、delegate |
| | `POST /source-events/{id}/ignore`，`POST /source-events/ignore` | 忽略未判定事件：单条；或批量，请求体 `{"source"}` 按来源、`{"all": true}` 全部 | Web 登录、delegate（附本人原话） |
| | `POST /source-events/rules` | 积压放开第 1 步：只跑规则，返回 `{"attached", "disposed", "remaining"}`；请求体 `{"source"?}` | Web 登录、delegate（附本人原话） |
| | `POST /source-events/score` | 积压放开第 2 步：影子打分，返回 `{"scored", "skipped"}`；请求体 `{"source"?}`；没有判断端点是 409；模型已暂停是 409 `models_paused` | Web 登录、delegate（附本人原话） |
| | `POST /source-events/rejudge` | 积压放开第 4 步：放开重判，返回 `{"requeued", "deferred"}`；请求体 `{"source"?, "limit"?}`；模型已暂停是 409 `models_paused` | Web 登录、delegate（附本人原话） |
| | `GET /source-events/restorable?from&to&source` | 预览：时间段内可放回积压的旧判断忽略事件的条数与按来源的分布 | observer；运行凭据不可用 |
| | `GET /source-events/{id}/outbound-preview` | 外发预览：这条事件按 `full`、`excerpt`、`meta` 裁剪后实际送给模型的内容，与外发同源（主文档 §14.14）；只读，不写 Event | observer；运行凭据不可用（返回 403）；内部来源 `capture:<项目>` 的事件只有 Web 登录能读 |
| | `POST /source-events/restore` | 把旧级联忽略的事件放回积压；请求体 `{"from", "to", "source"?}` | Web 登录、delegate（附本人原话） |
| 判断 | `GET /judgments?subject&use&template&source&after&limit`，`GET /judgments/{id}` | 判断记录（主文档 §6.16），不含资料正文 | observer；运行凭据限目标事项 |
| | `GET /judgments/stats?use&template&source&since` | 各级调用次数与四项指标（主文档 §8.10） | observer |
| | `GET /judgments/spot-check`，`POST /judgments/{id}/review` | 抽检池；填抽检结论 `{result: "should_see" \| "correct"}` | 读 observer；填只有 Web 登录 |
| 入站 | `POST /ingest/{source_id}` | webhook 接收路由 | webhook 接收密钥 |
| 事件 | `GET /events?cursor=` | SSE 或长轮询 | observer、导出和转发 ext 密钥 |
| 投递 | `GET /deliveries/next?wait=` | 长轮询领取；有租约 | 渠道 ext 密钥；网关插件 token（worker 及以上，只领唤醒类投递） |
| | `POST /deliveries/{id}/ack`，`/fail`，`/answer` | 送达、失败、转达回答 | 同上 |
| | `POST /channels/identify` | 渠道 ext 上报谁发来了识别口令：`{code, sender, display}`。`sender` 是平台用户 ID；ext 密钥的渠道须与发起识别的渠道一致，口令不符、已用过或过期返回 404。不写名单 | ext（channel） |
| | `GET /deliveries/unread`，`POST /deliveries/{id}/read` | Web 未读；`read` 即本人确认（投递转 `acknowledged`） | Web 登录 |
| | `GET /deliveries/today` | 今天的推送计数，与打扰日上限同口径（主文档 §7.4）；只读 | Web 登录 |
| | `GET/POST /forms/{一次性令牌}` | 一次性表单 | 令牌本身 |
| 运行 | `GET /runs?state&category&target&before&limit`，`GET /runs/{id}` | 运行记录 | observer |
| | `POST /runs/{id}/credential` | 把唤醒运行的 id 换成短期运行凭据 | worker、delegate（网关插件 token） |
| 回放与健康 | `GET /why/{id}`，`GET /health` | 回放（来源事件命中数值条件规则时另带 `alerts`：规则、条件文本、上一次与本次取值、产出的投递及其状态（提醒还被静默或合并窗口暂存时状态为 `held`），各凭据级别可见）、健康视图；所有健康告警都经同一处产出健康类投递（§15.2），读取走 `GET /deliveries/unread` | observer |
| | `GET /doctor` | 内核一侧的自检结果（主文档 §15.5）；每项检查与每个对象都带稳定机器码 `code` 与可变参数 `data`，形状见下文"自检结果" | observer |
| | `GET /version` | 版本与各协议版本（B.5） | 不需要凭据 |
| 能力包 | `GET /packs`，`GET/POST /instances`，`PATCH /instances/{id}` | 能力包与实例。`GET /packs` 的每个包另带 `default_instruction`（实例驱动的 `call_agent` 模板的默认内容要求，没有时省略，附录 C.7）；`GET /instances` 另带顶层 `agent_ready`（Web 设置了默认 Agent 且其无头运行声明已确认；为假时周期简报到点跳过，主文档 §7.3）。实例参数 `instruction` 不超过 2000 字 | observer 读，delegate 写 |
| | `POST /instances/{id}/run` | 立即跑一次（`instance.run`）：以当前时刻为到期时刻记 `instance.fired`，与定时到期同一路径，含 Agent 不能后台运行时的跳过；返回 202 与实例 | delegate |
| | `GET /rules`，`PATCH /rules/{id}/mode` | 规则与模式；每条带 `class`（`reminder` 提醒类或 `action` 动作类，由加载器推定，主文档 §14.8）；实例驱动的时钟规则（`schedule` 含 `{instance.…}`）不是可见规则，不列出，对它改模式与规则不存在同为 404 `not_found`（主文档 §7.3）；切换只有 Web | Web 登录 |
| 设置 | `/settings/policy`、`/settings/tokens`、`/settings/ext-keys`、`/settings/allowlists`、`/settings/executors`、`/settings/commands`、`/settings/declarations`、`/settings/model-endpoints`、`/settings/access-limits`、`/settings/projects/{id}/allowed-executors`、`/settings/grants`、`/settings/purge` | 策略（含人格、打扰上限与静音）、两个模型端点、凭据、白名单、名单、命令登记、声明确认、授权单签发与撤销（`POST /settings/grants`、`POST /settings/grants/{id}/revoke`）、物理删除 | 只有 Web 登录 |
| | `/settings/judgment` | 不确定带与影子模式 | 只有 Web 登录 |
| | `POST /settings/model-endpoints/models`、`POST /settings/model-endpoints/test` | "设置 › 模型"的获取模型与测试连接：只读，不写 Event，不改策略；同一类端点同一时刻只跑一个，其余返回 409（主文档 §4.1、§15.5） | 只有 Web 登录 |
| | `/settings/sources/{id}/outbound`、`/settings/projects/{id}/capture-outbound` | 来源与对话捕获的外发范围 | 只有 Web 登录 |
| | `PUT /settings/sources/{id}/trusted-senders` | 只替换这个来源的转发白名单 `{"senders": [...]}`，经 `policy.Update`，记 `policy.changed` 与 `allowlist.changed`；来源声明为需确认时只接受空名单（清掉以前填的、不生效的条目），非空为 400（`sender_unverifiable`）；名单为空即策略里没有这个键，Web 再次按"从未设置"预填本人 | 只有 Web 登录 |
| | `GET /settings/sources/{id}/self-sender` | 声明了 `sender.self: identity` 的来源：运行一次来源程序的 `identity` 模式（登记命令的第一个词加 `identity`，超时 10 秒），返回 `{"sender"}` 供预填；不产生来源事件、不写 Event；其他来源 404，程序失败或没给出身份时 409 `conflict` | 只有 Web 登录 |
| | `PUT /settings/channels/{name}/answerers` | 只替换这个渠道的回答人白名单 `{"answerers": [...]}`，记 `policy.changed` 与 `allowlist.changed` | 只有 Web 登录 |
| | `POST /settings/channels/{name}/identify`，`GET /settings/channels/{name}/identify` | 开始识别：返回一次性口令 `{code, expires_at}`（10 分钟，同一渠道同时一个，只在内存里，重启作废）；查看：`{state: waiting \| got \| expired, sender?, display?}` | 只有 Web 登录 |
| | `PATCH /settings/tokens/{id}` | 认领 token 所属的 harness `{"harness"}`（非空、不含空白，同无头运行声明的 `harness`）：只能从空设一次，已有值或已吊销为 409（`credential.assign`，记 `credential.assigned`，载荷 `{"harness"}`）；只对 worker、delegate。`POST /settings/tokens` 可带同一字段，在签发时给定；其他种类带它为 400。`GET /settings/tokens` 每条带 `harness`（空为未认领）与 `seen_harness`（该凭据最近一次事件里自报的 harness，没有时省略，只作认领推荐，不参与判定） | 只有 Web 登录 |
| | `PUT /settings/agents/{harness}/grant` | 替换该 Agent 的长期授权单：同一事务撤销它名下现有的 `grantee.agent` 式有效单（撤销原因 `replaced`）并按请求体 `{grantee: {background}, scope, actions, expires_at?, reason?}` 签发新单，记 `grant.revoked` 与 `grant.issued`；`actions` 为空即只撤销。响应 `{"grant": 新单或 null, "revoked": [被撤销的单 id]}`；新单校验失败为 400，旧单不动 | 只有 Web 登录 |
| 认证 | `POST /auth/login`、`/logout` | Web 会话。连续失败的限速与锁定见主文档 §14.13，锁定期间返回 429 | |
| | `POST /auth/password` | 改口令 `{current, new}`；新口令少于 12 个字符返回 400；成功后其他会话全部失效 | 只有 Web 登录 |
| | `GET /auth/sessions`，`DELETE /auth/sessions/{id}`，`DELETE /auth/sessions` | 在线会话与吊销 | 只有 Web 登录 |
| 动作链接 | `GET /act/{item}?do=done\|progress\|plan` | 不属于 API：Web 的确认页地址，不带版本前缀，不含凭据；未登录时先登录再回到确认页（主文档 §4.1、§14.13） | Web 登录 |

事项字段 `deadline`：`POST /items` 与 `PATCH /items/{id}` 可写，值为 RFC 3339 时间；`PATCH` 传 `null` 清除；`GET` 在已设置时返回。行为见主文档 §6.8。

事项字段 `planned_for`：`POST /items` 与 `PATCH /items/{id}` 可写，值为 `YYYY-MM-DD`（本机时区的日期），`PATCH` 传 `null` 清除；`GET` 在已设置时返回。它不产生定时与提醒（主文档 §6.3）。

事项字段 `proposal`：只读，由入站流水线写入：`{recommend, attach_to?, reason, low_confidence, sources[]}`（主文档 §6.3）。`GET /items?phase=proposed` 可带 `sort=score` 按初筛分数排序。

`POST /items/{id}/progress` 的请求体 `{"text": "…", "on": "YYYY-MM-DD"}`，`on` 可省略、不能晚于今天；成功返回 201 与这条进展，Event 为 `item.progress_added`。进展不改阶段，不推进 `version` 以外的字段。

`GET /items` 返回 `last_rejections`（事项 id → 最近一次未处理的退出码 23 拒绝：`seq`、`at`、`actor`、`code`、`why`、`grant`），`GET /items/{id}` 返回 `last_rejection`；只对 `needs_input`、`proposed` 事项给出，决策待办与建议流据此标明被拒绝的代答或处理。`GET /audit/events` 可加 `grant=<授权单 id>` 查该授权单的签发、使用与撤销记录。

事项字段 `created_at`、`updated_at`：`GET /items` 与 `GET /items/{id}` 返回，值为 RFC 3339 时间，只读。`created_at` 是该事项首条事项事件的时间（委派子事项的首条是 `delegation.created`，其余是 `item.created`）；`updated_at` 是最后一条事项事件的时间，与 `version` 同步推进（含关系变更；`item.deadline_passed` 与 `command.rejected` 不算）。

来源的显示名 `name`（主文档 §9.1）：`POST /sources` 可带，去掉首尾空白后不超过 40 个字符、不含控制字符，与实例内其他来源不重名（不区分大小写），重名返回 409 `conflict`；不带时取来源程序接入声明的名字（中文）存下，响应里给出；程序没有声明又不带时返回 400 `invalid_request`，`next` 说明带名字或在声明里补上；声明出的名字被占用同样返回 409，不自动加序号。`PATCH /sources/{id}` 带 `name` 即改名（`source.rename` 命令，记 `source.renamed`，载荷 `{"name", "previous"}`），空串改回接入声明的名字（没有声明时 400）；名字与其他字段同一请求里给出时分别生效。`GET /sources` 与 `GET /sources/{id}` 总带 `name`，另带发件人结论 `sender: {"trust": "verified" | "unverified", "hint"?: {"zh", "en"}, "self"?: "identity"}`（主文档 §9.1）：有接入声明按声明；没有声明的只用映射的 webhook 来源，所用映射里有按验签核实发件人的为 `verified`；其余为 `unverified`。`GET /sources/declared-name?type=&command=&source_type=&config=`（`config` 是 JSON 对象，非法时 400）按同一套声明返回 `{"name": {"zh", "en"}}`，没有声明时 `{"name": null}`，供 Web 新登记表单按界面语言预填；它只读，不登记。

`POST /sources`、`PATCH /sources/{id}` 的 `credentials_path` 必须落在该来源自己的目录（`<APA_HOME>/sources/<id>/`）内，否则返回 400。内核只记录这个路径，不读内容；每次拉取时经 stdin JSON 的 `credentials_path` 字段原样交给来源进程（主文档 §9.2），不经环境变量传递。

`POST /sources/{id}/webhook` 不带请求体。来源还没有有效密钥时创建，返回 201；已有时轮换，返回 200，旧密钥在 1 小时宽限期后失效；轮换总是作用于当前有效（未吊销、未轮换过）的密钥，已吊销或已轮换过的凭据不能再轮换（内核返回 409）。响应 `{"credential": {...}, "secret": "apa_whk_…", "url": "/ingest/<source_id>", "rotated": false}`，`secret` 只出现这一次，不进幂等结果表，Event 只记凭据 id。校验方案为 HMAC 的来源，明文另写入 `<APA_HOME>/webhook-keys/<source_id>`（主文档 §14.2），没有任何 API 能读回；共享 token 方案只存哈希。

`DELETE /sources/{id}/webhook` 不带请求体，吊销该来源全部有效的接收密钥（含宽限期内的旧密钥）并在同一事务里删除密钥文件，返回 200 `{"revoked": n}`，已无有效密钥时 `n` 为 0。文件删除失败时整个命令回滚、账本不变，返回错误，重试即可。Event 为 `source.webhook_key_revoked`，只记凭据 id。

`GET /sources/{id}/events` 按落盘顺序返回 `{"events": [...], "next_after": 12}`；带 `sort=score` 时按最近一次判断记录的概率从高到低返回（同分保持落盘顺序），没有分数的排在最后；此时 `after`、`next_after` 是已返回的条数（偏移），不是落盘序号；其他 `sort` 值返回 400。每条事件的字段见主文档 §6.10：`id`、`source_id`、`dedupe_key`、`type`、`subject`、`summary`、`detail`、`link`、`occurred_at`、`correlation_keys`、`sender`、`sender_verified`、`disposition`、`item`、`attach_state`、`unjudged_reason`、`rejudges`，以及 v0.2.0 新增的 `metrics`、`merge_key`、`items`、`judgment`、`score`（最近一次判断的概率，没有时省略）。其中 `item` 是挂靠或生成的事项 id，`items` 是一条事件抽出多条候选时的全部事项 id。`disposition` 可选，取值 `pending`、`attached`、`proposed`、`ignored`、`digest`、`alerted`、`unjudged`、`expired`，可逗号分隔多个（来源页的积压用 `unjudged,expired`），其他值返回 400。`unjudged_reason` 取 `model_unavailable`、`models_paused`（本人暂停了模型，主文档 §8.7）、`outbound_unset`、`restored`；v0.1 留下的 `undecided` 与空值原样返回，内核按 `model_unavailable` 对待。`limit` 缺省 100，上限 500；还有下一页时才返回 `next_after`，作为下一次的 `after`。

`GET /sources/{id}/rejects` 按发生顺序返回 `{"rejects": [...], "next_after": 12}`，分页参数与 `events` 相同。每条含 `source_id`、`run_id`、`line`（输出中的行号）、`reason`、`raw`（原始行）、`at`。`raw` 在落账时已截断到 512 字节；含接收密钥明文前缀的行返回 `[redacted]`（与入站记录的请求头同一规则）。来源未登记返回 404。
`capture:<项目>` 是对话捕获的内部来源（主文档 §9.3），不在 `GET /sources` 里，Web 按开启捕获的项目拼出 id。Web 登录读它返回同样的结构，其他凭据返回 404（运行凭据仍是 403）。

`POST /source-events/{id}/propose` 不带请求体，成功返回 200 与更新后的来源事件（`disposition` 为 `proposed`，`item` 为新事项 id），事件不是 `unjudged`、`expired` 或 `digest`（简报"知会"一节的条目）时返回 409，不存在时返回 404。事项阶段为 `proposed`，事项的 `item.created` 与来源事件的 `inbound.disposed` 都记调用者为操作者。

`POST /source-events/{id}/ignore` 不带请求体，成功返回 200 与更新后的来源事件（`disposition` 为 `ignored`），事件不在积压里（不是 `unjudged` 或 `expired`）时返回 409，不存在时返回 404。`POST /source-events/ignore` 的请求体恰好给出 `source` 或 `all: true` 之一，否则返回 400；在一个命令里忽略该来源或全部来源的积压，返回 200 `{"ignored": n}`，没有积压时 `n` 为 0。两者都可带 `Idempotency-Key`，每条被忽略的事件记一条 `inbound.disposed`，操作者是调用者。

`POST /source-events/rules`、`/score`、`/rejudge` 对应主文档 §9.3 的积压放开：请求体可带 `source` 限定来源，`rejudge` 可带 `limit`；都可带 `Idempotency-Key`。它们不受 `unjudged_rejudge_days` 限制，也作用于 `expired` 的事件。响应分别是 `{"attached": n, "disposed": n, "remaining": n}`、`{"scored": n, "skipped": n}`、`{"requeued": n, "deferred": n}`；`deferred` 是因当天进入建议流的数量已到 `backlog_release_per_day` 而留到次日的条数。没有判断端点时 `score` 返回 409。模型已暂停时 `score` 与 `rejudge` 都返回 409，`code` 为 `models_paused`，不碰积压、不调模型；`rules` 不需要模型，照常可用。每条被放回的事件记一条 `inbound.requeued`，操作者是调用者。

`GET /source-events/{id}/outbound-preview` 返回 200 `{"full": {…}, "excerpt": {…}, "meta": {…}}`，三档各是 `{"event_type", "subject", "sender", "occurred_at", "link", "material"}`：前五项是送给每一级的结构化输入（链接只有域名），`material` 是按该范围裁剪的资料正文（聚合事件的消息接在正文后；`full` 另带完整链接，`meta` 为空串）。`none` 与未选择不外发，不在结果里。事件不存在返回 404。

`GET /deliveries/today` 返回 200 `{"pushed", "cap", "suppressed_daily_cap", "suppressed_muted"}`：`pushed` 是当天经渠道推送、计入日上限的决策、提醒与简报条数（不含超限说明），日上限判定用的就是这个数；`cap` 是策略的 `disturb.daily_cap`，`0` 表示不限；后两项是当天被日上限、静音改为只留 Web 未读的条数。自然日按静默时段的时区算，未设置取 UTC。

`GET /rules` 每条规则的 `class` 只在读取时按当前已加载的能力包填写，不存账本；规则已不在加载的能力包里时省略。

`GET /source-events/restorable` 与 `POST /source-events/restore` 对应主文档 §9.3 的"补救旧级联误丢的事件"。`from`、`to` 必填，是 RFC 3339 时刻，按来源事件的落盘时间取左闭右开区间，`from` 不早于 `to` 返回 400；`source` 可选，不存在时返回 404。命中条件：处置为 `ignored`，最近一次处置由内核依判断结论写入（不是规则的 `ignore` 出口，也不是调用者的忽略），且没有判断记录。预览返回 200 `{"matched": n, "sources": [{"source", "count"}]}`，不写入。执行返回 200 `{"restored": n}`：每条命中的事件转 `unjudged`、`unjudged_reason` 为 `restored`，各记一条 `inbound.disposed`，操作者是调用者，载荷带原处置 `ignored` 与本次的 `from`、`to`；不调模型、不建事项。同一时间段再次执行时已放回的不再命中，返回 `{"restored": 0}`；可带 `Idempotency-Key`。`restored` 的事件只经 `/rules`、`/score`、`/rejudge` 重判，不参与自动重判，也不转 `expired`。

`POST /proposals/batch` 的请求体二选一：`{"mode": "recommended", "filter": {"source"?, "project"?, "ids"?}}` 按推荐处理范围内带推荐的建议；或 `{"mode": "explicit", "actions": [{"id", "do": "accept" \| "attach" \| "dismiss", "to"?, "reason"?}]}`。响应 200 `{"results": [{"id", "do", "ok", "code"?, "why"?}]}`，每条各自成败、各记一条 Event；一次至多 200 条。delegate 调用时依据作用于整批，授权单逐条校验范围。

`POST /items/{id}/attach` 的请求体 `{"to": "<事项 id>", "reason"?: "…"}`：建议转 `dropped`，它的来源事件改挂到目标事项，Event 为 `proposal.attached`；目标已终结返回 409，不存在返回 404（主文档 §7.2）。

`GET /judgments` 返回 `{"judgments": [...], "next_after": "<记录 id>"}`，按判断时刻升序；`after` 是上一页最后一条的 id，有下一页时才返回 `next_after`，`limit` 缺省 100、上限 500，`after` 不是已知记录 id 或 `limit` 越界返回 400。运行凭据列表必须带 `subject`，且只读目标事项范围内的记录：`GET /judgments/{id}` 读范围外的记录返回 404（CLI 退出码 5），列表返回空；ext、webhook 凭据返回 403。记录过了保留期（`judgment_retention_days`，缺省 180 天）被清理后返回 404，引用它的命令 Event 与事项不受影响。`GET /judgments/{id}` 返回主文档 §6.16 的字段。`GET /judgments/stats` 返回 `{"rows": [{"use", "template", "source"?, "backend", "calls", "decided", "uncertain", "errors", "reused"}], "rates": [{"use", "template", "source"?, "escalation", "undecided", "acceptance", "false_drop", "reviewed"}], "histogram": [...]}`；`since` 缺省 30 天。

`GET /health` 返回 `{"healthy": bool, "signals": [...]}`，信号恒为十三项且顺序固定（主文档 §15.2）：`source_failures`、`outcall`、`delivery_backlog`、`chain_trips`、`agentsmd_drift`、`pack_rejections`、`model_unavailable`、`unjudged_backlog`、`rule_fallbacks`、`receipt_failures`、`other_alerts`、`stalled_delegations`、`clock_lag`。前十一项的 id 与顺序同 v0.1，后两项是 v0.2.0 追加的，都是 `warning`，不影响 `healthy`。每项 `{id, ok, count, severity?, why?, next, detail?}`；`severity` 只在异常时给出，取 `error` 或 `warning`。`unjudged_backlog` 是 `warning`，不影响 `healthy`；它的 `detail` 为 `{"model_unavailable": n, "models_paused": n, "outbound_unset": n, "undecided": n, "restored": n, "expired": n, "rejudge_days": n, "sources": [{"source", "count"}]}`，`undecided` 只统计 v0.1 留下的事件，`expired` 只作参考、不计入 `count`；`rejudge_days` 是运行配置 `unjudged_rejudge_days` 的值，`0` 表示自动重判与到期都关闭（主文档 §9.3），Web 据它说明积压会不会自动判断。`model_unavailable` 的 `detail` 为 `{"judge": s, "generate": s, "endpoint": s, "paused": b}`，`judge`、`generate` 各取空、`not_configured`、`failing` 或 `paused`；`endpoint` 是 v0.1 的字段，保留，取两者中较差的一个。本人暂停了模型时 `paused` 为 true，`judge`、`generate` 都是 `paused`，`endpoint` 为空，信号正常（主文档 §8.7）。

`GET /items/{id}/messages` 返回 `{"messages": [...]}`，按落盘顺序。每条字段：`id`、`source`、`sender`、`sender_verified`、`subject`、`summary`、`detail`、`link`、`occurred_at`、`attach_state`、`forward_allowed`（是否进转发白名单）。只读，不推进 `attach_state`。运行身份和 `unattended` 事项的 worker、observer 只拿到 `forward_allowed` 为真的消息；Web 登录和 delegate 拿到全部。web_only 初筛中的事项对非 Web 凭据返回空列表。事项不存在返回 404。`apa item show` 把这个列表合进输出的 `messages`。

`GET /context?scope=item[&item=<id>]` 在普通字段外返回 `messages`：`[{"item": "...", "cursor": "12", "messages": [...]}]`，消息字段同 §9.4 的转发内容（不含 `attach_state`、`forward_allowed`）。`cursor` 只给负责者，回执时作为 `--ack-cursor` 带回。只有 `scope=item` 接受 `item` 参数，其他作用域带它返回 400。

`GET /runs` 返回 `{"runs": [...]}`，按入队顺序升序，只留最新的 `limit` 条（缺省 100、上限 500）。`state`、`category` 取值各可逗号分隔多个（任一匹配，如 `state=failed,timeout`、`category=task,background`），`target` 精确匹配；取值不在枚举内返回 400。`before` 是上一页最旧一条的 id，只取在它之前入队的运行，与其他筛选和 `limit` 叠加，Web「运行日志」据此「加载更多」；不是已知运行 id 返回 400。

`POST /runs/{id}/credential` 不带请求体，成功返回 200：`{"run": "...", "token": "apa_run_…", "expires_at": "<RFC 3339>"}`。`token` 只出现这一次，不进幂等结果表。运行不存在返回 404，不是未结束的 `launch: wake` 后台运行返回 409，其他凭据返回 403。用 delegate token 换取时，同时为该 token 开唤醒租约（§13.6）。

`GET /deliveries/next` 返回的投递：`content.recommend` 是决策的推荐选项，未给推荐时省略；`content.form_url` 是一次性表单链接，只出现在只能发文字的渠道上的非 web_only 决策里；`content.web_only` 为 `true` 表示这是 web_only 决策：内容只有标题、摘要与 `web_url`，不含 `options`、`recommend`、`form_url`，渠道只发 Web 链接、不渲染可点选项，也不要把回答转达给 `POST /deliveries/{id}/answer`（内核拒绝并回 Web 链接）；内核按事项的风险类别在写入投递时填写，初筛升级后产生的新版本投递同样带它；`run` 只出现在唤醒类投递上。规则 `alert` 出口的提醒没有事项：`item` 为空，带 `origin`（`{source_event, rule, source}`，触发它的来源事件、规则 id 与来源 id），`content` 取来源事件的标题、摘要与链接，不带选项；渠道 ext 须能处理空的 `item`（主文档 §6.9）。

`/settings/policy` 的 `channels` 段是渠道的登记属性：`{"<渠道名>": {"text_only": true}}`。`persona_lang` 段是助手说哪种语言：`"zh"`、`"en"`，空串为跟随系统（默认；旧策略版本没有这个键，读作空串），其他值 400。`default_harness` 段是规则未指定 harness 时任务型与后台运行用的 harness，值为 harness 名，空串表示未设置（主文档 §13.6）。`disturb` 段在 v0.2.0 新增三个键，旧键不变：`daily_cap`（整数，默认 20，`0` 不限）、`mutes`（`[{"scope": "source" \| "project" \| "item", "id", "until"?}]`）、`followup_defer`（布尔，默认 `false`）；`threshold_judge` 保留，含义改为"是否开启投递推迟判定"（主文档 §7.4）。

`/settings/model-endpoints`：`GET` 返回、`PUT` 接受下面的形状，两个端点都可为 `null`；`api_key` 只写不读，`GET` 以 `api_key_set` 表示是否已设置，以 `api_key_hint` 给出已存密钥的末 4 个字符（密钥短于 12 个字符时为空串）；`api_key_hint` 只读，照 `GET` 的形状回传时 `PUT` 忽略它。字段语义见主文档 §6.13、§8.8。

```json
{"judge_endpoint": {"url": "…", "api_key": "…", "protocol": "system-one", "model": "…", "locality": "external", "timeout_ms": 10000, "max_batch": 32, "max_material_chars": 8000},
 "generate_endpoint": {"url": "…", "api_key": "…", "protocol": "chat-completions", "model": "…", "locality": "external"},
 "screening_timeout_ms": 30000,
 "models_paused": false}
```

`models_paused` 为 true 时两个端点与密钥原样保留，内核按两者都未配置运行，"测试连接"与"获取模型"照常可用；两个端点都为 `null` 时保存为 false。修改是一次策略变更，记 `policy.changed`（主文档 §8.7）。已存的策略版本没有这个键，读作 false。

`judge_endpoint.protocol` 取 `system-one`（原生判断接口，`url` 是接口根，内核追加 `/systemone`）、`probability-judgment/1`（自建分类服务，`url` 是完整路径）或 `chat-logprobs`（Web 不提供）；省略时按 `probability-judgment/1`。`model` 在 `system-one` 与 `chat-logprobs` 下必填，缺失返回 400。Web 新配判断端点时默认 `system-one`，地址与模型名的预填值见附录 D.7，`timeout_ms` 预填 30000；`PUT` 不替调用方填默认协议。

`generate_endpoint.protocol` 取 `chat-completions`（对话补全接口）、`messages`（消息接口）或 `responses`（响应接口），其他值 400；省略或为空串按 `chat-completions`，`GET` 总是给出实际取值。三种协议的请求形态、结构化输出方式与解析见主文档 §8.1，带厂商名的版本头与经网关时的选法见附录 D.7。已存的策略版本没有这个键时读作 `chat-completions`，不改写。

`POST /settings/model-endpoints/models` 取模型列表，请求 `{"endpoint": "generate" | "judge", "url", "api_key"?, "protocol"?}`：生成端点按 `protocol`（同 `generate_endpoint.protocol`）放鉴权头，`GET <url>/models` 读 `data[].id`（`messages` 另带 `?limit=1000`），去重排序，至多 500 个。`endpoint` 为 `judge` 时只接受 `protocol: system-one`（`GET <接口根>/models`，读 `models[].name`；`url` 末尾带 `/systemone` 的先去掉）与 `chat-logprobs`，`probability-judgment/1` 没有模型列表，返回 400。探测本身的成败是数据，响应恒为 200：成功 `{"ok": true, "models": [...], "truncated": false, "elapsed_ms": 120}`，失败 `{"ok": false, "reason", "status"?, "elapsed_ms"}`，`reason` 取 `unauthorized`、`not_found`、`http_status`（带 `status`）、`timeout`、`bad_response`、`unreachable`、`region_unavailable`（HTTP 451，或 403 且响应体提到地区、国家）。

`POST /settings/model-endpoints/test` 测试连接，请求 `{"endpoint": "judge" | "generate", "config": {…}}`，`config` 与 `PUT` 里对应端点的形状相同，按保存时的同一套规则校验（不合规返回 400）；生成端点须有 `url` 与 `model`。用 `apa doctor` 的内置示例探测（主文档 §15.5）：判断端点用示例题，生成端点用起草建议模板（`data.sample` 为 `draft`，模板不可用时为 `builtin`），示例内容是固定文本，不取账本内容。响应与 `GET /doctor` 的明细同形：`{"subject", "status", "code", "why", "next"?, "data"}`；`code` 取 `ok`、`unauthorized`、`region_unavailable`（服务在当前网络位置不可用，`next` 提示改用可访问的网关地址或代理）、`protocol_error`、`self_reported`、`invalid_request`、`no_answer`、`timeout`、`http_status`、`schema_mismatch`（`status` 为 `warning`，内置小样例没守结构约束）、`draft_schema`（`status` 为 `error`，起草建议模板的输出约束没守；`data.shape` 是返回内容的形态：`empty`、`markdown_fence`、`truncated`、`malformed_json`、`trailing_data`、`text_around`、`plain_text`、`no_tool_call`（`messages` 的回复里没有调用输出工具），内容本身不返回）、`bad_response`（响应体不是所选协议的形状）、`unreachable`；`data` 含 `elapsed_ms`、`locality`，生成端点另有 `protocol` 与 `structured_output`（`response_format`、`text.format` 或 `tool_use`），判断端点另有 `protocol`、`probability_source` 与端点自报的 `model`。

**自检结果**（`GET /doctor` 与 `apa doctor --json`）：`{"ok", "checks": [{"name", "status", "code", "why", "next", "data"?, "details"?: [{"subject", "status", "code", "why", "next"?, "data"?}]}]}`。`status` 取 `ok`、`warning`、`error`；`why`、`next` 是机制性英文，供 CLI 与 agent 阅读；`code` 是稳定的机器码，取值只增不改，清单在 `kernel/doctor/codes.go`，Web 按 `code` 与 `data` 显示本地化的一句话。一个对象有多处问题时，`code` 是第一处，`data.problems` 按顺序列出全部问题的机器码。各项检查的 `code` 与 `data`：

| 检查 | 检查一级的 `code` | 对象一级的 `code` | `data` |
|---|---|---|---|
| `data_dir` | `data_dir_ok`、`data_dir_no_backup`、`data_dir_missing`、`data_dir_permissions`、`ledger_missing`、`ledger_unreadable`、`ledger_damaged`、`ledger_too_new`、`ledger_migrate` | — | `schema_binary`、`schema_ledger`、`last_backup_at` |
| `config` | `config_ok`、`config_invalid`、`config_exposed`、`host_file_invalid` | — | `listen` |
| `tier` | `tier_paused`、`tier_kernel_only`、`tier_no_host`、`tier_full`；账本事实读不出时 `facts_unreadable` | — | `tier`、`model_endpoint`、`host`、`models_paused` |
| `model_endpoint` | `models_paused`、`models_none`、`models_ok`、`models_failing` | 同 `POST /settings/model-endpoints/test` 的 `code`，另有 `not_configured`、`no_client` | 同测试连接 |
| `sources` | `sources_none`、`sources_ok`、`sources_attention`、`sources_restored_missing` | `source_ok`（`data.last_run` 为 `ok`、`never`、`unreadable`）、`source_command_missing`、`source_outbound_unset`、`source_outbound_full`、`source_last_failed`、`source_tool_timeout`、`source_tool_version`、`source_tool_missing`、`source_tool_config`、`source_tool_failed`、`source_creds_missing`、`source_webhook_key_missing` | 检查一级 `count`；对象一级 `enabled`、`type`、`command`、`outbound`、`last_run`、`consecutive_failures`、`last_success_at`、`last_failure_at`、`tool`、`problems` |
| `packs` | `packs_no_loader`、`packs_ok`、`packs_rejected` | `pack_rejected` | 检查一级 `loaded`、`rejected`；对象一级 `source`、`in_effect` |
| `host_plugin` | `host_none`、`host_ok`、`host_attention` | `host_ready`、`host_ready_no_selfcheck`、`host_headless_missing`、`host_selfcheck_failed`、`host_no_session_kind`、`host_hook_missing` | `last_call_at`、`plugin_version`、`missing_hooks`、`problems` |
| `overlaps` | `overlaps_skipped`、`overlaps_none`、`overlaps_found` | `overlap` | `harness`、`kind`、`name` |
| `channels` | `channels_none`、`channels_ok`、`channels_backlog` | `channel_ok`、`channel_backlog`、`channel_orphan`、`export_ok` | `kind`、`backlog`、`last_seen_at` |

`apa doctor` 在数据目录不可读或数据结构与二进制不符时只做前两项，其余各项为 `warning`，`code` 为 `skipped`，`data.reason` 取 `ledger_unreadable` 或 `ledger_schema_differs`。升级预检（`--upgrade-check`）的专用项只在 CLI 输出，不带 `code`。

两条路由的密钥规则相同：`api_key` 显式给出时照用，`""` 表示不带；省略或为占位 `***` 时沿用该端点已存的密钥，但只在请求的 URL 与已存 URL 同源（scheme、主机、端口相同）时沿用，否则返回 400，要求重新输入密钥。

v0.1 的请求形状 `{"model_endpoint": {"url", "model", "api_key", "screening_timeout_ms"}}` 在本版仍被 `PUT` 接受：等同只设置 `generate_endpoint`（`locality` 记为 `external`），`screening_timeout_ms` 提到顶层，判断端点不变；响应一律是新形状。下一个次版本起不再接受旧形状。

`/settings/judgment`：`{"bands": [{"template", "source"?, "low", "high"}], "shadow": ["<模板>"], "spot_check": {"per_brief": 5}}`。`bands` 覆盖能力包声明的不确定带，带 `source` 的条目优先；`low` 大于 `high` 返回 400。每次修改是一次策略变更，记 `policy.changed`（主文档 §8.10）。

`/settings/sources/{id}/outbound`：`{"local": "full", "external": "excerpt"}`，各取 `full`、`excerpt`、`meta`、`none`；`external` 可为 `null`（未选择）。`/settings/projects/{id}/capture-outbound` 同理，请求体与响应只有一个值：`{"value": "excerpt"}`，`value` 可为 `null`。两者读写都只有 Web 登录；写入记账本事件（来源是内部的 `source.updated`，项目是 `project.updated`），未变不记。`POST /sources`、`PATCH /sources/{id}` 可写 `default_project`，不能写 `outbound`（返回 403，退出码 20）。

`GET /materials/{id}/content` 只用于托管型，自有型返回 409（读修订）。响应 200，`state` 取三个值之一，其余字段随之变化：

```json
{"state": "reading", "run": "run_…"}
{"state": "ready", "content": "…", "commit": "abc123", "repo": "…", "path": "docs/design.md", "read_at": "…"}
{"state": "unavailable", "reason": "…", "repo": "…", "path": "docs/design.md"}
```

`reading` 时隔几秒再请求；同一材料的在途读取只有一个，在短任务队列内优先（主文档 §7.7）。`unavailable` 是降级结果，不是错误；再次请求会重新读取。

`POST /materials/{id}/changes` 只用于托管型，自有型返回 409。请求体 `{"assignee": "<执行者 id>", "request": "<修改要求>", "parent": "<事项 id，可选>"}`，可带 `Idempotency-Key`。成功返回 201：`{"item": "<子事项 id>", "parent": "<父事项 id>"}`。子事项带材料的 project、仓库和路径，委派成功后父事项以 `output` 关联该材料；`assignee` 不在项目允许范围内返回 403（退出码 21）；`parent` 不属于材料所在项目返回 400。父事项来源见主文档 §12.3。

`GET /agents-md?repo` 返回 `{"content": "…", "hash": "…", "rendered_at": "…", "last_check": {"state": "current|stale|drift", "file_hash": "…", "host": "…", "checked_at": "…"}}`。`last_check` 来自同一仓库最近一次 `POST /agents-md/check`，从未检查过时为 `null`。这两条路由的 `repo` 接受同一仓库的任一写法（主文档 §6.5）。

`GET /packs` 的每个包带 `views`，键是视图文件名去掉 `.yaml`，值只是声明性配置（附录 C.8），不含提示词正文与模板；未声明的列表返回 `[]`，没有视图的包返回 `{}`：

```json
{"views": {"brief": {"kind": "brief", "list": {"columns": ["title", "project", "created_at"], "filters": ["project", "tag"]}, "card": {"fields": ["summary", "spec.period"]}}}}
```

## B.3 错误响应

```json
{"code": "executor_not_allowed", "why": "执行者不在项目允许范围内", "next": "改选 allowed 中的执行者，或用 apa ask 交给本人", "allowed": ["work-host.cli"], "web_url": null}
```

- `why` 与 `next` 只写机制，不含账本里的用户内容。`allowed` 是 Web 中配置的执行者 id。本版另加：`allowed_match`（按机器或 harness 的匹配规则）与 `candidates`（名录里当前命中名单的执行者 id）。
- 委派记录与执行者句柄未通过校验时返回 400（退出码 2），`why` 只写键路径与规则名，不回显取值（主文档 §6.17）。
- HTTP 状态与退出码的对应：400 → 2；401 → 3；403 → 4、20、21、22（由 `code` 区分）；404 → 5；409 → 6；其余错误 → 1。不可达时 CLI 返回 7。
- 退出码 22 的 `code` 为 `risk_screening_pending`，`why` 说明 web_only 初筛未返回，`web_url` 给出 Web 链接（主文档 §14.4）。
- 退出码 23 在 v0.2.0 新增一个原因：`code` 为 `session_not_direct`，`why` 说明会话不是与本人的私聊，`web_url` 给出 Web 链接（主文档 §14.5）。HTTP 状态与其他依据不足的情况相同。
- 登录被限速或锁定时 `POST /auth/login` 返回 429，`code` 为 `login_locked`，带 `Retry-After`。
- 本人暂停了模型时，需要模型的本人操作（积压的影子打分与放开重判）返回 409，退出码 6，`code` 为 `models_paused`，`next` 提示到"设置 › 模型"恢复（主文档 §8.7）。

## B.4 标准事件类型

对外事件类型只增不改。初始集合：

`item.created`、`item.updated`、`item.phase_changed`、`item.deadline_passed`、`decision.requested`、`decision.resolved`、`delegation.created`、`delegation.reported`、`command.rejected`、`material.revised`、`material.revision_proposed`、`guideline.activated`、`project.updated`、`project.allowed_executors_changed`、`rule.mode_changed`、`allowlist.changed`、`policy.changed`、`grant.issued`、`grant.revoked`、`run.started`、`run.finished`、`source.failed`、`health.alert`。

v0.2.0 新增四个登录审计类型（主文档 §14.13），对象是 `owner`，载荷不含口令：登录成功、同一来源地址失败锁定、改口令（Web 或主机上 `apa init --reset-password`）、吊销会话（逐个或全部）。

`auth.login`、`auth.login_locked`、`auth.password_changed`、`auth.session_revoked`。

v0.2.0 新增两个类型（下一行），排在初始集合之后：前者表示事项追加了一条进展；后者表示建议被补到已有事项，对象是被补到的事项。

`item.progress_added`

`proposal.attached`。

已有类型的信封不变；`item.updated` 的载荷新增可选的 `planned_for`，入站处置类事件的载荷新增可选的 `judgment`（判断记录 id）。v0.1 写下的 `run.finished`（`category: judge`）仍可回放，新的判断不再产生它（主文档 §8.6）。

## B.5 协议与版本

`apa version` 与 `GET /version` 返回同一份内容：

```json
{"version": "0.2.0", "commit": "…", "test_clock": false, "schema": {"ledger": 6}, "protocols": {"http_api": "v1", "source_contract": 1, "channel_ext": 1, "probability_judgment": "probability-judgment/1"}}
```

`commit` 是构建所在的提交（不是从提交构建时为 `unknown`）；`test_clock` 在 `apa_testclock` 测试构建里为 true，发布构建恒为 false。`schema` 是各存储模块的数据结构版本，随发行只增；旧版本的二进制拒绝打开版本更高的账本（主文档 §16.5）。账本结构版本 3 在第一次有真实账本升级到它之后冻结，此后的结构变更另起迁移、版本依次递增，所以发布物里的 `ledger` 是发布时主干上的最新版本：来源显示名一列在版本 3 冻结之后加入，主干现为 4。概率判断协议以主文档 §8.8 为权威定义，本附录不重复字段。

## B.6 v0.2.0 对已有契约的改动

除下表外，v0.2.0 对 v0.1 的命令、路由、字段和事件都只做加法。

| 改动 | v0.1 | v0.2.0 | 兼容 |
|---|---|---|---|
| 凭原话的第二档命令 | 只要求原话与会话引用非空 | 另要求 `X-APA-Session-Kind: direct` | 没有兼容期：缺这个头即退出码 23。harness 插件须与内核一同升级 |
| `/settings/model-endpoints` | 一个 `model_endpoint` | `judge_endpoint`、`generate_endpoint`、顶层 `screening_timeout_ms` | 本版 `PUT` 仍接受旧形状；已存的策略版本不改写，读取时等同 `generate_endpoint` |
| `GET /health` 的信号数 | 十一项 | 十三项，末尾追加两项 | 前十一项的 id 与顺序不变；按下标取信号的调用方不受影响，按总数断言的要改 |
| `unjudged_reason` | `model_unavailable`、`undecided` | `model_unavailable`、`models_paused`、`outbound_unset`、`restored` | 旧值原样返回；内核按 `model_unavailable` 对待并参与重判。按枚举穷举判断的调用方要接受三个新值 |
| 策略的 `models_paused` | 无 | 顶层布尔，经 `/settings/model-endpoints` 读写 | 只增字段：已存的策略版本没有这个键，读作不暂停，不改写旧版本；`GET /health` 的 `model_unavailable.detail` 只增 `paused` 键与取值 `paused` |
| 生成端点的 `protocol` | 只有对话补全接口 | `generate_endpoint.protocol`：`chat-completions`、`messages`、`responses` | 只增字段：已存的策略版本没有这个键，读作 `chat-completions`，不改写，不迁移；`GET` 总是给出取值。旧二进制解码策略时忽略未知键，回退后按对话补全接口调用，选了另两种协议的端点会失败，在旧二进制里保存设置也不会写回这个键；回退前把协议改回 `chat-completions`，或回退后改用对话补全接口的地址 |
| `GET /health` 的 `unjudged_backlog.detail` | 各原因计数与 `sources` | 另带 `rejudge_days` | 只增字段，按键取值的调用方不受影响 |
| `disturb.threshold_judge` | 开启三选一的投递阈值判定 | 开启"可以晚点送吗"的推迟判定 | 键名与类型不变；已开启的继续开启，"只留 Web 未读"这一去向不再由模型选择 |
| 实例驱动的时钟规则 | `GET /rules` 列出，`PATCH /rules/{id}/mode` 可改，值不生效 | `GET /rules` 不再列出；`PATCH` 返回 404 `not_found`，与规则不存在一致 | 账本里 `rules_state` 已存的行不删不改，内核照旧用它们执行实例到期；只是不再读出给人看。调用方按列表取规则的不受影响，直接对这类规则改模式的要按 404 处理 |
| 来源规则的 `then: judge` | 交判断型三选一 | `screen` 的别名 | 仍被接受 |
| 判断留痕 | `run.finished`（`category: judge`）事件 | 判断记录（`/judgments`） | 旧事件保留、可回放；不计入校准统计 |
| 来源的显示名 | 无；Web 显示来源 id | `name` 总有值：登记时给定或取来源程序的接入声明；都没有时登记被拒（400），重名的登记与改名返回 409 | 只增字段与列（`sources_registry` 迁移 3，账本结构版本 3 → 4）。已有来源在 `apa serve` 启动时补名：先按接入声明，没有声明的按类型起名并记一条健康说明，逐个记 `source.renamed`（操作者 system），不触发判断、重判或健康说明以外的投递。**调用方要改的**：登记仓库之外的来源程序（自定义脚本、自建拉取程序、非自带类型的 webhook）以前可以不带名字，现在必须带 `name` / `--name`，否则 400；同一程序登记多个实例而声明给出同名时（如两个 RSS 订阅）要显式起名。迁移后旧二进制拒绝打开账本（结构版本更高），回退要用升级前快照，见主文档 §16.5 |
| 规则初始模式 | 一律 `observe` | 提醒类 `auto`，动作类 `observe` | 升级时未动过的内置提醒类规则切到 `auto` 并记 Event（主文档 §16.5） |
| 执行者与委派（主文档 §6.17） | 执行者由本人在 Web 手填或确认插件声明后进入清单；`assignee` 不在清单里返回 5；确认声明时覆盖该 harness 的全部条目；名单只按 id；挂靠消息只对 `assignee` 推进 | harness 随 `apa delegate --executor` 登记，身份字段固定；不在名录里也可指派；确认声明并入名录、不删除；新增委派记录（`apa dispatch`、`/items/{id}/dispatches`、`/dispatches`）与 `apa context --scope item` 的 `history`；名单可按机器或 harness 匹配（`allowed-executors` 的请求与返回加 `match`）；21 的返回体加 `allowed_match`、`candidates`；发起委派的凭据与运行身份对目标的委派子事项也能推进挂靠消息并拿到确认游标；删除 `GET /settings/executors`（读名录改用 `GET /executors`）与 Web 设置里的手填表单；`DELETE /settings/executors/{id}` 改为 `DELETE /executors/{id}`；`POST /settings/executors`（`executor.put`）保留为只 Web 登录、无界面的本人维护入口（如登记交给人的执行者） | 只增列与表（账本结构版本 4 → 5），已有条目、声明与名单原样保留，名单判定不变。**调用方要改的**：依赖"未登记返回 5"的调用方改为自己先查 `GET /executors`；用 `/settings/executors` 读名录的改用 `GET /executors`。带调用表的旧插件仍可委派，只是不写委派记录。删除的路由与 Web 切换在同一次改动里完成 |
| 授权单的被授权方与有效期 | `grantee.credential` 必填；`expires_at` 必填，不超过 90 天；运行凭据带 `--grant` 一律退出码 4 | 另可 `grantee.agent`（按内核记录的 harness 匹配）与 `background`；`scope.all`；`expires_at` 可选（不设即长期）；允许后台使用的单对该 Agent 的运行凭据生效，不符时退出码 23（原 4） | 已签发的单原样生效。按"运行凭据带单为 4"断言的调用方改为 23。见主文档 §6.13、§14.5 |
| 来源事件的 `sender_verified` | 来源程序逐条自报 | 以来源程序的接入声明为上限，没有声明的程序按需确认（恒为假）；声明为"不带发件人"的事件类型清空 `sender` | 已落盘的事件不改写。自定义脚本与 webhook 转换脚本以前自报为真的，之后按未核实处理，转发白名单对它们不再生效（主文档 §9.1） |
| 代码托管来源的事件 | `*.updated`、`*.closed`、`*.merged` 的 `sender` 为对象作者 | 这些类型不带 `sender`；新增评论事件 `issue.commented`、`pr.commented`、`pr.review_commented`（webhook 另有 `pr.reviewed`），带已核实的评论作者 | 已落盘的事件不改写。按事件类型匹配的用户规则若要处理评论，需加上新类型（附录 D.4） |
| 渠道回答人 | 即时通讯渠道 ext 优先报用户名 | 一律报平台用户 ID | 写着用户名的旧条目不再匹配，本人在"设置 › 渠道"识别一次 ID（主文档 §16.5） |
| 凭据表 | 无 `harness` | `auth_credential.harness`，账本结构版本 5 → 6 | 只增列，已有 token 照常可用；迁移后旧二进制拒绝打开账本，回退用升级前快照 |
