# 附录 B：API 与 CLI 清单

> 主文档 §17 的逐条清单，分组级语义见主文档，字段级契约在实现时随代码落地。路径省略版本前缀 `/v1`；`/ingest/` 不带版本。

## B.1 CLI

"最低凭据"指能调用该命令的最低级别；运行凭据按 worker 处理，并受 §14.3 的范围限制。

| 命令 | 作用 | 最低凭据 | 关键退出码 |
|---|---|---|---|
| **入门** | | | |
| `apa init` | 建立数据目录，设置 Web 登录 | 主机本地 | |
| `apa serve` | 启动内核与 Web | 主机本地 | |
| `apa login` | 保存 APA 地址与 token | 任意 token | 3 |
| `apa guide [主题] [--template]` | 输出使用说明，只写机制；`--template` 输出常驻指令模板 | 无 | |
| `apa declare <文件>` | 提交插件声明（无头运行、执行者），本人在 Web 确认后生效 | worker | 4 |
| **上下文** | | | |
| `apa context [--session] [--scope project/cwd/item/all] [--item <id>] [--format inject]` | 账本增量投影，含路由为注入的投递和挂靠消息；`--scope item` 见主文档 §7.6 | observer | 3、5、7 |
| `apa persona` | 读取当前人格，供插件在新会话开始时注入；未设定时输出为空 | observer | 3、7 |
| `apa inbox` | 列出待决和建议；web_only 初筛未返回的决策请求不列出 | observer | |
| **事项** | | | |
| `apa propose` | 以 `proposed` 新建 | observer | |
| `apa item show/list` | 读事项；`show` 带挂靠消息，按转发白名单过滤 | observer | 5 |
| `apa item add/update/transition/relate [--evidence \| --grant]` | 增改事项、转阶段、关联（`relate --to` 可以是事项或材料） | worker（自己的事项）；delegate（绑定的事项和以 `proposed` 新建不要求依据；其他事项附 `--evidence` 或 `--grant`） | 2、4、6、23 |
| **决策** | | | |
| `apa ask <id>` | 写入决策请求并转 `needs_input` | worker | 4 |
| `apa decide <id> --option [--evidence \| --grant]` | 代答决策；本人原话或授权单二选一 | delegate | 2、4、6、20、22、23 |
| `apa accept <id> [--evidence \| --grant]` | 接受建议；依据同上 | delegate | 2、4、20、23 |
| `apa dismiss <id> --reason [--evidence \| --grant]` | 丢弃建议；依据同上 | delegate | 2、4、23 |
| `apa grant list` | 列出该凭据名下未撤销、未过期的授权单：id、范围、动作、有效期；签发和撤销只在 Web | delegate | 4 |
| **委派** | | | |
| `apa executors [--project]` | 列出执行者，带项目时标明是否允许 | observer | |
| `apa delegate --parent --assignee` | 建子事项，校验项目允许的执行者 | worker（自己的事项）；delegate（任意） | 4、5、21 |
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
| `apa source add` | 登记附带的拉取程序或 webhook source；自定义命令（含邮件、日历工具的适配命令）只在 Web | delegate | 20 |
| `apa source auth <id>` | 以该来源登记的 `credentials_path` 启动附带拉取程序的认证模式（在本机运行 `<来源命令> auth <credentials_path>`，继承标准输入输出），凭据写在该路径；不是 pull 类型或未登记 `credentials_path` 的来源返回 2，工具适配的来源用工具自己的认证命令（主文档 §9.7） | 主机本地，经 API 读来源登记，需 observer | 2、5 |
| `apa source enable/disable` | 启停 | delegate | |
| `apa source webhook create/rotate/revoke` | 创建、轮换、吊销 webhook 接收密钥 | delegate | |
| `apa source ingests` | 查看原始 payload、校验结果、转换结果 | observer | |
| `apa source rejects <id> [--after N] [--limit N]` | 查看没有入账的输出行及原因（`next_after` 作为下一次的 `--after`） | observer | |
| `apa source replay <入站记录>` | 重放 | delegate | |
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

**只在 Web**：策略、信任级、token、ext 密钥、web_only 分类、两份白名单、项目允许的执行者、执行者清单、规则切 `auto`、运行权限上限、模型端点、可执行命令登记（另可由本人写主机配置文件，§14.12）、撤销 agent 变更、物理删除、规范生效、签发和撤销授权单。CLI 没有对应的写命令；相关命令（如 `apa project update`、`apa source add`）带上这些字段时返回退出码 20。

**代理写命令清单**：delegate 调用写命令时按动作归属分三档（主文档 §14.5）。其他凭据与 Web 本人不经这张表；只读命令不在清单内。

| 档 | 依据 | 缺失时 |
|---|---|---|
| 一：agent 自己的工作 | 凭据身份 + 对象绑定，不要求本人原话；带了 `--evidence` 照常记录 | 绑定不成立的回执、交回和事项修改按第二档判定；`delegate.report` 报 `done` 缺结果证据为退出码 2 |
| 二：替本人做决定 | 二选一：`--evidence`（`X-APA-Evidence`）本人原话 + `APA_SESSION`（`X-APA-Session`）会话引用；或 `--grant <id>`（`X-APA-Grant`）授权单。同时携带为退出码 2 | 退出码 23，不写入；`open`、`waiting` 的事项转 `needs_input`，其余阶段不变；见主文档 §14.5 |
| 三：高风险与设置类 | 只在 Web，见上文"只在 Web"；web_only 决策同样在这一档 | 退出码 20 |

| 模块 | 第一档 | 第二档（可用授权单） | 第二档（只接受本人原话） |
|---|---|---|---|
| 事项 | `item.create`（以 `proposed` 新建）；`item.update`、`item.transition`、`item.relate`（绑定的事项） | `item.create`（其他阶段）；`item.update`、`item.transition`、`item.relate`（其他事项） | |
| 对话捕获 | `inbound.capture` | | |
| 决策 | `decision.request` | `decision.answer`、`proposal.accept`、`proposal.dismiss` | |
| 材料 | `material.commit`、`material.add`、`blob.put` | `revision.confirm`、`revision.reject` | |
| 委派 | `delegate.create`；`delegate.report`、`followmsg.handback`（绑定的事项）；`executor.declare` | `delegate.report`、`followmsg.handback`（其他事项） | |
| 项目 | `project.checkout.set`、`project.checkout.remove` | | `project.create`、`project.update`、`project.repo.add`、`project.repo.remove` |
| 来源与能力包 | | | `source.register`、`source.update`、`source.set_enabled`、`source.webhook_key`、`source.webhook_revoke`、`source.replay`、`instance.create`、`instance.update`、`instance.toggle` |

授权单的 `actions` 只能取"可用授权单"一列的命令。

**会话身份**：环境变量 `APA_SESSION`、`APA_HARNESS` 由接入方（harness 插件）设置，CLI 随每次请求以 HTTP 头 `X-APA-Session`、`X-APA-Harness` 发出，Event actor 据此审计到会话粒度（主文档 §11.2、§17.4）。

## B.2 HTTP API

| 分组 | 方法与路径 | 说明 | 凭据 |
|---|---|---|---|
| 事项 | `GET/POST /items`，`GET/PATCH /items/{id}` | 查询、创建、更新 | 按 §14.3；导出 ext 密钥可 `GET` |
| | `GET /items/{id}/messages` | 事项的全部挂靠消息 | observer；运行身份和无人值守事项按转发白名单过滤；ext 密钥不可用 |
| | `POST /items/{id}/transition`，`POST /items/{id}/relations` | 转阶段、关联 | 同上 |
| | `POST /items/{id}/revert` | 撤销 agent 变更 | 只有 Web 登录 |
| 决策 | `POST /items/{id}/ask` | 写入决策请求 | worker |
| | `POST /items/{id}/decide`，`/accept`，`/dismiss` | 回答或处理建议；delegate 带 `X-APA-Evidence` 或 `X-APA-Grant`，二选一 | Web 登录、delegate |
| | `GET /grants` | 该凭据名下未撤销、未过期的授权单 | delegate |
| 委派 | `GET /executors` | 执行者清单 | observer |
| | `POST /delegations` | 建子事项 | worker |
| | `POST /items/{id}/report` | 回执 | worker |
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
| | `GET /context/persona` | 当前人格，前面带内核固定声明 | observer |
| | `POST /capture` | 提交对话增量 `{text, project?, path?, host?}`；运行身份下、项目未开启捕获时不生效，返回 200 `{"captured": false, "reason"}`；候选在提交后异步抽取 | worker |
| 插件声明 | `POST /declarations` | 提交无头运行声明与执行者，进 Web 待确认 | worker |
| 来源 | `GET/POST /sources`，`GET/PATCH /sources/{id}` | 来源登记 | observer 读，delegate 写；自定义命令只有 Web |
| | `POST /sources/{id}/webhook` | 创建或轮换接收密钥 | delegate |
| | `DELETE /sources/{id}/webhook` | 吊销该来源全部接收密钥，同时删除 HMAC 密钥文件 | delegate |
| | `GET /sources/{id}/ingests`，`POST /ingests/{id}/replay` | 入站记录与重放 | observer、delegate；运行凭据不能读入站记录（返回 403，原始请求体未经转发白名单） |
| | `GET /sources/{id}/events?disposition&after&limit` | 来源事件，`disposition` 可逗号分隔多个；内部来源 `capture:<项目>` 只有 Web 登录能读 | observer；运行凭据不可用（返回 403，挂靠消息经转发白名单读取） |
| | `GET /sources/{id}/rejects?after&limit` | 被拒收的输出行及原因 | observer；运行凭据不可用（返回 403，原始行是未过滤的外部输出） |
| | `POST /source-events/{id}/propose` | 把未判定事件转为建议 | Web 登录、delegate |
| | `POST /source-events/{id}/ignore`，`POST /source-events/ignore` | 忽略未判定事件：单条；或批量，请求体 `{"source"}` 按来源、`{"all": true}` 全部 | Web 登录、delegate（附本人原话） |
| 入站 | `POST /ingest/{source_id}` | webhook 接收路由 | webhook 接收密钥 |
| 事件 | `GET /events?cursor=` | SSE 或长轮询 | observer、导出和转发 ext 密钥 |
| 投递 | `GET /deliveries/next?wait=` | 长轮询领取；有租约 | 渠道 ext 密钥；网关插件 token（worker 及以上，只领唤醒类投递） |
| | `POST /deliveries/{id}/ack`，`/fail`，`/answer` | 送达、失败、转达回答 | 同上 |
| | `GET /deliveries/unread`，`POST /deliveries/{id}/read` | Web 未读；`read` 即本人确认（投递转 `acknowledged`） | Web 登录 |
| | `GET/POST /forms/{一次性令牌}` | 一次性表单 | 令牌本身 |
| 运行 | `GET /runs`，`GET /runs/{id}` | 运行记录 | observer |
| | `POST /runs/{id}/credential` | 把唤醒运行的 id 换成短期运行凭据 | worker、delegate（网关插件 token） |
| 回放与健康 | `GET /why/{id}`，`GET /health` | 回放、健康视图；所有健康告警都经同一处产出健康类投递（§15.2），读取走 `GET /deliveries/unread` | observer |
| 能力包 | `GET /packs`，`GET/POST /instances`，`PATCH /instances/{id}` | 能力包与实例 | observer 读，delegate 写 |
| | `GET /rules`，`PATCH /rules/{id}/mode` | 规则与模式；切换只有 Web | Web 登录 |
| 设置 | `/settings/policy`、`/settings/tokens`、`/settings/ext-keys`、`/settings/allowlists`、`/settings/executors`、`/settings/commands`、`/settings/declarations`、`/settings/model-endpoints`、`/settings/access-limits`、`/settings/projects/{id}/allowed-executors`、`/settings/grants`、`/settings/purge` | 策略（含人格）、凭据、白名单、名单、命令登记、声明确认、授权单签发与撤销（`POST /settings/grants`、`POST /settings/grants/{id}/revoke`）、物理删除 | 只有 Web 登录 |
| 认证 | `POST /auth/login`、`/logout` | Web 会话 | |

事项字段 `deadline`：`POST /items` 与 `PATCH /items/{id}` 可写，值为 RFC 3339 时间；`PATCH` 传 `null` 清除；`GET` 在已设置时返回。行为见主文档 §6.8。

`GET /items` 返回 `last_rejections`（事项 id → 最近一次未处理的退出码 23 拒绝：`seq`、`at`、`actor`、`code`、`why`、`grant`），`GET /items/{id}` 返回 `last_rejection`；只对 `needs_input`、`proposed` 事项给出，决策待办与建议流据此标明被拒绝的代答或处理。`GET /audit/events` 可加 `grant=<授权单 id>` 查该授权单的签发、使用与撤销记录。

事项字段 `created_at`、`updated_at`：`GET /items` 与 `GET /items/{id}` 返回，值为 RFC 3339 时间，只读。`created_at` 是该事项首条事项事件的时间（委派子事项的首条是 `delegation.created`，其余是 `item.created`）；`updated_at` 是最后一条事项事件的时间，与 `version` 同步推进（含关系变更；`item.deadline_passed` 与 `command.rejected` 不算）。

`POST /sources`、`PATCH /sources/{id}` 的 `credentials_path` 必须落在该来源自己的目录（`<APA_HOME>/sources/<id>/`）内，否则返回 400。内核只记录这个路径，不读内容；每次拉取时经 stdin JSON 的 `credentials_path` 字段原样交给来源进程（主文档 §9.2），不经环境变量传递。

`POST /sources/{id}/webhook` 不带请求体。来源还没有有效密钥时创建，返回 201；已有时轮换，返回 200，旧密钥在 1 小时宽限期后失效；轮换总是作用于当前有效（未吊销、未轮换过）的密钥，已吊销或已轮换过的凭据不能再轮换（内核返回 409）。响应 `{"credential": {...}, "secret": "apa_whk_…", "url": "/ingest/<source_id>", "rotated": false}`，`secret` 只出现这一次，不进幂等结果表，Event 只记凭据 id。校验方案为 HMAC 的来源，明文另写入 `<APA_HOME>/webhook-keys/<source_id>`（主文档 §14.2），没有任何 API 能读回；共享 token 方案只存哈希。

`DELETE /sources/{id}/webhook` 不带请求体，吊销该来源全部有效的接收密钥（含宽限期内的旧密钥）并在同一事务里删除密钥文件，返回 200 `{"revoked": n}`，已无有效密钥时 `n` 为 0。文件删除失败时整个命令回滚、账本不变，返回错误，重试即可。Event 为 `source.webhook_key_revoked`，只记凭据 id。

`GET /sources/{id}/events` 按落盘顺序返回 `{"events": [...], "next_after": 12}`。每条事件的字段见主文档 §6.10：`id`、`source_id`、`dedupe_key`、`type`、`subject`、`summary`、`detail`、`link`、`occurred_at`、`correlation_keys`、`sender`、`sender_verified`、`disposition`、`item`、`attach_state`、`unjudged_reason`、`rejudges`，其中 `item` 是挂靠或生成的事项 id。`disposition` 可选，取值 `pending`、`attached`、`proposed`、`ignored`、`unjudged`、`expired`，可逗号分隔多个（来源页的积压用 `unjudged,expired`），其他值返回 400。`limit` 缺省 100，上限 500；还有下一页时才返回 `next_after`，作为下一次的 `after`。

`GET /sources/{id}/rejects` 按发生顺序返回 `{"rejects": [...], "next_after": 12}`，分页参数与 `events` 相同。每条含 `source_id`、`run_id`、`line`（输出中的行号）、`reason`、`raw`（原始行）、`at`。`raw` 在落账时已截断到 512 字节；含接收密钥明文前缀的行返回 `[redacted]`（与入站记录的请求头同一规则）。来源未登记返回 404。
`capture:<项目>` 是对话捕获的内部来源（主文档 §9.3），不在 `GET /sources` 里，Web 按开启捕获的项目拼出 id。Web 登录读它返回同样的结构，其他凭据返回 404（运行凭据仍是 403）。

`POST /source-events/{id}/propose` 不带请求体，成功返回 200 与更新后的来源事件（`disposition` 为 `proposed`，`item` 为新事项 id），事件不是 `unjudged` 或 `expired` 时返回 409，不存在时返回 404。事项阶段为 `proposed`，事项的 `item.created` 与来源事件的 `inbound.disposed` 都记调用者为操作者。

`POST /source-events/{id}/ignore` 不带请求体，成功返回 200 与更新后的来源事件（`disposition` 为 `ignored`），事件不在积压里（不是 `unjudged` 或 `expired`）时返回 409，不存在时返回 404。`POST /source-events/ignore` 的请求体恰好给出 `source` 或 `all: true` 之一，否则返回 400；在一个命令里忽略该来源或全部来源的积压，返回 200 `{"ignored": n}`，没有积压时 `n` 为 0。两者都可带 `Idempotency-Key`，每条被忽略的事件记一条 `inbound.disposed`，操作者是调用者。

`GET /health` 返回 `{"healthy": bool, "signals": [...]}`，信号恒为十一项且顺序固定（主文档 §15.2）：`source_failures`、`outcall`、`delivery_backlog`、`chain_trips`、`agentsmd_drift`、`pack_rejections`、`model_unavailable`、`unjudged_backlog`、`rule_fallbacks`、`receipt_failures`、`other_alerts`。每项 `{id, ok, count, severity?, why?, next, detail?}`；`severity` 只在异常时给出，取 `error` 或 `warning`。`unjudged_backlog` 是 `warning`，不影响 `healthy`；它的 `detail` 为 `{"model_unavailable": n, "undecided": n, "expired": n, "sources": [{"source", "count"}]}`，`expired` 只作参考、不计入 `count`。`model_unavailable` 的 `detail.endpoint` 为空、`not_configured`（未配置而有事件因此未判定）或 `failing`（调用在全部重试后仍失败，之后没有成功）。

`GET /items/{id}/messages` 返回 `{"messages": [...]}`，按落盘顺序。每条字段：`id`、`source`、`sender`、`sender_verified`、`subject`、`summary`、`detail`、`link`、`occurred_at`、`attach_state`、`forward_allowed`（是否进转发白名单）。只读，不推进 `attach_state`。运行身份和 `unattended` 事项的 worker、observer 只拿到 `forward_allowed` 为真的消息；Web 登录和 delegate 拿到全部。web_only 初筛中的事项对非 Web 凭据返回空列表。事项不存在返回 404。`apa item show` 把这个列表合进输出的 `messages`。

`GET /context?scope=item[&item=<id>]` 在普通字段外返回 `messages`：`[{"item": "...", "cursor": "12", "messages": [...]}]`，消息字段同 §9.4 的转发内容（不含 `attach_state`、`forward_allowed`）。`cursor` 只给负责者，回执时作为 `--ack-cursor` 带回。只有 `scope=item` 接受 `item` 参数，其他作用域带它返回 400。

`POST /runs/{id}/credential` 不带请求体，成功返回 200：`{"run": "...", "token": "apa_run_…", "expires_at": "<RFC 3339>"}`。`token` 只出现这一次，不进幂等结果表。运行不存在返回 404，不是未结束的 `launch: wake` 后台运行返回 409，其他凭据返回 403。用 delegate token 换取时，同时为该 token 开唤醒租约（§13.6）。

`GET /deliveries/next` 返回的投递：`content.recommend` 是决策的推荐选项，未给推荐时省略；`content.form_url` 是一次性表单链接，只出现在只能发文字的渠道上的非 web_only 决策里；`content.web_only` 为 `true` 表示这是 web_only 决策：内容只有标题、摘要与 `web_url`，不含 `options`、`recommend`、`form_url`，渠道只发 Web 链接、不渲染可点选项，也不要把回答转达给 `POST /deliveries/{id}/answer`（内核拒绝并回 Web 链接）；内核按事项的风险类别在写入投递时填写，初筛升级后产生的新版本投递同样带它；`run` 只出现在唤醒类投递上。

`/settings/policy` 的 `channels` 段是渠道的登记属性：`{"<渠道名>": {"text_only": true}}`。`default_harness` 段是规则未指定 harness 时任务型与后台运行用的 harness，值为 harness 名，空串表示未设置（主文档 §13.6）。

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
{"code": "executor_not_allowed", "why": "执行者不在项目允许范围内", "next": "改选 allowed 中的执行者，或用 apa ask 交给本人", "allowed": ["work-host.codex"], "web_url": null}
```

- `why` 与 `next` 只写机制，不含账本里的用户内容。`allowed` 是 Web 中配置的执行者 id。
- HTTP 状态与退出码的对应：400 → 2；401 → 3；403 → 4、20、21、22（由 `code` 区分）；404 → 5；409 → 6；其余错误 → 1。不可达时 CLI 返回 7。
- 退出码 22 的 `code` 为 `risk_screening_pending`，`why` 说明 web_only 初筛未返回，`web_url` 给出 Web 链接（主文档 §14.4）。

## B.4 标准事件类型

对外事件类型只增不改。初始集合：

`item.created`、`item.updated`、`item.phase_changed`、`item.deadline_passed`、`decision.requested`、`decision.resolved`、`delegation.created`、`delegation.reported`、`command.rejected`、`material.revised`、`material.revision_proposed`、`guideline.activated`、`project.updated`、`project.allowed_executors_changed`、`rule.mode_changed`、`allowlist.changed`、`policy.changed`、`grant.issued`、`grant.revoked`、`run.started`、`run.finished`、`source.failed`、`health.alert`。
