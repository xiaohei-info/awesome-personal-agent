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
| `apa context [--session] [--scope project/cwd/item/all] [--format inject]` | 账本增量投影，含路由为注入的投递和挂靠消息 | observer | 3、7 |
| `apa inbox` | 列出待决和建议 | observer | |
| **事项** | | | |
| `apa propose` | 以 `proposed` 新建 | observer | |
| `apa item show/list` | 读事项 | observer | 5 |
| `apa item add/update/transition/relate` | 增改事项、转阶段、关联 | worker（自己的事项）；delegate（任意，附证据） | 4、6 |
| **决策** | | | |
| `apa ask <id>` | 写入决策请求并转 `needs_input` | worker | 4 |
| `apa decide <id> --option --evidence` | 代答决策 | delegate | 4、6、20 |
| `apa accept <id> --evidence` | 接受建议 | delegate | 4、20 |
| `apa dismiss <id> --reason --evidence` | 丢弃建议 | delegate | 4 |
| **委派** | | | |
| `apa executors [--project]` | 列出执行者，带项目时标明是否允许 | observer | |
| `apa delegate --parent --assignee` | 建子事项，校验项目允许的执行者 | worker（自己的事项）；delegate（任意） | 4、5、21 |
| `apa report <id> --outcome --summary --evidence --ack-cursor` | 回执：进展、完成、阻塞，确认挂靠消息 | 绑定该 assignee 的 worker、该运行的运行凭据或 delegate | 4、6 |
| `apa handback <消息> --reason` | 把挂靠消息交回一次 | 同上 | 4、6 |
| **项目** | | | |
| `apa project list/show` | 读项目，含 repos 与允许的执行者 | observer | |
| `apa project create/update` | 建项目、改基本信息。`allowed_executors` 不能改 | delegate | 20 |
| `apa project repos add/remove` | 增改本机检出（worker）或远程地址（delegate） | worker | 4 |
| **材料与规范** | | | |
| `apa material add --repo --path` | 登记托管型 | worker | |
| `apa material commit <id> --from --reason [--kind]` | 提交自有型修订 | worker | 6 |
| `apa material list` | 列出材料，可按项目、kind、标签筛选 | observer | |
| `apa material show <id> [--rev]` | 读当前或指定修订；托管型只回位置 | observer | 5 |
| `apa material search <查询>` | 检索，只回 id、标题、片段 | observer | |
| `apa blob put/get` | 附件 | worker / observer | |
| `apa agents check` | 比对工作区 AGENTS.md | observer | 10、11 |
| `apa agents sync` | 写入工作区；漂移时不覆盖并返回 6，本人选择"覆盖"后才写入 | observer | 6 |
| **来源** | | | |
| `apa source list/show` | 来源与统计 | observer | |
| `apa source add` | 登记附带的拉取程序或 webhook source；自定义命令只在 Web | delegate | 20 |
| `apa source auth <id>` | 启动来源程序的认证模式 | 主机本地 | |
| `apa source enable/disable` | 启停 | delegate | |
| `apa source webhook create/rotate` | 创建、轮换 webhook 接收密钥 | delegate | |
| `apa source ingests` | 查看原始 payload、校验结果、转换结果 | observer | |
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

**只在 Web**：策略、信任级、token、ext 密钥、web_only 分类、两份白名单、项目允许的执行者、执行者清单、规则切 `auto`、运行权限上限、模型端点、可执行命令登记、撤销 agent 变更、物理删除、规范生效。CLI 没有对应的写命令；相关命令（如 `apa project update`、`apa source add`）带上这些字段时返回退出码 20。

## B.2 HTTP API

| 分组 | 方法与路径 | 说明 | 凭据 |
|---|---|---|---|
| 事项 | `GET/POST /items`，`GET/PATCH /items/{id}` | 查询、创建、更新 | 按 §14.3 |
| | `POST /items/{id}/transition`，`POST /items/{id}/relations` | 转阶段、关联 | 同上 |
| | `POST /items/{id}/revert` | 撤销 agent 变更 | 只有 Web 登录 |
| 决策 | `POST /items/{id}/ask` | 写入决策请求 | worker |
| | `POST /items/{id}/decide`，`/accept`，`/dismiss` | 回答或处理建议 | Web 登录、delegate |
| 委派 | `GET /executors` | 执行者清单 | observer |
| | `POST /delegations` | 建子事项 | worker |
| | `POST /items/{id}/report` | 回执 | worker |
| | `POST /messages/{id}/handback` | 交回挂靠消息 | worker |
| 项目 | `GET/POST /projects`，`GET/PATCH /projects/{id}` | 项目 | observer 读，delegate 写 |
| | `POST /projects/{id}/repos` | repos 属性 | worker、delegate |
| | `GET /projects/resolve?path&host` | 当前目录到项目的映射 | observer |
| 材料 | `GET/POST /materials`，`GET /materials/{id}` | 材料 | observer 读，worker 写 |
| | `GET/POST /materials/{id}/revisions` | 修订 | 同上 |
| | `POST /materials/{id}/revert` | 回退一次 `direct` 修订 | 只有 Web 登录 |
| | `POST /revisions/{id}/confirm`，`/reject` | 确认或拒绝 proposed 修订 | Web 登录、delegate（规范生效只有 Web） |
| | `GET /materials/search` | 检索 | observer |
| | `PUT/GET /blobs/{hash}` | 附件 | worker 写，observer 读 |
| 规范 | `GET /agents-md?repo` | 取渲染结果 | observer |
| | `POST /agents-md/check` | 上报文件 hash，返回三态 | observer |
| 上下文 | `GET /context` | 账本增量投影 | observer |
| | `POST /capture` | 提交对话增量；运行身份下不生效 | worker |
| 插件声明 | `POST /declarations` | 提交无头运行声明与执行者，进 Web 待确认 | worker |
| 来源 | `GET/POST /sources`，`GET/PATCH /sources/{id}` | 来源登记 | observer 读，delegate 写；自定义命令只有 Web |
| | `POST /sources/{id}/webhook` | 创建或轮换接收密钥 | delegate |
| | `GET /sources/{id}/ingests`，`POST /ingests/{id}/replay` | 入站记录与重放 | observer、delegate |
| 入站 | `POST /ingest/{source_id}` | webhook 接收路由 | webhook 接收密钥 |
| 事件 | `GET /events?cursor=` | SSE 或长轮询 | observer、导出和转发 ext 密钥 |
| 投递 | `GET /deliveries/next?wait=` | 长轮询领取；有租约 | 渠道 ext 密钥、网关插件 token（worker 及以上） |
| | `POST /deliveries/{id}/ack`，`/fail`，`/answer` | 送达、失败、转达回答 | 同上 |
| | `GET /deliveries/unread`，`POST /deliveries/{id}/read` | Web 未读 | Web 登录 |
| | `GET/POST /forms/{一次性令牌}` | 一次性表单 | 令牌本身 |
| 运行 | `GET /runs`，`GET /runs/{id}` | 运行记录 | observer |
| 回放与健康 | `GET /why/{id}`，`GET /health` | 回放、健康视图 | observer |
| 能力包 | `GET /packs`，`GET/POST /instances`，`PATCH /instances/{id}` | 能力包与实例 | observer 读，delegate 写 |
| | `GET /rules`，`PATCH /rules/{id}/mode` | 规则与模式；切换只有 Web | Web 登录 |
| 设置 | `/settings/policy`、`/settings/tokens`、`/settings/ext-keys`、`/settings/allowlists`、`/settings/executors`、`/settings/commands`、`/settings/declarations`、`/settings/model-endpoints`、`/settings/access-limits`、`/settings/projects/{id}/allowed-executors`、`/settings/purge` | 策略、凭据、白名单、名单、命令登记、声明确认、物理删除 | 只有 Web 登录 |
| 认证 | `POST /auth/login`、`/logout` | Web 会话 | |

## B.3 错误响应

```json
{"code": "executor_not_allowed", "why": "执行者不在项目允许范围内", "next": "改选 allowed 中的执行者，或用 apa ask 交给本人", "allowed": ["work-host.codex"], "web_url": null}
```

- `why` 与 `next` 只写机制，不含账本里的用户内容。`allowed` 是 Web 中配置的执行者 id。
- HTTP 状态与退出码的对应：400 → 2；401 → 3；403 → 4、20、21（由 `code` 区分）；404 → 5；409 → 6；其余错误 → 1。不可达时 CLI 返回 7。

## B.4 标准事件类型

对外事件类型只增不改。初始集合：

`item.created`、`item.updated`、`item.phase_changed`、`decision.requested`、`decision.resolved`、`delegation.created`、`delegation.reported`、`command.rejected`、`material.revised`、`material.revision_proposed`、`guideline.activated`、`project.updated`、`project.allowed_executors_changed`、`rule.mode_changed`、`allowlist.changed`、`policy.changed`、`run.started`、`run.finished`、`source.failed`、`health.alert`。
