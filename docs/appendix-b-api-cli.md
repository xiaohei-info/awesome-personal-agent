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
| `apa repair-merges [--dry-run]` | 一次性修复（2026-11-23，主文档 §9.3）：把归类折叠掉的建议名下漏搬的消息搬到去处（每条在去处记合并留痕，`item` 为被折那条，判断方、理由、批次、分数同那次折叠），把仍是建议的名下挂着、没记进 `proposal.sources` 的消息补记进去；可重复执行，第二遍零写入。`apa serve` 占着监听端口时拒绝（退出码 6），先停服务；有未应用的迁移时拒绝。`--dry-run` 在账本的在线副本上照做一遍、输出同样的结果后删掉副本，服务开着也能跑，不动账本。输出 `{"ok", "dry_run", "items", "messages", "repaired": [{"item", "title", "into", "events"}]}`，`into` 等于 `item` 表示补记自己 | 主机本地 | |
| `apa merge run [--dry-run [--with-model] \| --apply] [--source <id\|前缀*>] [--project <id\|->] [--since <时刻>] [--until <时刻>] [--limit <n>]` | 手动跑一批跨来源合并（主文档 §9.3）：取待归类的建议（含本人补打的），按维护循环同一套规则（规则检索、资料上限、一次写作模型调用先分组再按优先级判定、第 6 层校验、`inbound.sweep` 落账），不等攒批条件；一批仍不超过 `batch_size`，其余留到下一次。筛选：来源（`origin.source`，结尾 `*` 为前缀）、项目（`-` 为不属于任何项目）、建立时刻 `[since, until)`（RFC 3339 或 `YYYY-MM-DD` 本地零点）、条数上限。缺省与 `--dry-run` 只演练：在账本的在线副本上算规则层，不调模型、不写账本，服务开着也能跑；`--with-model`（只配 `run`、不配 `--apply`）同样在在线副本上跑，但调一次写作模型、在副本上落账后读回，再删掉副本，真账本不动，输出另带 `"with_model": true` 与下面真跑才有的字段；`--apply` 才调模型并落账，不用停服务：`apa serve` 在跑时经数据目录 `host/serve.sock` 的主机本地入口（主文档 §14.12）交给服务进程执行，与维护循环同一个内核，规则层与模型调用不占归类锁、只有落账与维护循环互斥；没有服务在应答时离线执行（占住监听端口）；端口被占而主机本地入口不应答时拒绝（退出码 6）；离线执行时有未应用的迁移也拒绝。输出 `{"dry_run", "matched", "taken", "remaining", "batch", "rule_kept", "ask", "deferred", "chars"}`，`ask[]` 是送模型的成员（带资料编号 `num` 与仓库 + PR 号 `change`）与检索出的已有事项 `candidates[]`（按分数从高到低，至多 `merge.candidates` 条，检索不拦），另有 `topics[]`（给模型作已有主题的最近 20 批主题名，没有时不带），每条记录带 `item`、`title`、`source`、`ref`（origin.ref）、`project`、`phase`；同一账本状态下演练与真跑的这些字段相同。调过模型的另带 `groups[]`（`group`、`topic`、`members[]`，主题沿用了已有主题的带 `reused: true`）、`combined[]`（批内整合：`topic`、`carrier`、`members[]`、`title`、`summary`、`body`（要落到载体上的整段正文：要点清单 + 来源原文），没整合时 `rejected` 与 `why`，关键事实校验缺的片段在 `missing[]`，要点清单里缺的带「正文：」、成员原文里缺的带「原文：」前缀）、`check_failed`（校验没过、退回留着的整合项数）、`calls[]`（这一批的每次模型调用：`run`、`size` 问了几条、`fresh` 是否为不复用旧回答的重问、`reused`、`elapsed_ms`、`output_chars`、`complete`，不完整时 `missing` 缺结论的条数与 `error`：`missing`、`truncated` 等回答形态；回答始终不完整时命令报错、不落账，报告仍带 `calls[]`）与 `decisions[]`（从账本读回的结论，含 `topic`，被第 6 层拒绝的带 `rejected`，载体带 `integrated: true`）、`rejected`（按类别码计的拒绝条数）。可重复执行，已归类的不再命中 | 主机本地 | 2、6 |
| `apa merge mark-pending [--dry-run \| --apply] [--by <名字>] [筛选同上]` | 给还没有归类标记的建议补记待归类（`inbound.mark_pending`，主文档 §9.3）：只补 `proposed`、没有 `proposal.sweep` 的（含没有建议信息的迁移来的建议），合并关闭或来源不参与时跳过；补的记 `proposal.sweep.backfill`（`by` 取 `--by`，缺省为终端用户名，与时刻），事件记操作者。补打不触发归类：维护循环不自动归类补打的，要再跑 `apa merge run --apply`。幂等，第二遍零写入。缺省与 `--dry-run` 只列出将补哪些；`--apply` 的执行方式同上。输出 `{"dry_run", "by", "marked", "skipped"}`，`skipped` 按原因计数（`already_marked`、`merge_off`、`source_not_included`、`over_limit`） | 主机本地 | 2、6 |
| `apa login` | 保存 APA 地址与 token | 任意 token | 3 |
| `apa guide [主题] [--template]` | 输出使用说明，只写机制；`--template` 输出常驻指令模板 | 无 | |
| `apa declare <文件>` | 提交插件声明（无头运行；旧插件的 `executors[]` 仍被接受，确认后并入执行者名录），本人在 Web 确认后生效 | worker | 4 |
| `apa version` | 版本号、构建提交、各模块数据结构版本与各协议版本 | 无 | |
| `apa doctor [--json] [--upgrade-check]` | 自检（主文档 §15.5）；`--upgrade-check` 是升级前的只读预检（主文档 §16.5） | 主机本地；内核一侧的检查需 observer | 1 |
| `apa authz preview [--json] [--days 30]` | 权限模型切换的只读预演（主文档 §16.5）：在账本快照上列出切换会做什么（撤销的授权单、清理的旧问题状态、写入的 Agent 档位、去掉的旧策略段），按新分档重判最近 N 天 Agent 发出的命令（照常生效、变为等本人确认、被拒、无法判定），并列出 Agent、钥匙（不含哈希）、材料类别与参与判定的设置；不写账本 | 主机本地 | 1 |
| `apa backup create/verify/restore` | `create [--out <目录>] [--no-secrets]` 备份（默认带连接凭据的主密钥 `master.key`，`--no-secrets` 不带）；`verify <备份>` 校验；`restore <备份> --home <目录>` 恢复到空目录（主文档 §16.5） | 主机本地 | 1、6 |
| `apa secrets rekey` | 换连接凭据的主密钥：全部可解的密文用新密钥重新加密，再原子替换 `master.key`；只在服务停止时运行，服务仍在监听时拒绝；账本还没迁移到本二进制时拒绝（主文档 §14.2、§16.5） | 主机本地 | 2、6 |
| **上下文** | | | |
| `apa context [--session] [--scope project/cwd/item/all] [--item <id>] [--format inject]` | 账本增量投影，含路由为注入的投递和挂靠消息；`--scope item` 见主文档 §7.6 | observer | 3、5、7 |
| `apa persona [--lang zh\|en]` | 读取当前人格，供插件在新会话开始时注入；本人没写时是内置默认语气（主文档 §11.2）；语言：`--lang` > Web 里选的助手语言 > `LC_ALL`、`LC_MESSAGES`、`LANG` > 英文 | observer | 3、7 |
| `apa inbox` | 列出待决和建议 | observer | |
| `apa today` | 今天的清单：计划今天或更早且未完成的、今天到期的、待决的、久未推进的（主文档 §4.1） | observer | |
| **事项** | | | |
| `apa propose` | 以 `proposed` 新建；可带 `--deadline`、`--origin`、`--ext`（规则同 `apa item add`） | observer | |
| `apa item show/list` | 读事项；`show` 带挂靠消息（按转发白名单过滤）与进展；`list` 可带 `--q <全文>`、`--sort updated/deadline/planned_for/priority`、`--planned-for <日期>`、`--from <日期> --to <日期>`（按日期范围读，同 HTTP 的 `from`、`to`）、`--origin-source <来源>`、`--origin-ref <外部 id>` | observer | 5 |
| `apa item add/update/transition/relate` | 增改事项、转阶段、关联（`relate --to` 可以是事项或材料；`relate --key <键> [--domain <去重域>] [--remove]` 登记或移除关联键，主文档 §9.4）；`add`、`update` 可带 `--planned-for <日期>` 与 `--planned-slot "HH:MM - HH:MM"`（`update` 传空串清除；有时段必须有计划日期，否则退出码 2）；`add`、`update`、`propose` 可带 `--deadline <RFC 3339 时间或 YYYY-MM-DD>`（只给日期时取本机时区当天 23:59:59；`update` 传空串清除并取消截止定时）、`--origin <JSON>`（`{"source", "actor"?, "ref"?}`，`source` 不能以 `import:` 开头）、`--ext <JSON>`，与 HTTP 请求体对齐；`add`、`transition` 可带 `--evidence <说明或结果证据>`，进请求体的 `evidence`（委派子事项进入 `done` 必须带），只作记录，不参与权限判定。 | worker（自己的事项）；delegate（任意事项）。一档；以非 `proposed` 阶段新建、使事项离开 `proposed` 或 `needs_input` 为二档（见下文三档表） | 2、4、6、20、23 |
| `apa item progress <id> --text [--on <日期>]` | 追加一条进展，不改阶段；`--on` 不能晚于今天 | worker（自己的事项）；delegate（任意事项） | 2、4 |
| **决策** | | | |
| `apa ask <id>` | 写入决策请求并转 `needs_input` | worker | 4 |
| `apa decide <id> --option` | Agent 替本人回答问题（二档）：该 Agent 的档位放开拍板才生效，答案记为 Agent 所答，本人可在"Agent 改动"逐次撤回 | delegate | 2、4、6、23 |
| `apa accept <id>` | 接受建议（二档，立项） | delegate | 2、4、23 |
| `apa dismiss <id> [--reason]` | 丢弃建议（二档）；理由选填，缺省时事件理由为 `proposal dismissed` | delegate | 2、4、23 |
| `apa attach <id> --to <事项> [--reason]` | 把建议补到已有事项（二档）；补到之后不能逐条撤销（来源事件已改挂） | delegate | 2、4、5、6、23 |
| `apa proposals apply (--recommended \| --file <处置清单>) [--source] [--project]` | 批量处理建议：按推荐，或逐条给出处置；每条按对应的单条命令各自判定、各自成败（批量等于逐条），输出逐条结果；`--file -` 从标准输入读处置清单，供插件不落临时文件调用。按推荐处理必须带范围（`--source` 或 `--project` 至少一项），都不带时拒绝、什么都不处理，退出码 2（2026-11-23，见 B.6） | delegate | 2、4、23 |
| **委派** | | | |
| `apa executors [--project]` | 列出执行者名录（主文档 §6.17），带项目时标明是否允许 | observer | |
| `apa delegate --parent --assignee` | 建子事项，校验项目允许的执行者。`--executor <JSON 文件或 ->`（`kind`、`host`、`label`、`handle`）在同一事务里登记或更新执行者，`harness` 取 `APA_HARNESS`；`assignee` 不必先在名录里；身份字段冲突或名录已满返回 6 | worker（自己的事项）；delegate（任意） | 2、4、5、6、21 |
| `apa dispatch add <子事项> [--session] [--resumable] [--cwd] [--repo] [--handle <JSON 文件或 ->] [--continues <记录>] [--idempotency-key]` | 在委派子事项上写一条委派记录，输出该记录；`harness` 取 `APA_HARNESS`，必填；`--resumable` 要有 `--session`；同一子事项上已有运行中、harness 与会话都相同的记录时不新增，返回已有记录；句柄规则见主文档 §6.17 | 同 `apa report` | 2、4、5、6 |
| `apa dispatch update <记录> [--state ended\|lost] [--resumable true\|false]` | 推进委派记录；`lost` 同时置 `resumable` 为假，终态不可改 | 同 `apa report` | 2、4、5、6 |
| `apa report <id> --outcome --summary [--pr --commit --branch --link] --ack-cursor` | 回执：进展、完成、阻塞，确认挂靠消息；`done` 必须带结果证据（`--pr`、`--commit`、`--branch`、`--link` 至少一项），附不出只能报 `progress` 或 `blocked`。`--link` 可重复给出多个；报 `done` 时落在已登记笔记库内的链接（`vault:<库名>/<路径>`）自动登记为链接型材料并以"产出"关联到父事项（主文档 §7.5） | 绑定该 assignee 的 worker、该运行的运行凭据、delegate | 2、4、6 |
| `apa handback <消息> --reason` | 把挂靠消息交回一次 | 同上 | 4、6 |
| **项目** | | | |
| `apa project list/show` | 读项目，含 repos 与允许的执行者 | observer | |
| `apa project create/update` | 建项目、改基本信息（一档）；改对话捕获开关为 3a。`allowed_executors`、远程地址与外发范围是 3b，只在 Web | delegate | 20、23 |
| `apa project repos add/remove` | 增改本机检出（一档）；远程地址是 3b，只在 Web | worker | 4、20 |
| **材料与规范** | | | |
| `apa material add --repo --path [--project] [--kind]` | 登记托管型；project 缺省取登记了该仓库的项目，kind 缺省 `design`。`apa material add --link <位置> --title <标题> [--project] [--kind]` 登记链接型材料（主文档 §12.8）：位置是 `vault:<库名>/<库内相对路径>` 或网址，kind 缺省 `reference`；同一项目内同一位置重复登记返回已有的那条。`apa material show` 对链接型只回位置与打开地址 | worker | 2、5 |
| `apa material commit <id> --from --reason [--kind]` | 提交自有型修订 | worker | 6 |
| `apa material list` | 列出材料，可按项目、kind、标签筛选 | observer | |
| `apa material show <id> [--rev]` | 读当前或指定修订；托管型只回位置 | observer | 5 |
| `apa material search <查询>` | 检索，只回 id、标题、片段 | observer | |
| `apa blob put/get` | 附件 | worker / observer | |
| `apa agents list` | 查看 Agent 卡、默认选择与可用原因 | observer | — |
| `apa agents check` | 比对工作区 AGENTS.md | observer | 10、11 |
| `apa agents sync` | 写入工作区；漂移时不覆盖并返回 6，本人选择"覆盖"后才写入 | observer | 6 |
| **来源** | | | |
| `apa source list/show` | 来源与统计 | observer | |
| `apa source add` | 登记附带的拉取程序或 webhook source，可带 `--default-project`、`--name`（显示名；不带时取来源程序接入声明的名字，输出里给出；程序没有声明又不带时退出码 2；重名退出码 6）；登记来源是 3b，只在 Web；CLI 调用返回 20 | Web 登录 | 20 |
| `apa source auth <id>` | 以该来源登记的 `credentials_path` 启动附带拉取程序的认证模式（在本机运行 `<来源命令> auth <credentials_path>`，继承标准输入输出），凭据写在该路径；不是 pull 类型或未登记 `credentials_path` 的来源返回 2，工具适配的来源用工具自己的认证命令（主文档 §9.7） | 主机本地，经 API 读来源登记，需 observer | 2、5 |
| `apa source rename <id> <名字>` | 改显示名（3a，主文档 §9.1）；空串改回接入声明的名字（没有声明时退出码 2）；重名退出码 6；记 `source.renamed` | delegate | 6、23 |
| `apa source enable/disable` | 启停（3a） | delegate | 23 |
| `apa source webhook create/rotate/revoke` | 创建、轮换、吊销 webhook 接收密钥（3b，只在 Web；CLI 调用返回 20） | Web 登录 | 20 |
| `apa source ingests` | 查看原始 payload、校验结果、转换结果 | observer | |
| `apa source rejects <id> [--after N] [--limit N]` | 查看没有入账的输出行及原因（`next_after` 作为下一次的 `--after`） | observer | |
| `apa source event <事件 id> [--part body\|original\|headers\|attachments\|messages]` | 读一条来源事件及其原文（主文档 §7.6）：不带 `--part` 输出事件、原文清单与正文，带时只输出那一部分。token 读到的内容按该来源对外部端点的外发范围裁剪，输出写明所用范围与裁剪量；运行凭据不可用 | observer | 4、5 |
| `apa source replay <入站记录>` | 重放 | delegate | |
| `apa source backlog list [--source] [--sort score]` | 列出积压（未判定与已到期），旧原因 `undecided`、空原样返回；`--sort score` 按影子分数从高到低排（各来源合并后统一排序，同分保持来源顺序），每条带 `score`，没有分数的排最后 | observer | |
| `apa source backlog rules [--source]` | 积压只跑规则：重走归属与确定性规则，能挂靠的挂靠、规则有明确出口的按出口处置，其余原样；不调模型，不受 `unjudged_rejudge_days` 限制，输出 `attached`、`disposed`、`remaining`（主文档 §9.3） | delegate | |
| `apa source backlog score [--source]` | 影子打分：用判断端点给积压按批（每批不超过判断端点的 `max_batch`）打分，只写带 `shadow` 的判断记录，不改处置、不建事项、不问对话端点；输出 `scored`、`skipped`（外发范围不允许送出的，或判断端点没有给出概率的）；没有判断端点时返回 6（说明没有判断端点），不写记录；模型已暂停时返回 6（`models_paused`），什么也不做（主文档 §8.7、§9.3） | delegate | |
| `apa source backlog rejudge [--source] [--limit N]` | 放开重判：剩余积压（含已 `expired` 与旧原因）按分数从高到低（没有判断端点时按落盘顺序，由对话端点逐条回答同一道初筛题）走完整流水线；进入建议流的数量受每日上限 `backlog_release_per_day` 约束，被忽略的不占额度，当天用完只推迟、次日继续；`--limit N` 把本次进入建议流的数量再压到 N 以内；输出 `requeued`、`deferred`；模型已暂停时返回 6（`models_paused`），不放回任何事件（主文档 §8.7、§9.3） | delegate | |
| `apa source backlog restore --from <时间> --to <时间> [--source] [--dry-run]` | 把时间段内被旧级联的判断忽略、没有判断记录的事件放回积压（未判定原因 `restored`），之后只经上面三步重判（主文档 §9.3）；`--dry-run` 只返回命中条数与按来源的分布，不写入 | delegate（`--dry-run` 为 observer） | |
| **对外** | | | |
| `apa events --follow [--cursor]` | 跟随事件流 | observer | |
| `apa ext list` | 列出仓库提供的 ext | 无 | |
| `apa ext run <名>` | 本机启动 ext | ext 密钥（由 ext 读取） | |
| `apa ext check` | 协议一致性自测 | 无 | |
| `apa connection list [--direction channel]` | 列出连接（内置渠道）：id、方向、类型、名字、启停、状态、问题代码与最近成功、失败的时间；不含配置值与凭据（主文档 §7.6）。就是 `GET /connections` | observer | |
| **自动化** | | | |
| `apa instance add/list/enable/disable` | 能力包实例（3a；改实例的执行 Agent 为 3b） | delegate（`list` 为 observer） | 20、23 |
| `apa pack list` | 列出已加载能力包 | observer | |
| `apa pack validate <目录>` | 本地校验能力包 | 无 | |
| **可观测** | | | |
| `apa why <id>` | 回放事项或运行 | observer，按权限裁剪 | 5 |
| `apa health` | 健康视图 | observer | |
| `apa run list/show` | 运行记录 | observer | |
| `apa judgment stats [--use] [--template] [--source] [--since]` | 各级调用次数与升级率、未定率、接受率、误丢率（主文档 §8.10） | observer | |
| `apa judgment show <id>` | 一条判断记录的逐级结果 | observer，按权限裁剪 | 5 |
| `apa judgment spot-check` | 列出待抽检的条目；抽检结论只在 Web 填 | observer | |

**三档与硬线**（主文档 §14.3）：每条登记命令固定在下表的一档，不随调用者、对象归属或通道变化；少数命令按这一次的状态变化取更高的档（表内注明）。钥匙只决定能碰哪些对象（主文档 §14.2），档位决定 Agent 发起时能否直接生效：一档永远开；二档、3a 由本人在 Web 给每个 Agent 选档位放开（`PUT /settings/agents/{agent}/tier`，二档可限定项目）；3b 永不放开，只有本人在 Web 做，且须刚输过口令。Web 上的本人不受档位限制。判定顺序是硬线 → 钥匙范围 → 档位 → 项目允许的执行者，硬线不读档位。

| 档 | 命令 | Agent 发起而档位未放开时 |
|---|---|---|
| 读 | `credential.list`（只有 Web 登录）、`executor.list`、`executor.pending`、`delegate.dispatch_list`、`blob.get` | 不涉及 |
| 一档 整理 | `decision.request`；`delegate.create`、`delegate.report`、`delegate.dispatch`、`delegate.dispatch_update`；`followmsg.handback`；`rule.create_timer`；`material.commit`、`material.link`、`material.add`、`blob.put`；`inbound.capture`、`inbound.propose_unjudged`、`inbound.split`、`inbound.unmerge`、`inbound.ignore_unjudged`、`inbound.backlog_rules`、`inbound.backlog_score`、`inbound.backlog_rejudge`、`inbound.backlog_restore`；`project.create`、`project.update`、`project.checkout.set`、`project.checkout.remove`；`source.replay`、`source.fetch_attachment`；`executor.declare`；`agents.check`、`agents.sync`；`item.create`、`item.update`、`item.transition`、`item.relate`、`item.progress`。按状态升档：以 `proposed` 之外的阶段新建、使事项离开 `proposed` 或 `needs_input` 的转阶段为二档；改事项的指定 Agent、改项目的对话捕获开关为 3a | 不涉及：一档永远开，可在"Agent 改动"撤回 |
| 二档 拍板 | `decision.answer`、`proposal.accept`、`proposal.dismiss`、`proposal.attach`、`revision.confirm`、`revision.reject`、`channel.answer`。按状态升档：回答 AGENTS.md 漂移问题、确认或拒绝规范类修订为 3a。`channel.answer` 是本人经渠道回答，只看渠道回答人白名单，不看档位 | 退出码 23（`tier_closed`），不写入；`web_url` 指向该对象。修订在档位未放开时留待确认（主文档 §14.3） |
| 3a 改日常 | `source.set_enabled`、`source.rename`、`source.set_content_retention`、`retention.set`；`instance.create`、`instance.update`、`instance.toggle`、`instance.run`；`item.start`、`item.revert`；`rule.set_mode`；`judgment.review`；`material.revert`、`material.update`；`agents.resolve`。按状态升档：新建或修改实例时改了它的执行 Agent 为 3b | 退出码 23（`tier_closed`），`web_url` 指向该动作的设置页 |
| 3b 造新权力 | `credential.issue`、`credential.revoke`、`credential.assign`；`source.webhook_key`、`source.webhook_revoke`、`source.register`、`source.register_script`、`source.update`、`source.update_script`、`source.set_outbound`；`connection.create`、`connection.configure`、`connection.enable`、`connection.disable`、`connection.rename`、`connection.remove`；`executor.confirm`、`executor.reject`、`executor.register`、`executor.put`、`executor.remove`；`agent.configure`；`project.repo.add`、`project.repo.remove`、`project.capture_outbound.set`、`project.allowed_executors.set`；`purge.object` | 退出码 20（`hard_line`），不论档位；Web 会话没刚输过口令为退出码 20（`reauth_required`） |
| 系统内部 | `source.ingest`、`source.run_result`；`inbound.dispose`、`inbound.sweep`、`inbound.mark_pending`、`inbound.requeue`、`inbound.expire`；`followmsg.attach`、`followmsg.ack`、`followmsg.release`；`delegate.settle`；`followup.settle`；`retention.purge_content`、`retention.report`；`agents.render`；`revision.summarize`；`material.refresh`、`material.relocate` | 没有对外入口；Agent 凭据调用为退出码 20（`hard_line`） |

不走命令的设置路由按段定档（主文档 §14.3）：3a 是打扰与提醒（`disturb`）、人格与助手语言、渠道的登记属性（`channels`）、不确定带与影子模式、抽检设置、暂停模型、信任的发件人（转发白名单）、来源的原文保留天数；3b 是工具档位上限（`permission_cap`）、判断端点与生成端点、渠道回答人、默认 Agent 与后台由谁干（`default_agent_id`、`main_agent_id`、`rule_agent`、`default_harness`）、启动方式（`main_session_launch`）、笔记库、Agent 档位本身、删除渠道与识别回答人、外发范围、口令与会话。CLI 没有这些设置的写命令。

钥匙范围外（observer 写入、worker 改别人的事项、运行凭据写本次目标之外的对象）为退出码 4（`out_of_scope`）。三种拒绝都记 `command.rejected`，走同一条留痕路径。

**会话身份**：环境变量 `APA_SESSION`、`APA_HARNESS` 由接入方（harness 插件）设置，CLI 随每次请求以 HTTP 头 `X-APA-Session`、`X-APA-Harness` 发出，只用于把 Event actor 审计到会话粒度，不参与任何权限判定（主文档 §14.2）。

<details>
<summary><b>已取消（2026-11-20）：原"只在 Web"清单、代理写命令清单、授权单与会话类型说明</b>。理由：由上面的三档与硬线取代（主文档 §14.3、决定 84）。原文保留如下，其中的命令与参数已不再登记。</summary>

**只在 Web**：策略、信任级、token、ext 密钥、web_only 分类、两份白名单、项目允许的执行者、移除执行者名录条目、规则切 `auto`、运行权限上限、判断端点与生成端点、不确定带与影子模式、抽检结论、打扰上限与静音、来源与对话捕获的外发范围、登录口令与在线会话、可执行命令登记（另可由本人写主机配置文件，§14.12）、撤销 agent 变更、物理删除、规范生效、签发和撤销授权单。CLI 没有对应的写命令；相关命令（如 `apa project update`、`apa source add`）带上这些字段时返回退出码 20。

**工作台能力迭代新增的只在 Web 的操作**：笔记库登记（名字与打开方式）、来源的原文保留天数、来源事件的物理删除、导入用授权单的签发。本人在 Web 上传导入计划文件不需要授权单。

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
| 链接型材料与来源原文 | `material.link` | | `source.fetch_attachment` |
| 决策 | `decision.request` | `decision.answer`、`proposal.accept`、`proposal.attach`、`proposal.dismiss`、`proposal.batch` | |
| 材料 | `material.commit`、`material.add`、`blob.put` | `revision.confirm`、`revision.reject` | |
| 委派 | `delegate.create`；`delegate.report`、`delegate.dispatch`、`delegate.dispatch_update`、`followmsg.handback`（绑定的事项）；`executor.declare` | `delegate.report`、`delegate.dispatch`、`delegate.dispatch_update`、`followmsg.handback`（其他事项） | |
| 项目 | `project.checkout.set`、`project.checkout.remove` | | `project.create`、`project.update`、`project.repo.add`、`project.repo.remove` |
| 来源与能力包 | | | `source.register`、`source.update`、`source.set_enabled`、`source.webhook_key`、`source.webhook_revoke`、`source.replay`、`inbound.backlog_rules`、`inbound.backlog_score`、`inbound.backlog_rejudge`、`inbound.backlog_restore`、`instance.create`、`instance.update`、`instance.toggle`、`instance.run` |

授权单的 `actions` 只能取"可用授权单"一列的命令（`import.apply` 已取消，不能签发，主文档 §14.5）。运行凭据只能使用签给本次运行所属 Agent（`grantee.agent` 等于运行记录的 harness）且允许后台使用的单，`apa grant list` 与 `GET /grants` 对运行凭据只列出这些单（凭据归属某个 Agent 的 delegate 另能看到签给该 Agent 的单），对象须同时在运行范围内，触发不可信时为退出码 23（`untrusted_trigger`）；`grantee.agent` 式的单放行 `proposal.accept`、`proposal.attach` 时，建议须来自可信发件人（`untrusted_origin`，主文档 §14.5）。

**会话身份**：环境变量 `APA_SESSION`、`APA_HARNESS`、`APA_SESSION_KIND` 由接入方（harness 插件）设置，CLI 随每次请求以 HTTP 头 `X-APA-Session`、`X-APA-Harness`、`X-APA-Session-Kind` 发出。前两项让 Event actor 审计到会话粒度；`X-APA-Session-Kind` 取 `direct`、`group`、`unknown`，缺失等同 `unknown`，只有 `direct` 时内核接受凭本人原话的第二档命令（主文档 §11.2、§14.5、§17.4）。

</details>

## B.2 HTTP API

| 分组 | 方法与路径 | 说明 | 凭据 |
|---|---|---|---|
| 事项 | `GET/POST /items`，`GET/PATCH /items/{id}` | 查询、创建、更新。`GET /items` 可带 `q`（全文）、`sort`（`updated`、`deadline`、`planned_for`、`priority`）、`phase`、`project`、`tag`、`assignee`、`planned_for`、`from` 与 `to`（日期范围） | 按 §14.3；导出 ext 密钥可 `GET` |
| | `GET /today` | 今天的清单，六组：`planned`、`due`、`pending`、`stale`、`overdue`（截止的本机日早于今天、未结束，按截止先后）、`high_priority`（紧急或高、未结束，紧急在前）；一件事可在几组里同时出现 | observer |
| | 已有路由 GET /items 的新增查询参数 | `origin_source`、`origin_ref`：按事项的 `origin.source` 与 `origin.ref` 精确筛选，可只给前者（主文档 §7.1） | observer |
| | 已有路由 GET /items 的新增查询参数 | `progress_from`、`progress_to`：只列进展日落在范围内的事项，响应另带 `progress`（主文档 §7.1） | observer |
| | `POST /items/{id}/progress` | 追加进展 `{text, on?}`；`GET /items/{id}` 返回 `progress[]` | worker（自己的事项）；delegate |
| | `POST /items/{id}/correlation-keys`，`DELETE /items/{id}/correlation-keys` | 登记或移除关联键 `{key, domain?}`（经 `item.relate`；`GET /items/{id}` 返回 `correlation_keys[]`） | 同 `relations` |
| | `GET /items/{id}/messages` | 事项的全部挂靠消息 | observer；运行身份和无人值守事项按转发白名单过滤；ext 密钥不可用 |
| | `POST /items/{id}/transition`，`POST /items/{id}/relations` | 转阶段、关联 | 同上 |
| | `POST /items/{id}/revert` | 撤销 agent 变更，含 Agent 替本人的回答（3a） | Web 登录；delegate 按档位 |
| | `POST /items/{id}/start` | 现在开工（`item.start`，主文档 §13.6）：事项须是 `open`，否则 409；记内部事件 `item.start_requested`，规则引擎随后执行能为它拉起后台运行的规则，不看规则模式，权限上限、项目允许的执行者、链路深度与在途运行照常复核；返回 202 与 `{"id"}`（3a） | Web 登录；delegate 按档位 |
| 决策 | `POST /items/{id}/ask` | 写入决策请求 | worker |
| | `POST /items/{id}/decide`，`/accept`，`/dismiss` | 回答或处理建议（二档）：delegate 按其 Agent 的档位，未放开为 403（退出码 23）；旧客户端带的 `X-APA-Evidence`、`X-APA-Grant`、`X-APA-Session-Kind` 一律忽略 | Web 登录、delegate |
| | `POST /items/{id}/attach` | 把建议补到已有事项，请求体 `{to, reason?}`（二档） | Web 登录、delegate |
| | `POST /proposals/batch` | 批量处理建议，逐条按单条命令判定，见下 | Web 登录、delegate |
| 委派 | `GET /executors` | 执行者名录：`id`、`kind`、`host`、`harness`、`label`、`handle`、`created_at`；带 `project` 时另带 `allowed` | observer |
| | `DELETE /executors/{id}` | 移除名录条目，已有事项与委派记录里的 id 原样保留 | 只有 Web 登录 |
| | `POST /delegations` | 建子事项 | worker |
| | `POST /items/{id}/report` | 回执 | worker |
| | `POST /items/{id}/dispatches` | 写委派记录 `{session, resumable?, cwd?, repo?, handle?, continues?}`，返回 201 与记录 | 同回执 |
| | `PATCH /dispatches/{id}` | 推进委派记录 `{state?, resumable?}` | 同回执 |
| | `GET /dispatches` | 列委派记录，可带 `executor`、`item`、`resumable`、`limit` | observer；运行凭据限范围内的事项 |
| | `POST /messages/{id}/handback` | 交回挂靠消息 | worker |
| 项目 | `GET/POST /projects`，`GET/PATCH /projects/{id}` | 项目；建与改是一档，改对话捕获开关是 3a | observer 读，delegate 写 |
| | `POST /projects/{id}/repos` | repos 属性：本机检出是一档，远程地址是 3b | worker；远程地址只有 Web 登录 |
| | `GET /projects/resolve?path&host` | 当前目录到项目的映射 | observer |
| 材料 | `GET/POST /materials`，`GET /materials/{id}` | 材料；单个材料带与事项的类型化关系 `relations` | observer 读，worker 写；导出 ext 密钥可 `GET` |
| | `GET /materials/{id}/content` | 托管型正文，异步读取 | 只有 Web 登录 |
| | `POST /materials/{id}/changes` | 对托管型发起修改，生成委派子事项 | Web 登录、delegate；worker、运行凭据须带自己范围内的 `parent` |
| | `GET/POST /materials/{id}/revisions` | 修订 | observer 读，worker 写 |
| | `POST /materials/{id}/revert` | 回退一次 `direct` 修订（3a） | Web 登录；delegate 按档位 |
| | `PATCH /materials/{id}` | 改链接型材料的标题、kind 与标签 `{"title"?, "kind"?, "tags"?, "reason"?}`，经 `material.update`；位置不可改，非链接型返回 409（3a） | Web 登录；delegate 按档位 |
| | `POST /revisions/{id}/confirm`，`/reject` | 确认或拒绝 proposed 修订（二档；规范类为 3a） | Web 登录、delegate |
| | `GET /revisions/pending` | 全站等本人确认的 proposed 修订，先提交的在前：`{"pending": [{"revision", "material", "baseline_changed"}]}`；只读 | 同材料读取 |
| | `GET /materials/search` | 检索 | observer |
| | `PUT/GET /blobs/{hash}` | 附件 | worker 写，observer 读 |
| | 已有路由 POST /materials 的新增请求体 | 带 `link` 键时登记链接型材料：`{"link", "title", "project", "kind"?, "tags"?}`，经 `material.link`；已有同项目同位置的返回 200 与那一条，新建返回 201。`GET /materials` 与 `GET /materials/{id}` 的每条材料加只读字段 `form`（`hosted`、`owned`、`link`），链接型另带 `link` 与 `open_url`（库已登记打开方式时） | worker 写，observer 读 |
| | `GET /note-vaults` | 已登记的笔记库：`{"vaults": [{"name", "open_url"}]}` | observer |
| 规范 | `GET /agents-md?repo` | 取渲染结果，带最近一次检查状态 | observer |
| | `POST /agents-md/check` | 上报文件 hash，返回三态 | observer |
| 上下文 | `GET /context` | 账本增量投影 | observer |
| | `GET /context/persona` | 当前人格，前面带内核固定声明；本人没写时是内置默认；语言：查询参数 `lang`（`zh` 或 `en`）> 策略的 `persona_lang` > 查询参数 `locale`（调用方的区域设置，`zh` 开头为中文）> 英文；另带 `default`，即该语言的默认原文（不含声明） | observer |
| | `POST /capture` | 提交对话增量 `{text, project?, path?, host?, failures?}`；默认所有项目都收；运行身份下、会话不在任何项目（`no_project`）、项目捕获被直接调 API 关掉（`capture_disabled`）时不收，返回 200 `{"captured": false, "reason"}`，后两种各记一条 `inbound.capture_failed`（只有类别与时间）；`failures` 是插件补报的没送达的失败 `[{at, class}]`，类别取 `kernel_unreachable`、`no_credentials`、`kernel_refused`、`plugin_error`（其余按 `plugin_error`），一次至多 20 条，晚于现在的时间按现在算；候选在提交后异步抽取 | worker |
| 插件声明 | `POST /declarations` | 提交无头运行声明（旧插件可带 `executors[]`），进 Web 待确认 | worker |
| 来源 | `GET/POST /sources`，`GET/PATCH /sources/{id}` | 来源登记。登记与修改配置是 3b；`PATCH` 只改启停或显示名是 3a | observer 读；登记与改配置只有 Web 登录；启停与改名 delegate 按档位 |
| | `GET /sources/declared-name?type&command&source_type&config` | 来源程序接入声明的显示名（中英），新登记表单预填 | observer |
| | `POST /sources/{id}/webhook` | 创建或轮换接收密钥（3b） | 只有 Web 登录 |
| | `DELETE /sources/{id}/webhook` | 吊销该来源全部接收密钥，同时删除 HMAC 密钥文件（3b） | 只有 Web 登录 |
| | `GET /sources/{id}/ingests`，`POST /ingests/{id}/replay` | 入站记录与重放 | Web 登录与内部钥匙；外部钥匙与运行凭据不能读入站记录（返回 403：原始请求体不按档位裁剪，也未经转发白名单） |
| | `GET /sources/{id}/events?disposition&sort&after&limit` | 来源事件，`disposition` 可逗号分隔多个；内部来源 `capture:<项目>` 只有 Web 登录能读；外部钥匙按它对该来源的档位裁（见下），没给这个来源返回 403（退出码 4） | observer 及以上、有档位的导出 ext 密钥；运行凭据不可用（返回 403，挂靠消息经转发白名单读取） |
| | `GET /sources/{id}/rejects?after&limit` | 被拒收的输出行及原因 | observer；运行凭据不可用（返回 403，原始行是未过滤的外部输出） |
| | `POST /source-events/{id}/propose` | 把未判定事件或 digest 事件转为建议 | Web 登录、delegate |
| | `POST /source-events/{id}/split` | 把误并入一条建议的来源事件拆出来另成一条建议（`inbound.split`，主文档 §9.3）：请求体 `{"from": "<建议 id>"}`，返回 `{"item": "<新建议 id>"}` | Web 登录、delegate |
| | `POST /items/{id}/unmerge` | 撤销跨来源合并的一条折叠（`inbound.unmerge`，一档，主文档 §9.3）：被折叠的建议回到 proposed，`proposal.sweep.action` 记 `undone`，与去处互记 `not_same`；合并的来源事件与留痕从去处移回，并入的在去处追加一条撤回说明；撤销一条批内整合过的成员时整组还原（同一次整合的其他成员一并回到建议，载体的标题与摘要恢复成 `proposal.integrated` 里的原文，本人事后改过的不覆盖）。返回 `{"item": "<id>"}`；不是被折叠的或已撤销过是 409 |
| | `GET /merge/activity?days=7` | 最近自动归类（读）：`{"days", "pending", "model_calls", "chars", "batches": [{"batch", "at", "size", "run"?, "chars", "decisions": [{"item", "title", "source"?, "sources", "action", "target"?, "target_title"?, "target_phase"?, "why"?, "by", "hint"?}]}]}`；`days` 至多 90；`sources` 是这条建议带着的消息条数：合并进别处的，按去处上从它搬去的合并留痕计（含不在它 `sources` 里的挂靠消息），其余按它自己的 `proposal.sources` |
| | `GET /packs/{pack}/prompts/{name}` | 已加载能力包里一个提示词的正文（不含头部声明）：`{"pack", "name", "agent", "body"}`；设置 › 跨来源合并用它显示内置默认与"恢复默认" |
| | `POST /source-events/{id}/ignore`，`POST /source-events/ignore` | 忽略未判定事件：单条；或批量，请求体 `{"source"}` 按来源、`{"all": true}` 全部 | Web 登录、delegate |
| | `POST /source-events/rules` | 积压放开第 1 步：只跑规则，返回 `{"attached", "disposed", "remaining"}`；请求体 `{"source"?}` | Web 登录、delegate |
| | `POST /source-events/score` | 积压放开第 2 步：影子打分，返回 `{"scored", "skipped"}`；请求体 `{"source"?}`；没有判断端点是 409；模型已暂停是 409 `models_paused` | Web 登录、delegate |
| | `POST /source-events/rejudge` | 积压放开第 4 步：放开重判，返回 `{"requeued", "deferred"}`；请求体 `{"source"?, "limit"?}`；模型已暂停是 409 `models_paused` | Web 登录、delegate |
| | `GET /source-events/restorable?from&to&source` | 预览：时间段内可放回积压的旧判断忽略事件的条数与按来源的分布 | observer；运行凭据不可用 |
| | `GET /source-events/{id}/outbound-preview` | 外发预览：这条事件按 `full`、`excerpt`、`meta` 裁剪后实际送给模型的内容，与外发同源（主文档 §14.14）；只读，不写 Event | Web 登录与内部钥匙；外部钥匙与运行凭据不可用（返回 403）；内部来源 `capture:<项目>` 的事件只有 Web 登录能读 |
| | `POST /source-events/restore` | 把旧级联忽略的事件放回积压；请求体 `{"from", "to", "source"?}` | Web 登录、delegate |
| | `GET /source-events/{id}` | 单条读取来源事件及其原文，见下 | Web 登录与内部钥匙读全文；外部钥匙按钥匙的逐来源档位裁剪，没给的来源 403；运行凭据只经挂靠消息与转发白名单；非导出 ext 密钥不可用（403） |
| | `POST /source-events/{id}/attachments/{n}/fetch` | 取回第 n 个附件（`source.fetch_attachment`，主文档 §7.6）：成功返回 200 与更新后的附件清单项（带 `blob`）；来源程序不支持取回是 409，取回失败是 502 并带原因 | Web 登录、delegate |
| | `GET /storage` | 存储用量：`{"ledger_bytes", "blobs_bytes", "content_bytes", "sources": [{"id", "name", "events", "content_bytes", "oldest", "retention_days"?}]}` | observer |
| | 已有路由 GET /sources/{id}/events 与 GET /items/{id}/messages 的改动 | 列表不再带正文全文：每条事件的 `detail` 是前 2000 个字符的预览，另带 `content`（原文清单：各部分的字节数、总字节数、`state`、`missing?`）；全文经上面的单条读取（见 B.6） | 同原路由 |
| 判断 | `GET /judgments?subject&use&template&source&after&limit`，`GET /judgments/{id}` | 判断记录（主文档 §6.16），不含资料正文 | observer；运行凭据限目标事项 |
| | `GET /judgments/stats?use&template&source&since` | 各级调用次数与四项指标（主文档 §8.10） | observer |
| | `GET /judgments/spot-check`，`POST /judgments/{id}/review` | 抽检池；填抽检结论 `{result: "should_see" \| "correct"}`（3a） | 读 observer；填 Web 登录，delegate 按档位 |
| 入站 | `POST /ingest/{source_id}` | webhook 接收路由 | webhook 接收密钥 |
| Agent | `GET /agents` | 卡片列表与选择预览（`rule`、`runner`、`schedule`、`item_agent`、`harness`），本人可见实例参数；每张卡带 `tier`（`organize`、`decide`、`rules`，没设过为 `organize`）与 `tier_projects`（拍板限定的项目，空为全部） | observer |
| 设置 | `POST /settings/agents` | 创建或更新卡片，更新时须带原 `id`、`harness` 和 `rev`。同时修改权限上限时传 `permission_cap` 和 `policy_version`，在同一事务生效（3b） | 只有 Web 登录 |
| 事件 | `GET /events?cursor=` | JSON 长轮询 | observer、导出和转发 ext 密钥 |
| 投递 | `GET /deliveries/next?wait=` | 长轮询领取；有租约 | 渠道 ext 密钥；网关插件 token（worker 及以上，只领唤醒类投递） |
| | `POST /deliveries/{id}/ack`，`/fail`，`/answer` | 送达、失败、转达回答 | 同上 |
| | `POST /channels/identify` | 渠道 ext 上报谁发来了识别口令：`{code, sender, display}`。`sender` 是平台用户 ID；ext 密钥的渠道须与发起识别的渠道一致，口令不符、已用过或过期返回 404。不写名单 | ext（channel） |
| | `GET /deliveries/unread`，`POST /deliveries/{id}/read` | Web 未读；`read` 即本人确认（投递转 `acknowledged`） | Web 登录 |
| | `GET /deliveries/today` | 今天的推送计数，与打扰日上限同口径（主文档 §7.4）；只读 | Web 登录 |
| | `GET /forms/{令牌}` | 旧的一次性表单链接：302 到需要登录的 `/decisions`，不兑现令牌、不回答 | 不需要凭据 |
| 运行 | `GET /runs?state&category&target&before&limit`，`GET /runs/{id}` | 运行记录 | observer |
| | `POST /runs/{id}/credential` | 把唤醒运行的 id 换成短期运行凭据 | worker、delegate（网关插件 token） |
| 回放与健康 | `GET /why/{id}`，`GET /health` | 回放（来源事件命中数值条件规则时另带 `alerts`：规则、条件文本、上一次与本次取值、产出的投递及其状态（提醒还被静默或合并窗口暂存时状态为 `held`），各凭据级别可见）、健康视图；所有健康告警都经同一处产出健康类投递（§15.2），读取走 `GET /deliveries/unread` | observer |
| | `GET /source-events/{id}/trace`，`GET /items/{id}/trace` | 处理经过（主文档 §15.1）：消息经过是 `{message, entries}`，事项经过是 `{origins, more_origins?, entries}`；每行 `{at, who: {kind, name?, role?}, kind, args?, item?: {id, title, gone?}, mech?, steps}`，`steps` 与 `/why` 的 Step 同形、同样按凭据级别裁剪。`kind` 与 `args` 见下文"处理经过的行"；内部来源 `capture:<项目>` 的消息只有 Web 登录能读 | observer；运行凭据 403 |
| | `GET /doctor` | 内核一侧的自检结果（主文档 §15.5）；每项检查与每个对象都带稳定机器码 `code` 与可变参数 `data`，形状见下文"自检结果"。已保存端点的探测通过、且该端点有未恢复的失败时，记一条探测通过的 `run.finished`（同测试连接），健康视图据此清除该端点此前的失败（§15.2） | observer |
| | `GET /audit/rule-changes?agent&before&limit` | 规矩改动清单（主文档 §15.3）：3a 的每次改动 `{"changes": [{seq, at, actor, agent?, command, object, sections?, before?, after?, revert: {ok, why?}}]}`，最新的在前；`command` 为 `policy` 时是策略段改动，`before`、`after` 是该段的前值与后值；`revert.why` 是撤不回的原因码：`run_started`（已拉起的运行）、`data_purged`（已清除的数据）、`superseded`（之后又被改过）、`not_revertible` | 只有 Web 登录 |
| | `POST /audit/rule-changes/{seq}/revert`，`POST /audit/rule-changes/revert` | 按次撤回一条（撤不回为 409，`reason` 为上面的原因码）；按 Agent 批量撤回 `{"agent"}`，逐条各自成败，返回 `{"results": [{seq, ok, reason?}]}`。撤回以本人身份执行，记各自的事件 | 只有 Web 登录 |
| | `GET /version` | 版本与各协议版本（B.5） | 不需要凭据 |
| 能力包 | `GET /packs`，`GET/POST /instances`，`PATCH /instances/{id}` | 能力包与实例。`GET /packs` 的每个包另带 `default_instruction`（实例驱动的 `call_agent` 模板的默认内容要求，没有时省略，附录 C.7）；`GET /instances` 另带顶层 `agent_ready`（Web 设置了默认 Agent 且其无头运行声明已确认；为假时周期简报到点跳过，主文档 §7.3）。实例参数 `instruction` 不超过 2000 字 | observer 读，delegate 按档位写（3a；改执行 Agent 为 3b） |
| | `POST /instances/{id}/run` | 立即跑一次（`instance.run`，3a）：以当前时刻为到期时刻记 `instance.fired`，与定时到期同一路径，含 Agent 不能后台运行时的跳过；返回 202 与实例 | delegate 按档位 |
| | `GET /rules`，`PATCH /rules/{id}/mode` | 规则与模式；每条带 `class`（`reminder` 提醒类或 `action` 动作类，由加载器推定，主文档 §14.8）；实例驱动的时钟规则（`schedule` 含 `{instance.…}`）不是可见规则，不列出，对它改模式与规则不存在同为 404 `not_found`（主文档 §7.3）；切换模式是 3a | Web 登录；切换 delegate 按档位 |
| 设置 | `/settings/policy`、`/settings/tokens`、`/settings/ext-keys`、`/settings/allowlists`、`/settings/executors`、`/settings/commands`、`/settings/declarations`、`/settings/model-endpoints`、`/settings/access-limits`、`/settings/projects/{id}/allowed-executors`、`/settings/purge`、`POST /settings/proactive/preview`、`GET /settings/notify/overview` | 策略（含人格、打扰上限与静音、主动提醒）、两个模型端点、凭据、白名单、名单、命令登记、声明确认、物理删除（`POST /settings/purge` 的 `object` 可以是事项、材料或来源事件 id；删除事项时可带 `"with_source_events": true` 连同它的来源事件一并删除，对其他对象带它是 400；去重键留作墓碑，主文档 §6.15）。策略各段按 B.1"不走命令的设置路由"定档：3a 段 delegate 按档位可读可写；3b 段与凭据、名单、命令登记、声明确认、物理删除只有 Web 登录 | 只有 Web 登录；3a 段 delegate 按档位 |
| | `/settings/judgment` | 不确定带与影子模式（3a） | Web 登录；delegate 按档位 |
| | `POST /settings/model-endpoints/models`、`POST /settings/model-endpoints/test` | "设置 › 模型"的获取模型与测试连接：不改策略；只有测试的正是已保存的端点、测试通过且该端点有未恢复的失败时，记一条探测通过的 `run.finished`（见下文"测试连接"），其余情况不写 Event；同一类端点同一时刻只跑一个，其余返回 409（主文档 §4.1、§15.2、§15.5） | 只有 Web 登录 |
| | `/settings/sources/{id}/outbound`、`/settings/projects/{id}/capture-outbound` | 来源与对话捕获送模型端点的范围（3b）；只管内核调模型，不管钥匙读取（主文档 §14.14）。Web 在「设置 › 模型」里编辑 | 只有 Web 登录 |
| | `PUT /settings/tokens/{id}/scope`、`PUT /settings/ext-keys/{id}/scope` | 改钥匙给谁用与逐来源档位 `{"audience": "internal"\|"external", "source_scopes": {"<来源 id>": "full"\|"excerpt"\|"meta"}}`（`credential.scope.set`，3b，记 `credential.scope_changed`，载荷 `{"before", "after"}`，各含 `audience` 与 `source_scopes`），立即生效。`internal` 只给 observer、worker、delegate token，且 `source_scopes` 必须为空；来源须已登记，不能写 `capture:*`；不在表里即不给；已吊销为 409；组不对（ext-keys 改 token）为 404。`POST /settings/tokens`、`POST /settings/ext-keys` 可带同样两个字段，缺省 `external`、空表；`GET` 两组列表每条带 `audience` 与 `source_scopes` | 只有 Web 登录 |
| | `GET/PUT /settings/note-vaults` | 笔记库登记：`{"vaults": [{"name", "open_url"}]}`，整体替换；经 `policy.Update`，记 `policy.changed`。名字重复、模板不以协议开头或用了被拒绝的协议返回 400 | 只有 Web 登录 |
| | `PUT /settings/sources/{id}/content-retention` | 来源的原文保留天数：`{"days": 90}`，`null` 为不清理（主文档 §6.15；3a） | Web 登录；delegate 按档位 |
| | `GET /settings/retention` | 三项保留期与最近一次清理：`{"run_days", "ingest_days", "judgment_days", "defaults": {同前三项}, "last_sweep": null或{"at", "run_dirs", "run_dir_bytes", "ingest_bodies", "judgments", "skipped_in_flight": [运行 id], "error"?}}`；0 为不清理；ext 与 webhook 凭据不可读（主文档 §6.15） | observer |
| | `PUT /settings/retention` | 改保留期：`{"run_days"?, "ingest_days"?, "judgment_days"?}`，缺的不改，至少一项；运行目录与请求体 0 或 1–3650，判断记录 0 或 30–3650，越界是 `invalid_request` 并写明范围；返回改后的三项；命令 `retention.set`，写 `retention.changed`（3a） | Web 登录；delegate 按档位 |
| | 已有路由 /settings/model-endpoints 的新增字段 | `generate_endpoint.max_material_tokens`：写作的资料预算（估算 token，约 1.5 个字符记 1 个），缺省 16000（= 原 24000 字 ÷ 1.5），可设 2000–500000，0、负数、范围外 400 并写明范围、不截到边界；旧键 `max_material_chars` 只读，请求里带它 400（主文档 §8.4） | 只有 Web 登录 |
| | `PUT /settings/sources/{id}/trusted-senders` | 只替换这个来源的转发白名单 `{"senders": [...]}`，经 `policy.Update`，记 `policy.changed` 与 `allowlist.changed`；来源声明为需确认时只接受空名单（清掉以前填的、不生效的条目），非空为 400（`sender_unverifiable`）；名单为空即策略里没有这个键，Web 再次按"从未设置"预填本人（3a） | Web 登录；delegate 按档位 |
| | `GET /settings/sources/{id}/self-sender` | 声明了 `sender.self: identity` 的来源：运行一次来源程序的 `identity` 模式（登记命令的第一个词加 `identity`，超时 10 秒），返回 `{"sender"}` 供预填；不产生来源事件、不写 Event；其他来源 404，程序失败或没给出身份时 409 `conflict` | 只有 Web 登录 |
| | `PUT /settings/channels/{name}/answerers` | 只替换这个渠道的回答人白名单 `{"answerers": [...]}`，记 `policy.changed` 与 `allowlist.changed`（3b）；名单为空时该渠道只送提醒，确认只走 Web | 只有 Web 登录 |
| | `DELETE /settings/channels/{name}` | 删除一个自写程序渠道：同一事务里吊销该渠道 `channel.<名>` 的全部未吊销渠道密钥，并从策略的 `channels` 与 `channel_answerers` 里删掉这个名字；要么三件都成、要么都不动，重复删除返回 `{"version": 当前版本, "revoked": 0}`，记 `policy.changed`（改到回答人时另记 `allowlist.changed`）。指向它的打扰路由不动。内置渠道的名字前缀 `ch-` 为 400 | 只有 Web 登录 |
| | `POST /settings/channels/{name}/identify`，`GET /settings/channels/{name}/identify` | 开始识别：返回一次性口令 `{code, expires_at}`（10 分钟，同一渠道同时一个，只在内存里，重启作废）；查看：`{state: waiting \| got \| expired, sender?, display?}` | 只有 Web 登录 |
| 连接 | `GET /connections?direction=` | 列出连接（渠道），按创建顺序：`{"connections": [{"id", "direction", "type", "name", "config", "secrets": {"<字段>": true/false}, "enabled", "state": ok/problem/disabled, "problem"?, "detail"?, "last_ok_at"?, "last_error_at"?, "created_at", "updated_at"}]}`。凭据只回是否已设置且可用（主文档 §14.2）；必填凭据不可用时 `problem` 为 `credentials_missing`。token 读到的每条不含 `config`、`secrets`、`detail`（主文档 §7.6） | observer 读裁剪后的字段；Web 登录读全部；ext 与 webhook 密钥不可 |
| | `GET /connections/types?direction=` | 内置连接类型的接入声明（选类型与渲染表单用）：`{"types": [{"type", "direction", "title", "tagline", "default", "via", "capabilities", "fields", "guide"?}]}`；文字都是 `{zh, en}` | 只有 Web 登录 |
| | `POST /connections` | 新建（`connection.create`）：`{"type", "name"?, "config"?, "secrets"?, "enabled"?}`；`name` 缺省取声明的默认名，重名 409；配置按声明的字段表校验，必填缺失或字段不符 400。返回 201 与连接 | 只有 Web 登录 |
| | `PUT /connections/{id}` | 改配置与凭据（`connection.configure`）：`{"config"?, "secrets"?}`；`config` 给出时整体替换非凭据配置；`secrets` 里出现的字段非空即替换、空串即清除，没出现的保持 | 只有 Web 登录 |
| | `POST /connections/{id}/rename`，`POST /connections/{id}/enable`，`POST /connections/{id}/disable` | 改名 `{"name"}`（`connection.rename`）、启用（`connection.enable`）、停用（`connection.disable`）；状态未变不记事件。停用后，路由到它的投递直接进 Web 未读 | 只有 Web 登录 |
| | `DELETE /connections/{id}` | 删除（`connection.remove`）：只对已停用的连接，否则 409；配置、凭据与现状一起删 | 只有 Web 登录 |
| | `POST /connections/test` | 测表单里未保存的草稿 `{"type", "id"?, "config"?, "secrets"?, "send"?}`：插件自检，`send` 为真时再发一条内容固定的测试消息；带 `id` 时没给出的凭据用已保存的。不改账本。返回 `{"ok", "problem"?, "detail"?}`，`problem` 是问题代码（主文档 §7.6），`detail` 已脱敏 | 只有 Web 登录 |
| | `POST /connections/{id}/test` | 对已保存的连接发一条测试消息，结果写进连接现状；返回同上 | 只有 Web 登录 |
| | 已有路由 GET /settings/ext-keys 的新增字段 | 每把密钥带 `last_seen_at`：同一权限范围（`channel.<名>`、`export` 等）的密钥最近一次经认证调用内核的时间，从未调用过时省略（与 `apa doctor` 的 `last_seen_at` 同源） | 只有 Web 登录 |
| | `PATCH /settings/tokens/{id}` | 认领 token 所属的 harness `{"harness"}`（非空、不含空白，同无头运行声明的 `harness`）：只能从空设一次，已有值或已吊销为 409（`credential.assign`，记 `credential.assigned`，载荷 `{"harness"}`）；只对 worker、delegate。`POST /settings/tokens` 可带同一字段，在签发时给定；其他种类带它为 400。`GET /settings/tokens` 每条带 `harness`（空为未认领）与 `seen_harness`（该凭据最近一次事件里自报的 harness，没有时省略，只作认领推荐，不参与判定） | 只有 Web 登录 |
| | `PUT /settings/agents/{agent}/tier` | 设这个 Agent 的档位 `{"tier": "organize" \| "decide" \| "rules", "projects"?: [...], "version"?}`（3b）：`organize` 只整理，`decide` 另可拍板（`projects` 非空时只对这些项目），`rules` 另可改日常规矩；`version` 不符为 409。记 `policy.changed`（主文档 §14.3） | 只有 Web 登录 |
| 认证 | `POST /auth/login`、`/logout` | Web 会话。连续失败的限速与锁定见主文档 §14.13，锁定期间返回 429 | |
| | `POST /auth/password` | 改口令 `{current, new}`；新口令少于 12 个字符返回 400；成功后其他会话全部失效 | 只有 Web 登录 |
| | `POST /auth/reauth` | 再输一次口令，请求体与登录相同：成功返回 204，当前会话在 10 分钟内可做 3b 动作；口令不对为 401，失败计入登录限速与锁定（主文档 §14.13） | 只有 Web 登录 |
| | `GET /auth/sessions`，`DELETE /auth/sessions/{id}`，`DELETE /auth/sessions` | 在线会话与吊销；吊销是 3b，会话须刚输过口令 | 只有 Web 登录 |
| 动作链接 | `GET /act/{item}?do=done\|progress\|plan` | 不属于 API：Web 的确认页地址，不带版本前缀，不含凭据；未登录时先登录再回到确认页（主文档 §4.1、§14.13） | Web 登录 |

事项字段 `deadline`：`POST /items` 与 `PATCH /items/{id}` 可写，值为 RFC 3339 时间；`PATCH` 传 `null` 清除；`GET` 在已设置时返回。行为见主文档 §6.8。

事项字段 `planned_for`：`POST /items` 与 `PATCH /items/{id}` 可写，值为 `YYYY-MM-DD`（本机时区的日期），`PATCH` 传 `null` 清除；`GET` 在已设置时返回。它不产生定时；不产生提醒，除非本人开启了主动提醒的到点提醒（主文档 §6.3、§7.4，**已变更（2026-11-23）**）。清除计划日期时时段一起清除。

事项字段 `planned_slot`：`POST /items`、`PATCH /items/{id}` 与导入的 `item` 可写，值为 `HH:MM - HH:MM`（本机时区的墙上时间，开始 00:00–23:59，结束 00:01–24:00 且晚于开始），`PATCH` 传 `null` 或空串清除；`GET` 在已设置时返回。写了时段而请求与事项上都没有计划日期、格式不对、跨午夜或结束不晚于开始时返回 400 `invalid_request`。不产生定时；开启了到点提醒时提醒开始与截止（主文档 §6.3、§7.4，**已变更（2026-11-23）**），否则不产生提醒。事项事件的快照里随之带上这个字段。

事项字段 `daily_at`（**新增（2026-11-23）**）：`POST /items` 与 `PATCH /items/{id}` 可写（CLI 为 `apa item add/update --daily-at <HH:MM>`，空串清除），值为 `HH:MM`（本机时区，00:00–23:59，两位补零），`PATCH` 传 `null` 或空串清除；`GET` 在已设置时返回。表示每天这个钟点提醒一次，只在开启了到点提醒时产生提醒（主文档 §7.4）。与 `planned_for` 互斥：同一请求或事项上两者同时有值返回 400 `invalid_request`；写入计划日期而没写 `daily_at` 时内核在同一条命令里清掉它。事项事件的快照里随之带上这个字段。

`GET /items?from=YYYY-MM-DD&to=YYYY-MM-DD`：按日期范围读，含首尾，`to` 不早于 `from`，跨度至多 62 天，两个参数须同时给，否则 400。收录 `planned_for` 落在范围内、或 `deadline` 所在的本机日落在范围内的事项，与其他筛选取交集；不按阶段过滤。带范围时响应另有 `today`（本机时区的今天）与 `utc_offset`（本机时区此刻的偏移，如 `+08:00`）。

`GET /items?progress_from=YYYY-MM-DD&progress_to=YYYY-MM-DD`：按进展日读，含首尾，两个参数须同时给，`progress_to` 不早于 `progress_from`，跨度至多 62 天，否则 400。只收至少有一条进展的进展日落在范围内的事项（与其他筛选取交集，不按阶段过滤），响应另有 `progress`（事项 id → 落在范围内的进展条目，形状同 `GET /items/{id}` 的 `progress[]`，按记录先后）。进展日是条目的 `on`；没有 `on` 的取 `at` 所在的本机日。Web「已完成」页按它数一周的进展（界面设计 §9.13）。

`GET /items?phase=done`：响应另有 `done_at`（事项 id → 进入已完成的时刻，UTC）与 `done_unknown`（完成日期不详的事项 id 数组，无则为空数组）：取最近一次转入 `done` 的事件时刻；新建时就是已完成的事项（工作台迁移写入，带 `ext.workbench`）没有转入那一刻，取 `ext.workbench.completed` 那天的本机 00:00，没记完成日期的进 `done_unknown`、不在 `done_at` 里：不拿写入时刻或 `ext.workbench.updated`（最后改动日）顶替，也不猜。非迁移写入而新建即已完成的事项取写入时刻。Web「已完成」页按 `done_at` 所在的本机日分周，`done_unknown` 的事单列一节写"完成日期不详"，不算在任何一周、不计入完成数（界面设计 §9.13）。

`GET /items?phase=proposed`：响应另有 `origins`（建议 id → `{"type"?, "object"?, "ended"?: {"event", "type", "at"}}`）：`type` 是建议第一条来源事件的类型，`object` 是建议所属的上游对象（链接路径），`ended` 是对象已结束的依据事件（判定见主文档 §7.2）；没有来源事件的建议不在其中。只读，不改任何数据；其他阶段不给这个字段。

`GET /items?phase=dropped`：响应另有 `dismissals`（事项 id → `{"batch"?, "reason"?, "at", "actor"}`）：只列最近一次阶段变更是忽略建议（`proposal.dismiss`）的事项，`batch` 是那次批量的批次号，逐条忽略的没有。被合并折叠、放弃等转入 `dropped` 的不在其中。只读；其他阶段不给这个字段。

事项字段 `proposal`：只读，由入站流水线写入：`{recommend, attach_to?, reason, low_confidence, sources[], merges?[], similar?[], not_same?[], split_from?}`（主文档 §6.3、§9.3）。`merges[]` 每条 `{event, source, title, summary?, kind?, project?, by, score?, judgment?, at}`，`by` 取 `merge_key`、`classifier`、`chat`；`similar[]` 每条 `{item, score, judgment?}`，是只提示、没有合并的相似建议；`not_same[]` 是本人拆开过的建议 id；`split_from` 是这条建议从哪条拆出来的。`GET /items?phase=proposed` 可带 `sort=score` 按初筛分数排序。

`POST /items/{id}/progress` 的请求体 `{"text": "…", "on": "YYYY-MM-DD"}`，`on` 可省略、不能晚于今天；成功返回 201 与这条进展，Event 为 `item.progress_added`。进展不改阶段，不推进 `version` 以外的字段。

`GET /items` 返回 `last_rejections`（事项 id → 最近一次未处理的退出码 23 拒绝：`seq`、`at`、`actor`、`action`、`code`、`reason`、`why`；旧模型留下的拒绝另带 `grant`），`GET /items/{id}` 返回 `last_rejection`；对未终结的事项给出：`needs_input`、`proposed` 在其后有回答或阶段变化前给出，`open`、`waiting` 在其后事项有任何变更前给出（被拒的写入不转阶段，主文档 §14.5）。被拒的写入从不生效。事项的 `ask` 可带 `code`、`params`（内核生成的问题的类别码与参数，主文档 §6.3）；agent 凭据写它们返回用法错误。`GET /audit/events` 可加 `grant=<授权单 id>` 查旧模型授权单的签发、使用与撤销记录（只读历史，主文档 §14.5）。

`GET /audit/events?actor&object&type&grant&imported&limit` 返回 `{"events": [...]}`，缺省按 Event 序号升序，`after=<序号>` 取其后的一页，`limit` 缺省 100、上限 500。加 `order=desc` 改为最新的在前，`before=<序号>` 取序号小于它的一页（缺省从最新开始），游标是 Event 序号，与筛选组合时翻页稳定，不重复、不漏。`order` 取 `asc` 或 `desc`，其他值返回 400；`after` 只配升序、`before` 只配 `order=desc`，混用返回 400。Web 的操作记录页用 `order=desc`，CLI 与其他调用方沿用升序的 `after`，不受影响。

事项字段 `created_at`、`updated_at`：`GET /items` 与 `GET /items/{id}` 返回，值为 RFC 3339 时间，只读。`created_at` 是该事项首条事项事件的时间（委派子事项的首条是 `delegation.created`，其余是 `item.created`）；`updated_at` 是最后一条事项事件的时间，与 `version` 同步推进（含关系变更；`item.deadline_passed` 与 `command.rejected` 不算）。

来源的显示名 `name`（主文档 §9.1）：`POST /sources` 可带，去掉首尾空白后不超过 40 个字符、不含控制字符，与实例内其他来源不重名（不区分大小写），重名返回 409 `conflict`；不带时取来源程序接入声明的名字（中文）存下，响应里给出；程序没有声明又不带时返回 400 `invalid_request`，`next` 说明带名字或在声明里补上；声明出的名字被占用同样返回 409，不自动加序号。`PATCH /sources/{id}` 带 `name` 即改名（`source.rename` 命令，记 `source.renamed`，载荷 `{"name", "previous"}`），空串改回接入声明的名字（没有声明时 400）；名字与其他字段同一请求里给出时分别生效。`GET /sources` 与 `GET /sources/{id}` 总带 `name`，另带发件人结论 `sender: {"trust": "verified" | "unverified", "hint"?: {"zh", "en"}, "self"?: "identity"}`（主文档 §9.1）：有接入声明按声明；没有声明的只用映射的 webhook 来源，所用映射里有按验签核实发件人的为 `verified`；其余为 `unverified`。`GET /sources/declared-name?type=&command=&source_type=&config=`（`config` 是 JSON 对象，非法时 400）按同一套声明返回 `{"name": {"zh", "en"}}`，没有声明时 `{"name": null}`，供 Web 新登记表单按界面语言预填；它只读，不登记。

`POST /sources`、`PATCH /sources/{id}` 的 `credentials_path` 必须落在该来源自己的目录（`<APA_HOME>/sources/<id>/`）内，否则返回 400。内核只记录这个路径，不读内容；每次拉取时经 stdin JSON 的 `credentials_path` 字段原样交给来源进程（主文档 §9.2），不经环境变量传递。

`POST /sources/{id}/webhook` 不带请求体。来源还没有有效密钥时创建，返回 201；已有时轮换，返回 200，旧密钥在 1 小时宽限期后失效；轮换总是作用于当前有效（未吊销、未轮换过）的密钥，已吊销或已轮换过的凭据不能再轮换（内核返回 409）。响应 `{"credential": {...}, "secret": "apa_whk_…", "url": "/ingest/<source_id>", "rotated": false}`，`secret` 只出现这一次，不进幂等结果表，Event 只记凭据 id。校验方案为 HMAC 的来源，明文另写入 `<APA_HOME>/webhook-keys/<source_id>`（主文档 §14.2），没有任何 API 能读回；共享 token 方案只存哈希。

`DELETE /sources/{id}/webhook` 不带请求体，吊销该来源全部有效的接收密钥（含宽限期内的旧密钥）并在同一事务里删除密钥文件，返回 200 `{"revoked": n}`，已无有效密钥时 `n` 为 0。文件删除失败时整个命令回滚、账本不变，返回错误，重试即可。Event 为 `source.webhook_key_revoked`，只记凭据 id。

`GET /sources/{id}/events` 按落盘顺序返回 `{"events": [...], "next_after": 12}`；带 `sort=score` 时按最近一次判断记录的概率从高到低返回（同分保持落盘顺序），没有分数的排在最后；此时 `after`、`next_after` 是已返回的条数（偏移），不是落盘序号；带 `sort=recent` 时从新到旧返回，`after`、`next_after` 同样是偏移；其他 `sort` 值返回 400。每条事件的字段见主文档 §6.10：`id`、`source_id`、`dedupe_key`、`type`、`subject`、`summary`、`detail`、`link`、`occurred_at`、`correlation_keys`、`sender`、`sender_verified`、`disposition`、`item`、`attach_state`、`unjudged_reason`、`rejudges`，以及 v0.2.0 新增的 `metrics`、`merge_key`、`items`、`judgment`、`score`（最近一次判断的概率，没有时省略）。其中 `item` 是挂靠或生成的事项 id，`items` 是一条事件抽出多条候选时的全部事项 id。`disposition` 可选，取值 `pending`、`attached`、`proposed`、`ignored`、`digest`、`alerted`、`unjudged`、`expired`，可逗号分隔多个（来源页的积压用 `unjudged,expired`），其他值返回 400。`unjudged_reason` 取 `model_unavailable`、`models_paused`（本人暂停了模型，主文档 §8.7）、`outbound_unset`、`restored`；v0.1 留下的 `undecided` 与空值原样返回，内核按 `model_unavailable` 对待。`limit` 缺省 100，上限 500；还有下一页时才返回 `next_after`，作为下一次的 `after`。

`GET /sources/{id}/rejects` 按发生顺序返回 `{"rejects": [...], "next_after": 12}`，分页参数与 `events` 相同；带 `sort=recent` 时从新到旧返回，`after`、`next_after` 是偏移（同 `events`），其他 `sort` 值返回 400。每条含 `source_id`、`run_id`、`line`（输出中的行号）、`reason`、`raw`（原始行）、`at`。`raw` 在落账时已截断到 512 字节；含接收密钥明文前缀的行返回 `[redacted]`（与入站记录的请求头同一规则）。来源未登记返回 404。
`capture:<项目>` 是对话捕获的内部来源（主文档 §9.3），不在 `GET /sources` 里，Web 按开启捕获的项目拼出 id。Web 登录读它返回同样的结构，其他凭据返回 404（运行凭据仍是 403）。

`POST /source-events/{id}/propose` 不带请求体，成功返回 200 与更新后的来源事件（`disposition` 为 `proposed`，`item` 为新事项 id），事件不是 `unjudged`、`expired` 或 `digest`（简报"知会"一节的条目）时返回 409，不存在时返回 404。事项阶段为 `proposed`，事项的 `item.created` 与来源事件的 `inbound.disposed` 都记调用者为操作者。

`POST /source-events/{id}/ignore` 不带请求体，成功返回 200 与更新后的来源事件（`disposition` 为 `ignored`），事件不在积压里（不是 `unjudged` 或 `expired`）时返回 409，不存在时返回 404。`POST /source-events/ignore` 的请求体恰好给出 `source` 或 `all: true` 之一，否则返回 400；在一个命令里忽略该来源或全部来源的积压，返回 200 `{"ignored": n}`，没有积压时 `n` 为 0。两者都可带 `Idempotency-Key`，每条被忽略的事件记一条 `inbound.disposed`，操作者是调用者。

`POST /source-events/rules`、`/score`、`/rejudge` 对应主文档 §9.3 的积压放开：请求体可带 `source` 限定来源，`rejudge` 可带 `limit`；都可带 `Idempotency-Key`。它们不受 `unjudged_rejudge_days` 限制，也作用于 `expired` 的事件。响应分别是 `{"attached": n, "disposed": n, "remaining": n}`、`{"scored": n, "skipped": n}`、`{"requeued": n, "deferred": n}`；`deferred` 是因当天进入建议流的数量已到 `backlog_release_per_day` 而留到次日的条数。没有判断端点时 `score` 返回 409。模型已暂停时 `score` 与 `rejudge` 都返回 409，`code` 为 `models_paused`，不碰积压、不调模型；`rules` 不需要模型，照常可用。每条被放回的事件记一条 `inbound.requeued`，操作者是调用者。

处理经过的行：`who.kind` 取 `owner`、`agent`（`name` 是凭据 id）、`rule`（`name` 是"包/规则 id"）、`model`（`role` 为 `judge` 或 `generate`，`name` 是模型名，可空）、`system`、`source`（`name` 是来源 id）。`kind` 取 `arrived`（`args.type`、`sender`、`verified`）、`captured`、`rule`（`args.then`：`ignore`、`judge`、`proposed`、`digest`、`alert`、`extract`）、`judged`（`args.levels[]` 每级 `{backend, model, p?, seg?, answer?, outcome, error?}`，`seg` 为 `below`、`between`、`above`，按记录的不确定带推算；另有 `low`、`high`、`decided_by`、`answer`、`low_confidence?`、`shadow?`；记录过了保留期时只有 `expired`、`answer`、`decided_by`。`error` 是这一级没答上的原因码，`outcome` 为 `uncertain` 且是对话端点时表示答了但资料不足；一条判断记录在内核里仍是一行，Web 按 `levels` 逐级展开成多行，见主文档 §15.1）、`backlog`（`args.reason`）、`expired`、`requeued`、`ignored`、`proposed`、`merged`、`digest`、`alerted`、`attached`（`args.via`：`follow` 或 `proposal`）、`drafted`（`args.outcome`：`ok`、`failed`、`skipped`、`unknown`，`args.code`）、`spot`（`args.verdict`）、`created`、`accepted`、`dismissed`、`phase`（`args.from`、`to`、`waiting_kind?`、`waiting_ref?`）、`edited`（`args.fields[]`、`tags_added?`、`tags_removed?`）、`synced`、`related`、`absorbed`、`attach_to`、`progress`、`deadline_passed`、`decision`（`args.state`）、`follow`（`args.state`、`message?`、`subject?`、`text?`（消息正文的前 60 字）、`count?`）、`observed`、`run`（Agent 后台运行，`args.state`：`launched`、`started`、`finished`；`launched` 另有 `runner`、`launch`，`main_session` 运行再有 `from`（`rule`、`agent`、`declaration`、`default`）与选了唤醒却按无头运行时的 `fallback`，主文档 §13.6）、`generated`（写作模型的一次生成，`args.state`、`template`）、`alert`（系统告警）、`delegated`（`args.assignee`）、`dispatched`、`dispatch_updated`（`args.executor`、`harness`、`state`）、`reported`（`args.phase`）、`rejected`，以及兜底的 `event`（`args.type`）。`mech` 为真的是机械记录，界面可把相邻的同类折成一行。同一时刻的行按落账顺序排列。

`GET /source-events/{id}/outbound-preview` 返回 200 `{"full": {…}, "excerpt": {…}, "meta": {…}}`，三档各是 `{"event_type", "subject", "sender", "occurred_at", "link", "material"}`：前五项是送给每一级的结构化输入（链接只有域名），`material` 是按该范围裁剪的资料正文（聚合事件的消息接在正文后；`full` 另带完整链接，`meta` 为空串）。`none` 与未选择不外发，不在结果里。事件不存在返回 404。

`GET /deliveries/today` 返回 200 `{"pushed", "cap", "suppressed_daily_cap", "suppressed_muted"}`：`pushed` 是当天经渠道推送、计入日上限的决策、提醒与简报条数（不含超限说明），日上限判定用的就是这个数；`cap` 是策略的 `disturb.daily_cap`，`0` 表示不限；后两项是当天被日上限、静音改为只留 Web 未读的条数。自然日按静默时段的时区算，未设置取 UTC。

`GET /rules` 每条规则的 `class` 只在读取时按当前已加载的能力包填写，不存账本；规则已不在加载的能力包里时省略。`runner` 同样只在读取时填写：规则的 `start_background_run` 动作的运行者（`task` 或 `main_session`），不拉起后台运行的规则省略；为 `main_session` 的规则可在策略的 `main_session_launch.by_rule` 覆盖拉起方式。

`GET /source-events/restorable` 与 `POST /source-events/restore` 对应主文档 §9.3 的"补救旧级联误丢的事件"。`from`、`to` 必填，是 RFC 3339 时刻，按来源事件的落盘时间取左闭右开区间，`from` 不早于 `to` 返回 400；`source` 可选，不存在时返回 404。命中条件：处置为 `ignored`，最近一次处置由内核依判断结论写入（不是规则的 `ignore` 出口，也不是调用者的忽略），且没有判断记录。预览返回 200 `{"matched": n, "sources": [{"source", "count"}]}`，不写入。执行返回 200 `{"restored": n}`：每条命中的事件转 `unjudged`、`unjudged_reason` 为 `restored`，各记一条 `inbound.disposed`，操作者是调用者，载荷带原处置 `ignored` 与本次的 `from`、`to`；不调模型、不建事项。同一时间段再次执行时已放回的不再命中，返回 `{"restored": 0}`；可带 `Idempotency-Key`。`restored` 的事件只经 `/rules`、`/score`、`/rejudge` 重判，不参与自动重判，也不转 `expired`。

`POST /proposals/batch` 的请求体四选一：`{"mode": "recommended", "filter": {"source"?, "project"?, "ids"?}}` 按推荐处理范围内带推荐的建议，`filter` 必须带范围（`ids`、`source`、`project` 至少一项），都不带不等于"全部"，返回 400 `invalid_request`（CLI 退出码 2），`ids` 为空数组同样 400，都不处理；`{"mode": "explicit", "actions": [{"id", "do": "accept" \| "attach" \| "dismiss", "to"?, "reason"?}]}`；或 `{"mode": "ended", "filter": {"ids": [...]}}` 忽略其中上游对象可核为已结束的建议（判定见主文档 §7.2），每条的忽略理由为 `upstream <对象> ended: <事件类型> <事件 id> at <时刻>`，拿不准的不处理、结果里记 `conflict`，`ids` 为 1–200 条且不能带 `source`、`project` 或 `actions`；或 `{"mode": "undo", "batch": "pb_…"}` 按批撤销忽略：把这一批里最近一次阶段变更仍是这次忽略的建议改回 `proposed`（每条一条 `item.transition`，`do` 为 `undo`），别处改过的不动，再撤一次结果为空，`batch` 之外不能带别的字段。前三种方式每次另起一个批次号（`pb_` 加 12 位十六进制，带 `Idempotency-Key` 时由键决定），忽略与撤销的每条结果带 `batch`，批里的忽略在 `item.phase_changed` 的载荷里记 `batch`；`POST /items/{id}/dismiss` 也接受可选的 `batch`，格式不符 400。响应 200 `{"results": [{"id", "do", "ok", "code"?, "why"?}]}`，每条各自成败、各记一条 Event；一次至多 200 条。每条按对应的单条命令（`proposal.accept`、`proposal.attach`、`proposal.dismiss`）判定，没有整批的门（主文档 §14.3）。

`POST /items/{id}/attach` 的请求体 `{"to": "<事项 id>", "reason"?: "…"}`：建议转 `dropped`，它的来源事件改挂到目标事项，Event 为 `proposal.attached`；目标已终结返回 409，不存在返回 404（主文档 §7.2）。

`GET /judgments` 返回 `{"judgments": [...], "next_after": "<记录 id>"}`，按判断时刻升序；`after` 是上一页最后一条的 id，有下一页时才返回 `next_after`，`limit` 缺省 100、上限 500，`after` 不是已知记录 id 或 `limit` 越界返回 400。运行凭据列表必须带 `subject`，且只读目标事项范围内的记录：`GET /judgments/{id}` 读范围外的记录返回 404（CLI 退出码 5），列表返回空；ext、webhook 凭据返回 403。记录过了保留期（`judgment_retention_days`，缺省 180 天）被清理后返回 404，引用它的命令 Event 与事项不受影响。`GET /judgments/{id}` 返回主文档 §6.16 的字段。`GET /judgments/stats` 返回 `{"rows": [{"use", "template", "source"?, "backend", "calls", "decided", "uncertain", "errors", "reused"}], "rates": [{"use", "template", "source"?, "escalation", "undecided", "acceptance", "false_drop", "reviewed"}], "histogram": [...]}`；`since` 缺省 30 天。

`GET /health` 返回 `{"healthy": bool, "signals": [...]}`，信号恒为十四项且顺序固定（主文档 §15.2）：`source_failures`、`outcall`、`delivery_backlog`、`chain_trips`、`agentsmd_drift`、`pack_rejections`、`model_unavailable`、`unjudged_backlog`、`rule_fallbacks`、`receipt_failures`、`other_alerts`、`stalled_delegations`、`clock_lag`、`capture_failures`。前十一项的 id 与顺序同 v0.1，后三项是 v0.2.0 追加的，都是 `warning`，不影响 `healthy`。`capture_failures` 另给 `captures: [{class, count, last_at, project?}]`（最近 24 小时，按类别汇总，最近的在前），`detail` 是类别列表；不含对话内容与凭据。每项 `{id, ok, count, severity?, why?, next, detail?}`；`severity` 只在异常时给出，取 `error` 或 `warning`。`unjudged_backlog` 是 `warning`，不影响 `healthy`；它的 `detail` 为 `{"model_unavailable": n, "models_paused": n, "outbound_unset": n, "undecided": n, "restored": n, "expired": n, "rejudge_days": n, "sources": [{"source", "count"}]}`，`undecided` 只统计 v0.1 留下的事件，`expired` 只作参考、不计入 `count`；`rejudge_days` 是运行配置 `unjudged_rejudge_days` 的值，`0` 表示自动重判与到期都关闭（主文档 §9.3），Web 据它说明积压会不会自动判断。`outcall` 的 `detail` 另带 `failures`：`[{"run", "target", "category", "fault"}]`，是仍为目标最近一次的失败的任务型与后台运行（24 小时内），计入 `count`。`receipt_failures` 另带 `receipts`：`[{"object", "command", "exit", "code"?, "credential"}]`，`code` 是拒绝的原因码（升级前的告警从同一对象紧挨着的 `command.rejected` 补上），不带原因原文；`detail` 仍是对象 id 列表。`source_failures` 的 `detail` 每项为 `{"source", "enabled", "consecutive_failures", "kind"?, "needs_input_item"?}`，只列当前连续失败或已停用的来源，最近一次拉取成功即不再出现；`kind` 是最近一次失败的类别（`transient`、`auth_or_config`、`permanent`，同来源统计的 `last_failure_kind`），Web 据 `auth_or_config` 说"要重新登录"；`needs_input_item` 只在升级前留下的认证失败事项仍未结束时给出，当前版本不再建这类事项。`other_alerts` 另带 `notices`：`[{"object", "rule"?, "code"?, "params"?, "item"?, "dropped"?}]`，每条是一条仍算现状的告警，`code` 取动作没能完成的类别码（`task_refused`、`background_refused`、`resume_refused`、`model_unset`、`models_paused`、`outbound_blocked`、`chain_tripped`、`action_rejected`、`action_failed`，主文档 §8.7），`params` 只放内核生成的机制值（`detail`；`action_rejected` 另带拒绝原因码 `code`），`item` 是受影响的事项，`dropped` 为真表示规则建的那一份已转 `dropped`；升级前的告警没有 `code`；`detail` 仍是对象 id 列表。`model_unavailable` 的 `detail` 为 `{"judge": s, "generate": s, "endpoint": s, "paused": b, "screening": [事项 id], "judgements": [{"kind", "object", "endpoint"}]}`，`judgements` 是尚未恢复的模型调用失败，`endpoint` 取 `judge` 或 `generate`，表示这次失败归哪个端点（判断型模板在配了判断端点时归 `judge`，否则与生成型一样归 `generate`）；`judge`、`generate` 各取空、`not_configured`、`failing` 或 `paused`；`endpoint` 是 v0.1 的字段，保留，取两者中较差的一个。本人暂停了模型时 `paused` 为 true，`judge`、`generate` 都是 `paused`，`endpoint` 为空，信号正常（主文档 §8.7）。

`GET /items/{id}/messages` 返回 `{"messages": [...]}`，按落盘顺序。每条字段：`id`、`source`、`sender`、`sender_verified`、`subject`、`summary`、`detail`、`link`、`occurred_at`、`attach_state`、`forward_allowed`（是否进转发白名单）。只读，不推进 `attach_state`。运行身份和 `unattended` 事项的 worker、observer 只拿到 `forward_allowed` 为真的消息；Web 登录和 delegate 拿到全部。事项不存在返回 404。`apa item show` 把这个列表合进输出的 `messages`。

`GET /context?scope=item[&item=<id>]` 在普通字段外返回 `messages`：`[{"item": "...", "cursor": "12", "messages": [...]}]`，消息字段同 §9.4 的转发内容（不含 `attach_state`、`forward_allowed`）。`cursor` 只给负责者，回执时作为 `--ack-cursor` 带回。只有 `scope=item` 接受 `item` 参数，其他作用域带它返回 400。

`GET /runs` 每条运行在失败或超时时另带 `fault`（`{code, params?}`，故障类别码见主文档 §6.11，参数只有 `exit`、`timeout`），失败附注与 stderr 不返回。`GET /runs` 返回 `{"runs": [...]}`，按入队顺序升序，只留最新的 `limit` 条（缺省 100、上限 500）。`state`、`category` 取值各可逗号分隔多个（任一匹配，如 `state=failed,timeout`、`category=task,background`），`target` 精确匹配；取值不在枚举内返回 400。`before` 是上一页最旧一条的 id，只取在它之前入队的运行，与其他筛选和 `limit` 叠加，Web「运行日志」据此「加载更多」；不是已知运行 id 返回 400。

`POST /runs/{id}/credential` 不带请求体，成功返回 200：`{"run": "...", "token": "apa_run_…", "expires_at": "<RFC 3339>"}`。`token` 只出现这一次，不进幂等结果表。运行不存在返回 404，不是未结束的 `launch: wake` 后台运行返回 409，其他凭据返回 403。用 delegate token 换取时，同时为该 token 记一条唤醒租约（§13.6），只供观测，不改变这把 token 的判定（~~租约期间降为运行身份~~ **已取消（2026-11-24，决定 89）**）。

`GET /deliveries/next` 返回的投递：`content.recommend` 是决策的推荐选项，未给推荐时省略；`content.form_url` 与 `content.web_only` 是旧模型的字段，协议里保留，内核恒不填（一次性表单与 web_only 类别已取消，主文档 §14.4）；只能发文字的渠道在决策投递里只发 `web_url`，本人点开后登录作答；Agent 替本人回答问题后，内核另投一条提醒类投递（`class` 为 `reminder`），标题与摘要写明替答的问题与所选选项，`web_url` 指向"Agent 改动"页，不带选项（主文档 §14.3）；`run` 只出现在唤醒类投递上。规则 `alert` 出口的提醒没有事项：`item` 为空，带 `origin`（`{source_event, rule, source}`，触发它的来源事件、规则 id 与来源 id），`content` 取来源事件的标题、摘要与链接，不带选项；`notify_urgent` 规则的紧急建议提醒同样没有事项，投递 id 等于建议事项 id，`origin` 另带 `item`（这条建议），`content` 是建议的标题、摘要、起草理由与指向它的 Web 链接；渠道 ext 须能处理空的 `item`（主文档 §6.9）。

`/settings/policy` 的 `channels` 段是渠道的登记属性：`{"<渠道名>": {"text_only": true}}`。`persona_lang` 段是助手说哪种语言：`"zh"`、`"en"`，空串为跟随系统（默认；旧策略版本没有这个键，读作空串），其他值 400。兼容模式的 `default_harness` 段是规则未指定 harness 时任务型与后台运行用的 harness，值为 harness 名，空串表示未设置（主文档 §13.6）。`main_session_launch` 段是以 Agent 本人身份做事时的拉起方式：`{"by_harness": {"<harness>": "<方式>"}, "by_rule": {"<规则键>": "<方式>"}}`，方式取 `headless` 或 `wake`，没有的键表示没选，其他值 400；取值顺序是规则、Agent、声明初始值、无头（主文档 §13.6）。旧策略版本没有这个段，读作两份空表。`disturb` 段另有 `push`（推送哪些事，主文档 §7.4）：`{"source_failing" | "template_failed" | "run_failed" | "new_proposals": {"on", "route"?, "min"?}}`，缺的类别取默认（前三类开，`new_proposals` 关、`min` 10），`route` 为空跟决策类路由，未知类别或负的 `min` 返回 400。`/settings/merge`（3a，主文档 §9.3）有两段：`merge_judge` 是执行合并的 Agent，`same`（缺省，与生成事项和建议相同）或 `off`，其他值 400；`merge` 是合并任务本身：`{"exclude_sources": [来源 id], "skip_existing": 布尔, "batch_size": 0 或 2–100, "wait_minutes": 0 或 5–1440, "budget_tokens"?: 2000–500000, "prompt": "提示词正文，空串用内置", "prompt_at"?: "只读，提示词最后一次变化的时刻"}`，`batch_size`、`wait_minutes` 为 0 取默认（100 条、30 分钟）；`budget_tokens` 是一批资料的上限（按约 1.5 个字符记 1 个 token 估算），不带这个键取默认 16000，带了就必须在 2000–500000 之内，0、负数与范围外都 400、不生效；`similarity`? 是规则检索已有事项的相似度档位：`loose`（0.25）、`strict`（0.40），不带或空串为默认档（0.30），其他值 400；`prompt` 缺 `## 任务`、`## 四种结论`、`## 判定依据`、`## 不许做的事` 任一段、某段为空、少了 `attach`/`merge`/`duplicate`/`keep` 任一词、或超过 8000 字时 400，不生效。`source.failed` 的 `kind` 另有 `streak`：同一来源连续临时失败到提醒线时记一条，不建事项、不改游标；提醒线是连续 3 次，这次失败的机会周期（来源失败时在 stdout 末行声明的 `{"chance_period": "<时长>"}`，没声明取拉取周期）超过 1 小时（重试退避的封顶）时是 1 次，此时载荷另带 `chance_period`（主文档 §7.4、§9.2，**已变更（2026-11-23）**）。`disturb` 段另有 `proactive`（主动提醒，主文档 §7.4，**新增（2026-11-23）**）：`{"on": 布尔, "since"?: "只读，最近一次从关到开的时刻", "work_start": "HH:MM", "work_end": "HH:MM", "workdays": [1–7，1 为周一], "shift": 布尔, "start": 布尔, "end": 布尔, "lead_min": 0–1440, "dedup_min": 0–1440, "max_items": ≥0, "max_batches": ≥0, "task_push"?: {"brief" | "review" | "routine": 布尔}, "instance_push"?: {"<实例 id>": 布尔}}`。没有这一段取默认：`on` 关、09:00–18:00、周一到周五、顺延开、开始与截止都开、提前量 0、去重 60 分钟、每天 20 件与 6 条、简报与复盘注入主会话、例行任务不注入。`work_start` 不早于 `work_end`、工作日为空或越界、数值越界、`task_push` 里未知的任务类型都是 400；`since` 由服务端在 `on` 从关到开时记下，请求体里带的不认，关掉时清空。`POST /settings/proactive/preview`（只给 Web 登录，只读）：请求体是 `proactive` 的形状（未保存的草稿），按当前数据假设现在开启，返回 `{"total", "batches", "next_at"?, "next_count", "route", "blocked"?, "entries": [{"item", "title", "kind": "start" | "end", "why", "at"}]}`，`entries` 至多 20 条；`route` 是送往的路由（主会话的唤醒路由，或 `web`），`blocked` 是主会话不能唤醒的原因码：`main_not_set`、`main_unavailable`、`main_cannot_wake`；设置不合法为 400。`disturb` 段另有 `delivery`（投递矩阵，主文档 §7.4，**新增（2026-11-23）**）：`{"default_channel": "" | "channel:<连接 id>", "rows": {"<行键>": {"agent": "off" | "main" | "next_chat", "user": "off" | "default" | "channel:<连接 id>", "else"?: "next_chat" | "default" | "channel:<连接 id>", "by_priority"?: {"low" | "normal" | "high" | "urgent": <同形状，不再嵌套>}}}}`；行键是 `decision`、`agent_answered`、`scheduled`、`overdue`、`followup`、`stale`、`rule_reminder`、`task_result:<brief | review | routine | other>`、`task_failed:<同上>`、`instance:<实例 id>`、`instance_failed:<实例 id>`、`source_failing`、`run_failed`、`health`、`new_proposals`、`source_alert`，缺的行取出厂值；`else` 只在 `agent` 为 `main` 时可用；未知行键、取值或优先级为 400。GET 总是返回生效的矩阵：没保存过时是按旧键换算的值，另带只读的 `delivery_saved`（布尔，PUT 回传时忽略）。没保存过时 PUT 回传的 `delivery` 与换算值完全相同就当作没改矩阵；矩阵写入之后 `routes`、`push.*.route`、`proactive.task_push` 与 `proactive.instance_push` 冻结，PUT 改了它们为 400（`invalid_request`）、什么都不存；PUT 不带 `delivery` 时沿用已存的矩阵。`GET /settings/notify/overview`（只给 Web 登录，只读，**新增（2026-11-23）**）是设置页只读总览与各页提醒单选旁预览的数据：`{"since", "rows": [{"key", "produced", "blocked"?, "rules"?, "events", "pushed"}], "now": {"decisions_pending", "deadlines", "overdue", "waiting", "stale_now", "stale_from"?, "proposals_new", "rule_observed": {"<规则键>": n}}, "source_alert": {"streak_failures", "low_frequency_over_sec", "sources": [{"id", "name", "schedule", "alert_after"}]}}`。`rows` 是投递矩阵的固定行加每个实例的 `instance:`、`instance_failed:` 两行；`produced` 为假表示现在没有东西会出这一行（规则设的提醒、来源即时提醒按已加载能力包里有没有带 `create_timer` 动作、`then: alert` 或 `notify_urgent` 的规则，`rules` 列出这些规则键；内置类型的类型行恒为假，它们的产出归实例行）；`blocked` 只在实例行：`instance_disabled`、`rule_off`、`rule_observe`；`events` 是近 7 天不同的提醒条数，`pushed` 是其中真推出去的投递条数（两层都推算 2，只留 Web 未读与被压下的不算，合并摘要不计）。`disturb` 段在 v0.2.0 新增三个键，旧键不变：`daily_cap`（整数，默认 20，`0` 不限）、`mutes`（`[{"scope": "source" \| "project" \| "item" \| "all", "id", "until"?}]`；`all` 是"暂停所有提醒"，`id` 必须为空，生效期间健康类以外的提醒都只留 Web 未读，健康类（来源连续失败、Agent 运行失败等）照常送达，**新增（2026-11-23）**）、~~`followup_defer`（布尔，默认 `false`）；`threshold_judge` 保留，含义改为"是否开启投递推迟判定"（主文档 §7.4）~~ **已取消（2026-11-23）**：两道推迟判断删除，推迟多久不可预期、推迟了也不告诉本人，少打扰由安静时段、每天上限、合并与暂停承担；`followup_defer`、`threshold_judge` 不再出现在 GET 里，PUT 带着它们（旧客户端整段回传）照常接受并忽略，旧策略版本里的值读入后忽略。

`/settings/model-endpoints`：`GET` 返回、`PUT` 接受下面的形状，两个端点都可为 `null`；`api_key` 只写不读，`GET` 以 `api_key_set` 表示是否已设置，以 `api_key_hint` 给出已存密钥的末 4 个字符（密钥短于 12 个字符时为空串）；`api_key_hint` 只读，照 `GET` 的形状回传时 `PUT` 忽略它。字段语义见主文档 §6.13、§8.8。

```json
{"judge_endpoint": {"url": "…", "api_key": "…", "protocol": "system-one", "model": "…", "locality": "external", "timeout_ms": 10000, "max_batch": 32, "max_material_chars": 8000},
 "generate_endpoint": {"url": "…", "api_key": "…", "protocol": "chat-completions", "model": "…", "locality": "external", "max_material_tokens": 16000},
 "models_paused": false}
```

`models_paused` 为 true 时两个端点与密钥原样保留，内核按两者都未配置运行，"测试连接"与"获取模型"照常可用；两个端点都为 `null` 时保存为 false。修改是一次策略变更，记 `policy.changed`（主文档 §8.7）。已存的策略版本没有这个键，读作 false。

`judge_endpoint.protocol` 取 `system-one`（原生判断接口，`url` 是接口根，内核追加 `/systemone`）、`probability-judgment/1`（自建分类服务，`url` 是完整路径）或 `chat-logprobs`（Web 不提供）；省略时按 `probability-judgment/1`。`model` 在 `system-one` 与 `chat-logprobs` 下必填，缺失返回 400。Web 新配判断端点时默认 `system-one`，地址与模型名的预填值见附录 D.7，`timeout_ms` 预填 30000；`PUT` 不替调用方填默认协议。

`generate_endpoint.protocol` 取 `chat-completions`（对话补全接口）、`messages`（消息接口）或 `responses`（响应接口），其他值 400；省略或为空串按 `chat-completions`，`GET` 总是给出实际取值。三种协议的请求形态、结构化输出方式与解析见主文档 §8.1，带厂商名的版本头与经网关时的选法见附录 D.7。已存的策略版本没有这个键时读作 `chat-completions`，不改写。

`POST /settings/model-endpoints/models` 取模型列表，请求 `{"endpoint": "generate" | "judge", "url", "api_key"?, "protocol"?}`：生成端点按 `protocol`（同 `generate_endpoint.protocol`）放鉴权头，`GET <url>/models` 读 `data[].id`（`messages` 另带 `?limit=1000`），去重排序，至多 500 个。`endpoint` 为 `judge` 时只接受 `protocol: system-one`（`GET <接口根>/models`，读 `models[].name`；`url` 末尾带 `/systemone` 的先去掉）与 `chat-logprobs`，`probability-judgment/1` 没有模型列表，返回 400。探测本身的成败是数据，响应恒为 200：成功 `{"ok": true, "models": [...], "truncated": false, "elapsed_ms": 120}`，失败 `{"ok": false, "reason", "status"?, "elapsed_ms"}`，`reason` 取 `unauthorized`、`not_found`、`http_status`（带 `status`）、`timeout`、`bad_response`、`unreachable`、`region_unavailable`（HTTP 451，或 403 且响应体提到地区、国家）。

`POST /settings/model-endpoints/test` 测试连接（不改策略），请求 `{"endpoint": "judge" | "generate", "config": {…}}`，`config` 与 `PUT` 里对应端点的形状相同，按保存时的同一套规则校验（不合规返回 400）；生成端点须有 `url` 与 `model`。用 `apa doctor` 的内置示例探测（主文档 §15.5）：判断端点用示例题，生成端点用起草建议模板（`data.sample` 为 `draft`，模板不可用时为 `builtin`），示例内容是固定文本，不取账本内容。响应与 `GET /doctor` 的明细同形：`{"subject", "status", "code", "why", "next"?, "data"}`；`code` 取 `ok`、`unauthorized`、`region_unavailable`（服务在当前网络位置不可用，`next` 提示改用可访问的网关地址或代理）、`protocol_error`、`self_reported`、`invalid_request`、`no_answer`、`timeout`、`http_status`、`schema_mismatch`（`status` 为 `warning`，内置小样例没守结构约束）、`draft_schema`（`status` 为 `error`，起草建议模板的输出约束没守；`data.shape` 是返回内容的形态：`empty`、`markdown_fence`、`truncated`、`malformed_json`、`trailing_data`、`text_around`、`plain_text`、`no_tool_call`（`messages` 的回复里没有调用输出工具），内容本身不返回）、`bad_response`（响应体不是所选协议的形状）、`unreachable`；`data` 含 `elapsed_ms`、`locality`，生成端点另有 `protocol` 与 `structured_output`（`response_format`、`text.format` 或 `tool_use`），判断端点另有 `protocol`、`probability_source` 与端点自报的 `model`。测试通过、`config` 与已保存的该端点相同（地址、模型、协议、密钥），且该端点有未恢复的失败时，内核清除该端点的故障标记并记一条 `run.finished`：对象 `models:judge` 或 `models:generate`，载荷 `{"category": "judge" | "generate", "state": "succeeded", "probe": true}`，操作者是发起测试的本人；健康视图把它当作该端点恢复的证据（主文档 §15.2）。没有要清除的失败、或测的是未保存的配置时不写 Event。

**自检结果**（`GET /doctor` 与 `apa doctor --json`）：`{"ok", "checks": [{"name", "status", "code", "why", "next", "data"?, "details"?: [{"subject", "status", "code", "why", "next"?, "data"?}]}]}`。`status` 取 `ok`、`warning`、`error`；`why`、`next` 是机制性英文，供 CLI 与 agent 阅读；`code` 是稳定的机器码，取值只增不改，清单在 `kernel/doctor/codes.go`，Web 按 `code` 与 `data` 显示本地化的一句话。一个对象有多处问题时，`code` 是第一处，`data.problems` 按顺序列出全部问题的机器码。各项检查的 `code` 与 `data`：

| 检查 | 检查一级的 `code` | 对象一级的 `code` | `data` |
|---|---|---|---|
| `data_dir` | `data_dir_ok`、`data_dir_no_backup`、`data_dir_missing`、`data_dir_permissions`、`ledger_missing`、`ledger_unreadable`、`ledger_damaged`、`ledger_too_new`、`ledger_migrate` | — | `schema_binary`、`schema_ledger`、`last_backup_at` |
| `config` | `config_ok`、`config_invalid`、`config_exposed`、`host_file_invalid` | — | `listen` |
| `tier` | `tier_paused`、`tier_kernel_only`、`tier_no_host`、`tier_full`；账本事实读不出时 `facts_unreadable` | — | `tier`、`model_endpoint`、`host`、`models_paused` |
| `model_endpoint` | `models_paused`、`models_none`、`models_ok`、`models_failing` | 同 `POST /settings/model-endpoints/test` 的 `code`，另有 `not_configured`、`no_client` | 同测试连接 |
| `sources` | `sources_none`、`sources_ok`、`sources_attention`、`sources_restored_missing` | `source_ok`（`data.last_run` 为 `ok`、`never`、`unreadable`）、`source_command_missing`、`source_outbound_unset`、`source_outbound_full`、`source_last_failed`、`source_tool_timeout`、`source_tool_version`、`source_tool_missing`、`source_tool_config`、`source_tool_failed`、`source_creds_missing`、`source_webhook_key_missing` | 检查一级 `count`；对象一级 `enabled`、`type`、`command`、`outbound`、`last_run`、`consecutive_failures`、`last_success_at`、`last_failure_at`、`tool`、`problems` |
| `packs` | `packs_no_loader`、`packs_ok`、`packs_rejected` | `pack_rejected` | 检查一级 `loaded`、`rejected`；对象一级 `source`、`in_effect` |
| `host_plugin` | `host_none`、`host_ok`、`host_attention` | `host_ready`、`host_ready_no_selfcheck`、`host_headless_missing`、`host_selfcheck_failed`、`host_hook_missing` | `last_call_at`、`plugin_version`、`missing_hooks`、`problems` |
| `overlaps` | `overlaps_skipped`、`overlaps_none`、`overlaps_found` | `overlap` | `harness`、`kind`、`name` |
| `channels` | `channels_none`、`channels_ok`、`channels_backlog` | `channel_ok`、`channel_backlog`、`channel_orphan`、`export_ok` | `kind`、`backlog`、`last_seen_at` |
| `secrets` | `secrets_none`、`secrets_ok`、`secrets_key_missing`、`secrets_key_mode`、`secrets_unreadable` | — | `rows`、`unreadable`、`other_key`、`key_file`、`mode` |
| `connections` | `connections_none`、`connections_ok`、`connections_problem` | `connection_ok`、`connection_problem`、`connection_disabled` | 检查一级 `count`；对象一级 `direction`、`type`、`name`、`enabled`、`problem`、`last_ok_at`、`missing_credentials` |
| `authz` | `authz_switched`、`authz_not_switched`（`error`：策略里仍有旧模型的段，或还没切换且仍有有效授权单，主文档 §16.5） | — | `switched`、`legacy_policy`、`active_grants` |

`apa doctor` 在数据目录不可读或数据结构与二进制不符时只做前两项，其余各项为 `warning`，`code` 为 `skipped`，`data.reason` 取 `ledger_unreadable` 或 `ledger_schema_differs`。升级预检（`--upgrade-check`）的专用项只在 CLI 输出，不带 `code`。

两条路由的密钥规则相同：`api_key` 显式给出时照用，`""` 表示不带；省略或为占位 `***` 时沿用该端点已存的密钥，但只在请求的 URL 与已存 URL 同源（scheme、主机、端口相同）时沿用，否则返回 400，要求重新输入密钥。

v0.1 的请求形状 `{"model_endpoint": {"url", "model", "api_key", "screening_timeout_ms"}}` 在本版仍被 `PUT` 接受：等同只设置 `generate_endpoint`（`locality` 记为 `external`），`screening_timeout_ms` 随初筛取消而被忽略，判断端点不变；响应一律是新形状。下一个次版本起不再接受旧形状。

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

**工作台能力迭代的路由细节**：

`GET /source-events/{id}[?part=<部分>]`：响应 200：`{"event": {…来源事件字段…}, "content": {"state", "bytes", "parts": [{"part", "media_type", "bytes", "sha256"}], "missing"?}, "body"?, "original"?: {"media_type", "text"}, "headers"?, "attachments"?, "messages"?, "scope": "full" | "excerpt" | "meta", "trimmed"?: {"total_chars", "returned_chars"}}`。Web 登录与内部钥匙的 `scope` 恒为 `full`。外部钥匙的 `scope` 取钥匙对该来源的档位（`source_scopes`）：`excerpt` 只回摘要与正文前 500 个字符，不回 `original`、`headers` 与 `messages`；`meta` 只回 `event` 里的元信息与 `content` 清单；不在表里返回 403（`permission_denied`，`reason: out_of_scope`，退出码 4）。挂在事项上的挂靠消息同样按档位读；运行凭据只能读挂在本次范围内事项上、且过了转发白名单的挂靠消息。带 `part` 时只回那一部分。对话捕获的内部来源只对 Web 登录开放。

**外部钥匙读到的事项与材料**（主文档 §14.14）：一条事项的来源是 `origin.source` 加上挂在它上面的全部来源事件的来源（只算已登记的来源与对话捕获），材料的来源是与它有关系边的全部事项的来源；取最严（不给 < `meta` < `excerpt` < `full`），不出自任何来源的按全文。`excerpt`：事项 `body` 与每条进展正文留前 500 个字符；`meta`：去掉 `body`、`summary`、建议的 `reason` 与合并留痕的摘要和理由，进展只留条目、正文为空，材料修订的 `content` 为空；不给：列表（`/items`、`/today`、`/materials`、`/revisions/pending`、`/context`）里不出现，单条读与它的经过、回放返回 403（退出码 4）。单条读另带 `content_scope` 与正文被截时的 `trimmed`；列表带 `content_scopes: {"<id>": "excerpt"|"meta"}`，全文的不列。带 `q` 的搜索在裁剪后的内容上匹配；材料搜索对外部钥匙只留按全文能读的材料。`/why/{id}` 对外部钥匙：全文档位按凭据级别回放，元信息与摘录档只给信封（同 observer：类型、时间、操作者、授权单号与判断结论，不含载荷、理由与证据），不给的 403；`/items/{id}/trace`、`/source-events/{id}/trace` 的行里带进展正文，只在全文档位可读；其余对象（运行等）与 `/audit/events` 只给信封。外部钥匙读 `GET /sources`、`/sources/{id}` 只看到有档位的来源，且不带 `command`、`config`、`credentials_path`、`cursor`、`outbound`。`attachments` 里的 `ref` 是来源程序的取回句柄：来源接入声明里没有 `content.attachments: fetch` 时，响应不带每项的 `ref`（没有新字段；Web 以有无 `ref` 决定是否显示「取回」）。

`GET /sources/{id}/events` 与 `GET /items/{id}/messages` 的每条事件：`detail` 改为预览（至多 2000 个字符），新增 `content`（同上，不含各部分内容）；聚合消息不在列表里返回。外部钥匙按档位：`excerpt` 的 `detail` 是正文前 500 个字符、`meta` 不带 `summary` 与 `detail`，两者都不带 `content`；`GET /items/{id}/messages` 里没给的来源的消息不列。

来源事件字段新增：`content`（只读）。来源登记字段新增：`content_retention_days`（只读，经 `/settings/sources/{id}/content-retention` 改）；`stats` 新增 `events`、`content_bytes`、`last_failure_kind`（最近一次失败的类别：`transient`、`auth_or_config`、`permanent`；拉取成功后清空，没有失败时不出现）。

事项字段新增：`imported`（只读，`{"batch", "at"}`；导入已取消，只见于旧记录）；`proposal.urgent`（只读，可缺）。`GET /items/{id}` 的进展条目与阶段变更在导入写入时带 `precision`（`day` 时界面只显示到日）。`GET /audit/events` 的每条事件新增可选的 `imported`、`recorded_at`；带 `imported=<批次>` 可只取一个批次的事件。`GET /events`（事件订阅）不返回带导入标记的事件。

`POST /items/{id}/report` 的 `link` 可以是字符串或字符串数组；回执事件的证据里 `link` 仍是第一个，另带 `links[]`。

`GET /context`、`GET /today` 与简报的资料文件不含导入事件产生的增量；导入的事项本身照常出现在事项列表与"今天"里。

### 拉取事件

在「设置 › Agent」的脚本钥匙管理区为每个消费程序签发一把 observer 钥匙。程序携带 `Authorization: Bearer <token>` 请求 `GET /v1/events?cursor=40&limit=100&wait=30`。worker、delegate、本人 Web 会话及具有 export 或 forward 作用域（含 `.<名>`）的 ext 也可读。

| 参数 | 缺省 | 十进制整数范围（含边界） |
|---|---:|---|
| `cursor` | 0 | 0 至 2^62。0 从可读历史开头拉取。 |
| `limit` | 100 | 1 至 500，按可见信封数计。 |
| `wait` | 0 秒 | 0 至 2^31 秒，实际最多等待 60 秒。有新事件即返回。 |

HTTP 200 返回登记过的现场事件，按 `seq > cursor` 升序排列，允许跳号。信封只含元信息，可选的 `title` 随对象提供。正文按对象接口的权限另取。

```json
{"events":[{"seq":42,"type":"item.created","object":"item_example","title":"核对简报","actor":{"type":"user","principal":"owner","via":"web"},"at":"2026-11-18T08:30:00Z"}],"next":43}
```

消费者按账本与 `seq` 去重，整页处理成功后才持久化响应中的 `next`。上例处理完 42 应保存 43。空页如 `{"events":[],"next":43}` 也保存 `next`。中途失败或保存游标前退出，从上次成功游标重读，因此是至少一次投递。网络或服务端故障按原游标退避重试。HTTP 400 时修正参数，401 时检查钥匙，403 时检查凭据类型和作用域。

## B.3 错误响应

```json
{"code": "executor_not_allowed", "why": "执行者不在项目允许范围内", "next": "改选 allowed 中的执行者，或用 apa ask 交给本人", "allowed": ["work-host.cli"], "web_url": null}
```

- `why` 与 `next` 只写机制，不含账本里的用户内容。`allowed` 是 Web 中配置的执行者 id。本版另加：`allowed_match`（按机器或 harness 的匹配规则）与 `candidates`（名录里当前命中名单的执行者 id）。
- `reason`（可缺）：同一个 `code` 之下更细的稳定原因。权限拒绝只有三种口径（主文档 §14.3）：`hard_line`（`code` 为 `web_only`，退出码 20：3b 或系统内部动作，只有本人在 Web 做，与档位无关）、`tier_closed`（`code` 为 `tier_closed`，退出码 23：该 Agent 的档位没放开这一档）、`out_of_scope`（`code` 为 `permission_denied`，退出码 4：钥匙范围外）；Web 会话做 3b 而没刚输过口令时 `code` 与 `reason` 都是 `reauth_required`（退出码 20）。拒绝的 `web_url` 落在该动作所在的页面。同一取值记进 `command.rejected` 与回执告警的载荷，`last_rejection(s)` 与 `GET /health` 的 `receipt_failures.receipts` 也带它。旧模型的原因 `grant_screening_pending`、`grant_risk_ceiling`、`grant_not_for_background`、`untrusted_origin`、`untrusted_trigger` 不再产出，只出现在旧的拒绝记录里。
- 委派记录与执行者句柄未通过校验时返回 400（退出码 2），`why` 只写键路径与规则名，不回显取值（主文档 §6.17）。
- HTTP 状态与退出码的对应：400 → 2；401 → 3；403 → 4、20、21、23（由 `code` 区分）；404 → 5；409 → 6；其余错误 → 1。不可达时 CLI 返回 7。
- 退出码 22（`risk_screening_pending`）与退出码 23 的旧码 `basis_required`、`session_not_direct` 属于已取消的初筛与本人原话机制（主文档 §14.4、§14.5），内核不再产出，取值保留为只读的历史值。
- 登录被限速或锁定时 `POST /auth/login` 返回 429，`code` 为 `login_locked`，带 `Retry-After`。
- 本人暂停了模型时，需要模型的本人操作（积压的影子打分与放开重判）返回 409，退出码 6，`code` 为 `models_paused`，`next` 提示到"设置 › 模型"恢复（主文档 §8.7）。
- 退出码 23 的原因 `grant_required`、`import_source_mismatch`、`import_limit_exceeded` 只为 `import.apply` 产出；导入已取消（主文档 §7.1），内核不再产出，取值保留为只读的历史值，只出现在旧的拒绝记录里。
- 取回附件失败（来源程序报错、超时或超出输出上限）返回 502，退出码 1，`code` 为 `fetch_failed`，`why` 取自来源程序 stderr 的末行；附件清单不变（主文档 §7.6）。
- 来源事件超过单条上限被拒收时，"被拒收的事件"里的原因是 `event_too_large`，带实际字节数。

## B.4 标准事件类型

对外事件类型只增不改。初始集合：

`item.created`、`item.updated`、`item.phase_changed`、`item.deadline_passed`、`decision.requested`、`decision.resolved`、`delegation.created`、`delegation.reported`、`command.rejected`、`material.revised`、`material.revision_proposed`、`guideline.activated`、`project.updated`、`project.allowed_executors_changed`、`rule.mode_changed`、`allowlist.changed`、`policy.changed`、`grant.issued`、`grant.revoked`、`agent.configured`、`agent.queued`、`run.started`、`run.finished`、`source.failed`、`health.alert`。

v0.2.0 新增四个登录审计类型（主文档 §14.13），对象是 `owner`，载荷不含口令：登录成功、同一来源地址失败锁定、改口令（Web 或主机上 `apa init --reset-password`）、吊销会话（逐个或全部）。

`auth.login`、`auth.login_locked`、`auth.password_changed`、`auth.session_revoked`。

v0.2.0 新增两个类型（下一行），排在初始集合之后：前者表示事项追加了一条进展；后者表示建议被补到已有事项，对象是被补到的事项。

`item.progress_added`

`proposal.attached`。

工作台能力迭代新增一个类型（下一行），导入取消后是只读的历史值，不再写入：一个导入请求写完，对象是批次，载荷是来源名与新建、已存在、不合法的条数（`source`、`created`、`existing`、`invalid`）；同一批次分几次请求提交时每次各记一条。带导入标记的事项事件不进订阅流，订阅方要拿到导入的内容，在收到它之后按 `origin` 经只读 API 取（主文档 §7.1）。

`import.completed`。

连接登记的事件（连接的新建、改配置、启用、停用、改名、删除）是内部类型，不进订阅流，不在本表。钥匙档位的两个事件同为内部类型：`credential.scope_changed`（对象是凭据 id）、`credential.scopes_migrated`（升级时的一次性换算，对象 `settings:credentials`，载荷是每把钥匙换算出的档位）。权限模型切换（主文档 §16.5）记一条内部事件 `authz.switched`（对象 `authz`，载荷是每一步的计数与明细），同样不进订阅流。`grant.issued`、`grant.revoked` 保留在本表：授权单取消后内核只在切换时写 `grant.revoked`，此后不再写入。

已有类型的信封不变；工作台能力迭代给信封新增两个可选字段 `imported`、`recorded_at`（只出现在导入事件上，而导入事件不进订阅流，所以订阅方看不到它们）；入站处置类事件的载荷新增可选的 `material`（这次调用送出的资料字符数、裁剪前的字符数与裁剪方式）；`delegation.reported` 的证据新增可选的 `links[]`。`item.updated` 的载荷新增可选的 `planned_for`，入站处置类事件的载荷新增可选的 `judgment`（判断记录 id）。v0.1 写下的 `run.finished`（`category: judge`）仍可回放，新的判断不再产生它（主文档 §8.6）。

## B.5 协议与版本

`apa version` 与 `GET /version` 返回同一份内容：

```json
{"version": "0.2.0", "commit": "…", "test_clock": false, "schema": {"ledger": 9}, "protocols": {"http_api": "v1", "source_contract": 1, "channel_ext": 1, "connector_contract": 1, "probability_judgment": "probability-judgment/1"}}
```

`commit` 是构建所在的提交（不是从提交构建时为 `unknown`）；`test_clock` 在 `apa_testclock` 测试构建里为 true，发布构建恒为 false。`schema` 是各存储模块的数据结构版本，随发行只增；旧版本的二进制拒绝打开版本更高的账本（主文档 §16.5）。账本结构版本 3 在第一次有真实账本升级到它之后冻结，此后的结构变更另起迁移、版本依次递增，所以发布物里的 `ledger` 是发布时主干上的最新版本：来源显示名一列在版本 3 冻结之后加入，此后每次结构变更各占一个版本，主干现为 9（版本 7 新增连接与凭据保管的三张表，已冻结）。`connector_contract` 是进程内连接插件接口的版本（主文档 §10.3），仓库内只增不改。工作台能力迭代的全部结构变更收在一次迁移里，已合入，主干为 8（主文档 §16.5）；结构版本 8 在第一次被真实账本应用后冻结。概率判断协议以主文档 §8.8 为权威定义，本附录不重复字段。

## B.6 v0.2.0 对已有契约的改动

账本结构版本 9 新增 Agent 实例表与钥匙的 `agent_id`。升级时每份已确认声明呈现一张 `legacy:<harness>` 兼容卡。旧 Run、Event、授权和默认设置保留原值。本人首次同时保存 `default_agent_id`、`main_agent_id` 与 `version` 后，即使清空角色也按未设置处理。旧客户端再写 `default_harness` 返回冲突。新增卡按 ID 匹配钥匙、授权与权限上限。


跨来源相似合并与推送哪些事（2026-11-23）只做加法：`proposal` 增 `merges`、`similar`、`not_same`、`split_from`，策略增 `merge_judge` 与 `disturb.push`，`source.failed` 增 `kind: streak`，新增 `POST /source-events/{id}/split`。不改账本结构、不迁移：旧建议没有这些键，读作没有合并留痕；旧策略没有这些键，按缺省（合并判断方 `same`，推送前三类开、新建议关）。升级前已合并的建议只有 `sources`，没有留痕的来源也能拆，新建议用来源事件的标题与摘要。同日修订为批量归类，仍只做加法：`proposal` 增 `sweep`（`state`、`batch`、`action`、`target`、`why`、`by`、`run`、`size`、`chars`、`at`），`proposal.merges[]` 增 `why`、`batch`、`run`、`item`，策略增 `merge` 段，`merge_judge` 只剩 `same` 与 `off`（第一版的 `judge`、`generate` 未被任何实例保存过）；`/settings/model-endpoints` 不再有 `merge_judge` 段，移到 `/settings/merge`；内置模板 `merge-same*` 换成 `merge-batch`。升级前的建议没有 `sweep`，不进批、不回填，只作为比对对象。

提醒配置跟着对象走（2026-11-23，主文档决定 88）只做加法，不改账本结构：`mutes` 的 `scope` 新增 `all`（`id` 为空）；新增只读路由 `GET /settings/notify/overview`；来源契约在临时失败时可声明 `{"chance_period": "<时长>"}`，`source.failed`（`kind: streak`）载荷可带 `chance_period`。旧策略没有 `all` 范围，行为不变；旧来源程序不声明，提醒线按拉取周期算，拉取周期不超过 1 小时的（线上全部来源）与升级前一样是连续 3 次。**行为变化**：拉取周期超过 1 小时的来源从连续 3 次改为失败 1 次就提醒；"暂停所有提醒"只压健康类以外的提醒，健康类照常送达（与按来源、项目、事项的静音一致）。回滚：旧二进制读到 `all` 范围的静音会因范围不合法拒绝整份策略——回滚前先关掉"暂停所有提醒"并保存一次。

投递矩阵（2026-11-23）只做加法，不改账本结构：策略的 `disturb` 段新增 `delivery`，响应另带只读的 `delivery_saved`；没有 `delivery` 时按旧键换算，去处与升级前逐值相同；写入后 `routes`、`push.*.route`、`proactive.task_push`、`proactive.instance_push` 冻结（改动为 400）。已有部署启动时一次性采用建议值（主文档 §16.5 第 4 条）。两层都推时 Agent 层的投递 `id` 为 `<基础 id>@agent`。**行为变化**：来源失败、运行失败、没出来与新建议攒多了受静默约束，静默期间攒着、结束后合成一条（摘要 `id` 以 `digest:` 开头、`item` 为 `attention:digest`）；到点提醒送到渠道时计入日上限；"下次对话时带上"的投递无人领取转 Web 未读后，没注入过、没在 Web 读过的下一次对话仍会带上；定时任务的产出不再并入低优先级摘要；决策被回答时同一版本的两层投递一并确认。`routes.wake` 本来没有读取方，设置页不再提供。

主动提醒（2026-11-23）只做加法：事项新增可选字段 `daily_at`，策略的 `disturb` 段新增 `proactive`，新增只读路由 `POST /settings/proactive/preview`；没有新事件类型。到点提醒的合并投递 `class` 为 `reminder`、`item` 为占位 `attention:schedule`、`id` 以 `schedbatch:` 开头，例行任务结果的投递 `id` 为 `task:<事项 id>`；路由为唤醒类路由的投递现在也有不带 `run` 的（提醒、简报、健康类），网关插件只注入事实、不换取运行凭据。不改账本结构、不迁移：暂存与"已处理"沿用触达已有的两张表，旧策略没有 `proactive` 读作默认。**行为变化**：简报与复盘默认注入主会话（主会话能唤醒时），只走这一条，不再按 `routes.brief` 发；要恢复原样，把 `task_push` 的 `brief`、`review` 设为 `false`。计划日期与时段在开启到点提醒后会产生提醒，默认关时与之前相同。

按推荐处理必须带范围（2026-11-23）：`POST /proposals/batch` 的 `recommended` 方式、`apa proposals apply --recommended` 与 Hermes 插件的 `apa_proposals_apply`（`recommended=true`）不带范围（`ids`、`source`、`project` 都没有，或 `ids` 为空数组）时拒绝：HTTP 400 `invalid_request`，CLI 退出码 2，一条都不处理。此前都不带表示"全部带推荐的建议"。不改账本、不迁移。**调用方要改的**：按推荐处理全部建议的脚本与 Agent 提示词要显式带上来源、项目或 id 列表。

来源契约：聚合消息允许没有文字（2026-11-23，主文档 §9.2）：`items[].text` 可以为空，不再因一条消息没有文字而整条事件拒收（原因 `items[N].text is empty`）；每条消息新增可选的 `attachments`（`[{kind, name?, media_type?, size?, ref?}]`，`kind` 必填，缺了整条拒收），附件只以元数据入账，不取内容。事件原文的 `messages` 部分与 `GET /source-events/{id}` 的消息照原样带 `attachments`；判断、抽取的模型输入与候选事项正文里，附件写成 `[attachment kind=… name="…" media_type=…]`（不含 `ref`），既无文字又无附件的写成 `[no text, no attachment]`。只做加法：不改账本结构、不迁移；旧事件没有 `attachments`，读法不变；已被旧校验拒收的事件不补回。旧内核遇到带 `attachments` 的输出会忽略该字段，但空文字仍整条拒收，所以发空文字消息的来源程序要在内核升级之后再换。

"今天"的逾期与高优先级（2026-11-23）只做加法：`GET /today` 增 `overdue`、`high_priority` 两组，旧的四组含义不变；简报与复盘资料 `due_waiting` 的 `reason` 增 `high_priority`。不改账本、不迁移；只认四组的调用方原样忽略新组。

合并结果按"N 条消息 → 这一条"展示（2026-11-23）只做加法：`GET /merge/activity` 每条结论增 `sources`（这条建议带着的消息条数）；邮件来源的事件在代码托管平台的通知邮件上增 `link`（所说 PR 或 issue 的规范链接，附录 D.2），其他邮件仍没有 `link`。不改账本结构、不迁移、不回填：已入账的旧通知邮件没有 `link`，与平台事件仍由批量归类判断。同日追加：归类折叠时被折那条名下的消息（含挂靠消息）全部搬到去处，"并入已有事项"也搬；仍是建议的事项收到来源对象更新时，消息记进它的 `proposal.sources`（此前只有回音记进去）。仍不改账本结构；存量用 `apa repair-merges` 一次性补齐（可重复执行），不随升级自动做。**调用方要注意的**：按 `proposal.sources` 取建议原文的调用方（如简报资料）会多拿到这些更新。

手动合并与合并护栏（2026-11-24）只做加法：新增 `apa merge run`、`apa merge mark-pending`（B.1）与系统内部命令 `inbound.mark_pending`。事项的 `proposal` 增 `absorbed[]`（归类折叠进这一条的建议本身：`item`、`title`、`source`、`ref`、`project`、`created_at`（它自己建立的时刻）、`action`、`batch`、`at`（折叠时刻），折叠时写在去处、撤销时删掉，`merge`、`attach`、`duplicate` 三种都写，被折那条名下没有消息时也写）；`proposal.sweep` 增 `backfill`（`by`、`at`，本人补打的待归类）。合并护栏：只有建议与去处都有项目且不是同一个时才拦——不当候选、不送模型，落账时也一律按"留着"处理；一边或两边没有项目放行。`apa serve` 在数据目录下新建 `host/`（0700）与其中的 `serve.sock`，服务停止时删除套接字。都在事项的 JSON 里，不改账本结构、不迁移、不回填：已折叠的旧记录没有 `absorbed`，仍可沿 `proposal.sweep.target` 与 `proposal.merges[].item` 回溯；不认识新字段的调用方原样忽略。回滚到上一版二进制前，先确认没有补打了但还没归类的条目，否则旧版会把它们当普通待归类自动合并，且旧版没有护栏（`apa merge run --dry-run` 的 `matched` 为 0）。

合并的判定结构与批量（2026-11-24 第二次）只做加法：`proposal.sweep` 增 `group`（`<批号>/G<k>`）与 `topic`（模型给的主题）；`proposal` 增 `integrated`（只在批内整合的载体上：`title`、`summary` 是整合前的原文，`to_title`、`to_summary` 是写入的整合文本，`members[]` 含载体自己，`batch`、`run`、`at`）；`proposal.absorbed[]` 增 `summary`（原始摘要）与 `events[]`（随折叠搬过去的来源事件 id）。`apa merge run` 增 `--with-model`，输出的 `groups[]` 改为模型给的分组，原先按候选关系连成的组去掉，`blocked_cross_project[]` 改名 `blocked[]`（另含同仓库不同 PR 号的）。策略 `merge.batch_size` 范围 2–100，0 的缺省 100。不改账本结构、不迁移、不回填；旧条目没有这些键按没有处理。回滚前先确认没有补打了但还没归类的条目，并把 `batch_size` 改回 50 以内。同日第三次（合并边界与完整性）同样只做加法：批内整合的标准改为"合并之后没有哪一条还要本人单独处理"（提示词正文变化，输出结构不变）；回答缺任一成员编号即不完整、整批不落账，重问一次后降到 50 条一批；`apa merge run` 输出增 `calls[]`；生成型 `run.finished` 载荷增 `elapsed_ms`、`output_chars`，失败时增 `shape`；按键取值的调用方不受影响。

合并 v3（2026-11-24 第四次）：①批量只有 `merge.batch_size` 一个数，攒满即跑、也是一批上限；回答不完整时降到它的一半（此前写死 50）。②批内整合同时改写载体的正文，`proposal.integrated` 增 `body`（整合前的正文）与 `to_body`（整合后的正文），撤销时正文一并还原；模板 `merge-batch` 的 `combined[]` 增必填 `body`，正文缺成员的关键数字或编号即退回保留。③下一批的资料列出最近 20 批的主题名供沿用，从已有的 `proposal.sweep.topic` 读，不另存。④规则检索只排序取前 `merge.candidates` 条（新增可选策略键，默认 20，可设 1–50），不设相似度下限、不拦；`merge.similarity` 不再参与，旧策略里的值原样保留；`apa merge run` 的 `ask[].blocked[]` 去掉，增 `topics[]`、`groups[].reused`、`combined[].body`、`combined[].rejected`、`decisions[].rejected` 与 `rejected` 计数。⑤禁止性规则集中到落账前的第 6 层校验，`proposal.sweep` 增 `rejected`（类别码，见主文档 §9.3）；attach 落到已完成事项不再被内核改判为 duplicate，按 `action_mismatch` 留着。都在事项与策略的 JSON 里，不改账本结构、不迁移、不回填。回滚：二进制回到上一版即可，旧版忽略这些键；已整合的载体正文是整合后的文本，要复原原文须在回滚前对它们执行撤销（旧版撤销不还原正文）；旧版的相似度下限照旧按 `merge.similarity` 生效。

合并 v3 正文补充（2026-11-24 第五次）：整合后的正文 = 模型写的要点清单 + 内核按成员顺序逐字附上的「来源原文」（来源、时间、原题、原摘要），总长受新增可选策略键 `merge.body_max_bytes` 约束（字节，缺省 32768，可设 4096–262144），原文先按内容去重（换行符统一、去行尾空白、连续空行压成一个、去首尾空白后非空且完全相同的只附成员顺序在前的一份，附原始字节，抬头标「另有 N 条正文与此完全相同」并列出各条来源与时间；不完全相同的不做模糊去重），额度在去重之后分配；超限时清单整段保留、原文按成员均分余下额度并在截断处写明截掉多少字节；`combined[].body` 即这段整文；关键事实校验另核对每个成员原始正文里的数字与编号（缺的在 `missing[]` 里带「原文：」）；`proposal.absorbed[]` 增 `body`（被折那条的原始正文全文）。仍只是事项与策略的 JSON 键，不改账本结构。回滚：**回滚前必须先对已整合的建议执行撤销（旧版撤销不恢复正文）**，再换二进制；旧版忽略 `body_max_bytes` 与 `absorbed[].body`。

对话捕获默认开启（2026-11-23）只做加法：新建项目的 `capture` 恒为 `true`，`POST /projects` 与 `apa project create` 不再接收 `capture`（带了是 400 与退出码 2）；已有项目升级后由启动迁移一次性补开（经 `project.update`，操作者 `kernel:defaults`，幂等，不改账本结构）；`PATCH /projects/{id}` 与 `apa project update --capture` 仍在（3a，默认已开，一般用不到），Web 项目页不再有这个开关。`POST /capture` 增可选 `failures`，`GET /health` 增第十四项 `capture_failures`，新增内部事件 `inbound.capture_failed`（不在 B.4 对外稳定集合里）。

按进展日读事项（2026-11-23）只做加法：`GET /items` 增 `progress_from`、`progress_to`，带上时响应另有 `progress`。不改账本结构、不迁移；不带这两个参数时响应不变。

钥匙档位（2026-11-23）：`auth` 迁移 v4（账本结构版本仍为 9）给凭据加 `audience`（缺省 `external`）与 `source_scopes`；升级后第一次启动时一次性换算，记 `credential.scopes_migrated`：observer、worker、delegate token 按各来源当时的 `outbound.external` 换算成它们当时经 token 实际读到的档位（`full`、`excerpt` 照给，`meta`、`none`、未选择换成 `meta`），ext、webhook 为空表，没有钥匙被标成内部。行为改动见下表「钥匙读来源内容」一行。回退只有旧二进制加升级前的账本快照一条路（新账本对旧二进制是 `ledger_too_new`）。

除下表外，v0.2.0 对 v0.1 的命令、路由、字段和事件都只做加法。

| 改动 | v0.1 | v0.2.0 | 兼容 |
| 简报与复盘资料的 `activity` | 进展按事件的写入时刻 `at` 取窗口 | 进展按业务日期 `on` 取（[窗口起点那天, 窗口终点那天)，主机日界）；没有 `on` 的进展与完成仍按 `at`；条目增 `on`、`date_source`（`on_date` 或 `at`） | 不改账本、不回填 `at`：只改哪些记录进窗口。按"写入时刻在窗口内即算本期"断言的调用方要改；补记的历史进展不再进本期活动 |
| 简报资料的 `proposals` | 窗口内新建的建议，至多 50 条取最新，另给 `pending_total` | 全部未处置的建议按优先级与到期、今天不处理有后果、本期新增（把握低在前）、更早的把握低排序：前 20 条给全，其余必须本人看的进 `also_needs_you`（精简，至多 40 条）；新增 `new_in_window`、`new_by_source`、`listed`、`not_listed`、`must_not_listed`、`more_web_url`，条目增 `why`、`created_at`、`deadline` | `pending_total` 与条目原有字段含义不变；按"proposals 全是窗口内新增"读的调用方改读 `new_in_window` 与条目的 `why` |
| 任务型资料文件 | `notice`、`item`、`materials`、`messages` | 在 `materials` 之前增 `catalog`（各段的目录） | 只增：旧键不变，按键读的调用方不受影响 |
| 运行目录的 `output.txt` | stdout 只留最后 64 KB，丢掉的开头不留痕 | 超过 64 KB 时全文另按 64 KB 分段落盘为 `output.001.txt`、`output.002.txt`……（依次拼接即全文）；`output.txt` 仍是末尾 64 KB，首行与末行各一行 JSON：`{"type":"apa_output_truncated","total_bytes":…,"kept_bytes":…,"dropped_head_bytes":…,"full_text_files":[…],"note":"输出共 N 字节，此处为末尾 M 字节；全文见 …"}` | 没超出时文件与原来相同、不分段；按行解析事件流的读者跳过这一类型即可；运行目录保留期不变 |
| 连接与凭据保管 | 无 | 新增 `/connections` 路由与 `connection.*` 命令（只在 Web）；新增 `apa secrets rekey` | 只增：已有路由、命令与 ext 协议不变 |
| 自检的检查项 | 九项 | 十一项：末尾追加 `secrets`、`connections` | 前九项的名字与顺序不变；`channels` 不再把内置渠道连接的路由 `channel:ch-…` 报成无密钥积压 |
| 账本结构版本 | 6 | 7：只增 `connections_connection`、`connections_status`、`secrets_value` 三张表 | 升级时自动留快照；旧二进制拒绝打开新账本，回退用快照 |
| 数据目录与备份 | 无主密钥 | `master.key`（0600）在升级后第一次启动时生成；`apa backup create` 默认带上，`--no-secrets` 不带并列入待办记录 | 恢复不带主密钥的备份时服务照常启动，连接显示要重新填写凭据 |
| 渠道 ext 的名字 | 任意 | 不能以 `ch-` 开头（留给内置渠道连接的路由） | 已签发的密钥不受影响；新签发时拒绝这个前缀 |
|---|---|---|---|
| 运行目录默认保留期与清理时机 | 默认 14 天；清理在维护循环里每分钟跑 | 默认 30 天；清理在启动时跑一次，之后每天 04:10；三项保留期可在设置里改，存账本 | 默认值变更：没在 `config.yaml` 写 `run_retention_days` 的部署，到期时间由 14 天变 30 天（只是更晚删）；写了的不变；设置里改过的优先于配置。账本结构版本 9 内追加迁移（retention/1），只增 `retention_setting`、`retention_sweep` 两张表 |
| 凭原话的第二档命令 | 只要求原话与会话引用非空 | 另要求 `X-APA-Session-Kind: direct` | 没有兼容期：缺这个头即退出码 23。harness 插件须与内核一同升级。**已取消（2026-11-20）**：本人原话与会话类型不再参与判定，见本表末行 |
| `/settings/model-endpoints` | 一个 `model_endpoint` | `judge_endpoint`、`generate_endpoint`、顶层 `screening_timeout_ms` | 本版 `PUT` 仍接受旧形状；已存的策略版本不改写，读取时等同 `generate_endpoint` |
| `GET /health` 的信号数 | 十一项 | 十三项，末尾追加两项 | 前十一项的 id 与顺序不变；按下标取信号的调用方不受影响，按总数断言的要改 |
| 问题与事件分离（主文档决定 81） | 运行故障与被拒的代理写入把 `open`、`waiting` 事项转 `needs_input` 并挂内核问题（被拒的写入是单选项"继续"）；问题可以只有一个选项；授权单只在显式携带时生效 | 运行故障不转阶段，`run.finished` 载荷另带 `recorded`、`fault`，`GET /health` 的 `outcall` 计入并列出 `failures`；被拒的写入不转阶段，`last_rejection(s)` 对 `open`、`waiting` 也给出并带 `action`；写入问题须至少两个选项或 `free_text`，否则用法错误（退出码 2）；`ask` 只增 `code`、`params`，`Run` 只增 `fault`，回执告警载荷只增 `code`，`receipt_failures` 只增 `receipts`；delegate 与运行凭据没带 `--grant` 且原话依据不成立时，内核按被授权方找可用的授权单 | 不改账本结构、不迁移：旧记录没有新字段时按旧格式读，故障类别码按已存的终态、退出码与失败附注算出。升级后首次启动把仍未回答的 `system:proxy-rejection` 单选项问题降级为通知（回到原阶段、去掉问题、记 `item.phase_changed`，不写回答）。写单选项问题的客户端要补第二个选项或 `free_text`；按"被拒后转 `needs_input`"断言的调用方要改；带 `--grant` 的调用不受影响 |
| 动作失败、没拉起与来源认证失败是事件（主文档决定 81，§8.7、§9.2） | 规则建的事项在动作失败或任务型没拉起时转 `needs_input`；后台运行、续接复核不通过时转 `needs_input`；来源退出码 2 建 `needs_input` 事项；跑完没结论的问题选项为 `rerun`、`reassign`、`drop` | 规则建的那一份转 `dropped`，阶段变更理由写明原因；没拉起与续接复核不通过记带类别码的 `health.alert`，本人的事项阶段不变；来源退出码 2 只记 `source.failed`（`kind: auth_or_config`），来源 `stats` 只增 `last_failure_kind`；`GET /health` 的 `source_failures.detail[]` 只增 `kind`，`other_alerts` 只增 `notices`；问题选项为 `handle`（回到 `open`）、`drop`（转 `dropped`） | 不改账本结构、不迁移：只增字段与告警载荷键，旧告警、旧统计没有时按旧格式读。升级前已转 `needs_input` 的这几类事项原样保留，旧选项照旧可答（`rerun`、`reassign` 回到 `open`），来源认证失败的事项在来源恢复时由内核转 `done`。按"转 `needs_input`""建认证失败事项"或旧选项 id 断言的调用方要改 |
| `unjudged_reason` | `model_unavailable`、`undecided` | `model_unavailable`、`models_paused`、`outbound_unset`、`restored` | 旧值原样返回；内核按 `model_unavailable` 对待并参与重判。按枚举穷举判断的调用方要接受三个新值 |
| 策略的 `models_paused` | 无 | 顶层布尔，经 `/settings/model-endpoints` 读写 | 只增字段：已存的策略版本没有这个键，读作不暂停，不改写旧版本；`GET /health` 的 `model_unavailable.detail` 只增 `paused` 键与取值 `paused` |
| 生成端点的 `protocol` | 只有对话补全接口 | `generate_endpoint.protocol`：`chat-completions`、`messages`、`responses` | 只增字段：已存的策略版本没有这个键，读作 `chat-completions`，不改写，不迁移；`GET` 总是给出取值。旧二进制解码策略时忽略未知键，回退后按对话补全接口调用，选了另两种协议的端点会失败，在旧二进制里保存设置也不会写回这个键；回退前把协议改回 `chat-completions`，或回退后改用对话补全接口的地址 |
| `GET /health` 的 `unjudged_backlog.detail` | 各原因计数与 `sources` | 另带 `rejudge_days` | 只增字段，按键取值的调用方不受影响 |
| `disturb.threshold_judge` | 开启三选一的投递阈值判定 | ~~开启"可以晚点送吗"的推迟判定~~ **已取消（2026-11-23）**：不再有含义 | ~~键名与类型不变；已开启的继续开启，"只留 Web 未读"这一去向不再由模型选择~~ 旧版本里的值读入后忽略（开着的不再推迟，算轻微放宽），下一次保存不再写出；PUT 带着它照常接受并忽略 |
| `disturb.followup_defer` | — | ~~开启"这一轮可以晚点提吗"的跟进推迟判断~~ **已取消（2026-11-23）**：不再有含义 | 同上；覆盖包里的 `defer_with` 照常加载、内核不再读 |
| 实例驱动的时钟规则 | `GET /rules` 列出，`PATCH /rules/{id}/mode` 可改，值不生效 | `GET /rules` 不再列出；`PATCH` 返回 404 `not_found`，与规则不存在一致 | 账本里 `rules_state` 已存的行不删不改，内核照旧用它们执行实例到期；只是不再读出给人看。调用方按列表取规则的不受影响，直接对这类规则改模式的要按 404 处理 |
| 来源规则的 `then: judge` | 交判断型三选一 | `screen` 的别名 | 仍被接受 |
| 判断留痕 | `run.finished`（`category: judge`）事件 | 判断记录（`/judgments`） | 旧事件保留、可回放；不计入校准统计 |
| 健康视图只显示现状 | 来源恢复后仍因留下的 `needs_input_item` 报"来源失败"；模型告警要同一模板之后成功一次才清除；熔断、外呼超时、其他告警、规则回退一直累计 | 每个信号只计现状，恢复条件见主文档 §15.2；`model_unavailable.detail.judgements[]` 加 `endpoint`；测试连接与 `GET /doctor` 在测通已保存端点、且有未恢复的失败时记一条 `run.finished`（载荷加 `probe: true`） | 不改账本结构、不迁移：`health.alert` 等原事件原样保留，只是不再算作现状；只增字段与一种 `run.finished` 载荷，按键取值的调用方不受影响。来源恢复时 runner 把自己建的、仍在 `needs_input` 的事项转 `done`（理由写在阶段变更的 Event 里），升级前留下的这类事项在下一次拉取成功时结束 |
| 来源的显示名 | 无；Web 显示来源 id | `name` 总有值：登记时给定或取来源程序的接入声明；都没有时登记被拒（400），重名的登记与改名返回 409 | 只增字段与列（`sources_registry` 迁移 3，账本结构版本 3 → 4）。已有来源在 `apa serve` 启动时补名：先按接入声明，没有声明的按类型起名并记一条健康说明，逐个记 `source.renamed`（操作者 system），不触发判断、重判或健康说明以外的投递。**调用方要改的**：登记仓库之外的来源程序（自定义脚本、自建拉取程序、非自带类型的 webhook）以前可以不带名字，现在必须带 `name` / `--name`，否则 400；同一程序登记多个实例而声明给出同名时（如两个 RSS 订阅）要显式起名。迁移后旧二进制拒绝打开账本（结构版本更高），回退要用升级前快照，见主文档 §16.5 |
| 规则初始模式 | 一律 `observe` | 提醒类 `auto`，动作类 `observe` | 升级时未动过的内置提醒类规则切到 `auto` 并记 Event（主文档 §16.5） |
| 执行者与委派（主文档 §6.17） | 执行者由本人在 Web 手填或确认插件声明后进入清单；`assignee` 不在清单里返回 5；确认声明时覆盖该 harness 的全部条目；名单只按 id；挂靠消息只对 `assignee` 推进 | harness 随 `apa delegate --executor` 登记，身份字段固定；不在名录里也可指派；确认声明并入名录、不删除；新增委派记录（`apa dispatch`、`/items/{id}/dispatches`、`/dispatches`）与 `apa context --scope item` 的 `history`；名单可按机器或 harness 匹配（`allowed-executors` 的请求与返回加 `match`）；21 的返回体加 `allowed_match`、`candidates`；发起委派的凭据与运行身份对目标的委派子事项也能推进挂靠消息并拿到确认游标；删除 `GET /settings/executors`（读名录改用 `GET /executors`）与 Web 设置里的手填表单；`DELETE /settings/executors/{id}` 改为 `DELETE /executors/{id}`；`POST /settings/executors`（`executor.put`）保留为只 Web 登录、无界面的本人维护入口（如登记交给人的执行者） | 只增列与表（账本结构版本 4 → 5），已有条目、声明与名单原样保留，名单判定不变。**调用方要改的**：依赖"未登记返回 5"的调用方改为自己先查 `GET /executors`；用 `/settings/executors` 读名录的改用 `GET /executors`。带调用表的旧插件仍可委派，只是不写委派记录。删除的路由与 Web 切换在同一次改动里完成 |
| 授权单的被授权方与有效期 | `grantee.credential` 必填；`expires_at` 必填，不超过 90 天；运行凭据带 `--grant` 一律退出码 4 | 另可 `grantee.agent`（按内核记录的 harness 匹配）与 `background`；`scope.all`；`expires_at` 可选（不设即长期）；允许后台使用的单对该 Agent 的运行凭据生效，不符时退出码 23（原 4） | 已签发的单原样生效。按"运行凭据带单为 4"断言的调用方改为 23。见主文档 §6.13、§14.5。**已取消（2026-11-20）**：授权单整套取消，见本表末行 |
| 来源事件的 `sender_verified` | 来源程序逐条自报 | 以来源程序的接入声明为上限，没有声明的程序按需确认（恒为假）；声明为"不带发件人"的事件类型清空 `sender` | 已落盘的事件不改写。自定义脚本与 webhook 转换脚本以前自报为真的，之后按未核实处理，转发白名单对它们不再生效（主文档 §9.1） |
| 代码托管来源的事件 | `*.updated`、`*.closed`、`*.merged` 的 `sender` 为对象作者 | 这些类型不带 `sender`；新增评论事件 `issue.commented`、`pr.commented`、`pr.review_commented`（webhook 另有 `pr.reviewed`），带已核实的评论作者 | 已落盘的事件不改写。按事件类型匹配的用户规则若要处理评论，需加上新类型（附录 D.4） |
| 渠道回答人 | 即时通讯渠道 ext 优先报用户名 | 一律报平台用户 ID | 写着用户名的旧条目不再匹配，本人在"设置 › 渠道"识别一次 ID（主文档 §16.5） |
| webhook 映射没写 `detail` | 事件的 `detail` 取原始 payload 的前 16 KiB | 不带 `detail`；原始 payload 只在接收记录里（`apa source ingests`） | 已落盘的事件不改写；已被原始 payload 覆盖正文的事项不修复，本人可在 Web 改正文。要把 payload 的某段作资料，在映射里显式写 `detail` |
| 代码托管来源的评论与评审挂到已有事项 | `issue.commented` 没有规则、按来源对象更新刷新事项的标题、摘要与正文 | 内置规则 `pr-discussion`、`issue-comment` 声明 `on_update: attach`：只记为挂靠消息，不改事项；`pr-activity` 只匹配合并、关闭与更新 | 覆盖了 `inbound-triage` 的同名包要同步这两条规则，否则评论仍按来源对象更新处理 |
| 入站处置的载荷 | 起草成败只写在理由文字里（`; draft failed: …`） | 另带可选的 `draft: {outcome, code}`，`outcome` 为 `ok`、`failed`、`skipped`，`code` 为 `outbound`、`unavailable`、`schema`、`unsettled`、`model`；只在初筛后进建议流的处置上有 | 只增字段，理由文字不变；已落盘的处置不改写，处理经过对旧数据按理由前缀推断，推断不出写"不清楚" |
| 抽检结论 | "该看"记 `inbound.disposed`（载荷带 `review`），"确实不用"只写判断记录 | 两种结论都另记一条 `inbound.spot_reviewed`（对象是来源事件，载荷 `{verdict, judgment}`，操作者是本人）；"该看"原有的 `inbound.disposed` 不变 | 只增事件类型，不迁移；之前的结论只在判断记录上，过了保留期就看不到 |
| `main_session` 的拉起方式 | 声明的 `main_session.launch` 单值决定，换方式要改插件、重新提交并确认声明 | 声明用 `main_session.wake` 表示支持唤醒；本人在 Web 经策略段 `main_session_launch` 按 Agent、按规则选；`GET /rules` 加 `runner`；`background.launched` 的载荷加 `launch_from`、`launch_fallback?`，处理经过的 `run` 行加 `launched` | 只增字段，不迁移、不加表。旧声明 `launch: wake` 视为支持唤醒并作为初始值，旧策略版本读作没选，已有部署的拉起方式不变；`launch: headless` 的旧声明照常接受。续接改按原运行的 `launch` 判断：原运行是无头运行的，声明支持唤醒也能续接 |
| 声明里唤醒能力的来源 | Hermes 插件看 `APA_WAKE_SESSION_KEY` 是否设置决定 `main_session.wake`，只在 Agent 调 `apa_declare_headless` 时提交；同一内容等于已生效版本时，仍待确认的旧版本留着 | 插件按唤醒插件的实际启用情况（已启用、指定了对话、允许插件发起回合、有连 APA 的钥匙）算 `wake`，不满足时带新的可选字段 `main_session.wake_missing`（每项 `{zh, en, how: {zh, en}}`，只在不能唤醒时允许）；gateway 启动时自动提交一次；`executor.declare` 的内容等于已生效版本时，同 harness 仍待确认的版本转 `superseded` | 只增字段，不迁移、不加表。旧内核严格解码会拒绝带 `wake_missing` 的声明，插件与内核一同升级。升级后插件提交的内容会变，本人在 Web 确认一次；确认前已生效的旧版本照常生效 |
| 凭据表 | 无 `harness` | `auth_credential.harness`，账本结构版本 5 → 6 | 只增列，已有 token 照常可用；迁移后旧二进制拒绝打开账本，回退用升级前快照 |
| 账本结构版本（工作台能力迭代） | 7 | 8：新增原文表、导入批次与导入对照两张表；来源事件表、Event 表、事项表、材料表、来源登记表各加列（主文档 §16.5） | 只增表与列，不改写已有行。升级时自动留快照；旧二进制拒绝打开新账本，回退用快照 |
| 时段与按日期范围读（W26） | 事项只有计划日期；`GET /items` 只能按某一天精确筛选 | 事项新增可选字段 `planned_slot`（经事项新建与更新写入，随事项事件的快照下发）；清除 `planned_for` 时一并清除 `planned_slot`；`GET /items` 新增 `from`、`to`，带范围时响应另有 `today`、`utc_offset`；内置起草与抽取的输出新增可选的 `planned_slot` | 只增字段与参数：时段存在事项数据里，不改账本结构、不迁移；已有事项与旧事件没有时段，按未定时显示；不认识新字段的客户端原样忽略 |
| 来源事件列表里的正文 | `GET /sources/{id}/events`、`GET /items/{id}/messages` 的 `detail` 是来源程序给的全部内容 | `detail` 是至多 2000 个字符的预览，另带 `content` 清单；全文经 `GET /source-events/{id}` | **调用方要改的**：从列表里读正文全文的脚本与插件改用单条读取。升级前落盘的事件（`content.state: legacy`）在单条读取里照常返回原来的内容 |
| token 读来源事件的原文 | observer 及以上的 token 能从事件列表读到全文，不受外发范围约束 | 全文只经单条读取，token 读到的内容按该来源的 `outbound.external` 裁剪；未选择或 `none` 时只有元信息 | 收紧。依赖 token 读全文的调用方，由本人把该来源对外部端点的范围选为全文；Web 上本人始终看到全文。挂靠消息不受影响 |
| 提醒事项来源的游标与事件 | 游标是拉取时刻；只发 `reminder.due`、`reminder.open` | 游标是拉取范围的指纹与上一轮看到的未完成提醒（id 到标题）；离开未完成集合的逐条核实，已完成的发 `reminder.completed`，仍未完成或读不到的发 `reminder.out_of_view`，内置规则 `reminder-status` 挂到已有事项（附录 D.2） | 旧游标读作基线：升级后第一轮不发这两种事件，此后正常；拉取范围变了同样只建基线；按事件类型穷举的规则要认识新类型 |
| 简报与复盘的取材与写回 | 实例第一份取 24 小时，窗口从上一份（不论成败）的 `until` 起；"知会"在拉起运行时即记为已汇入；复盘带上全部简报资料并按项目分节（`result_fields` 含 `sections[].project`）；事项标题的到期时刻按 UTC 写；推送摘要"按项目分节：…" | 第一份从本次到期之前的上一个调度节拍起，窗口只从上一份已完成的简报起；"知会"在本份成功后才汇入；资料段由模板 `materials` 决定，新增 `context`、`prior`、`open_items` 与条目的 `web_url`、`followed_by`；复盘 `result_fields` 为 `period`、`sections[].body`，不带知会与抽检；标题按实例时区；推送摘要"分节：…"，按 `title` 分的节也列出 | 账本结构不变，已生成的简报不改写；本人覆盖的旧模板不写 `materials` 时段集合不变，只是"知会"改为成功后汇入；按 `sections[].project` 判断复盘写全的调用方要改按 `body` |
| 来源程序给的 `detail` | 各内置来源程序把正文截到各自的上限（邮件 16 KiB、代码托管 4000 字符、订阅 4000 字符） | 不截断；新增可选的 `original`、`headers`、`attachments`、`content`；单条事件超过 `max_source_event_mb` 整条拒收 | 已落盘的事件不改写、不回填，标为 `legacy`。自写的来源脚本不必改；想让界面显示"完整"之外的状态时自报 `content` |
| 建议事项的正文 | 由来源事件建出的事项，正文是事件 `detail` 的副本 | 原文不超过 4000 个字符时同前；更长时只写前 4000 个字符并指回来源事件 | 已有事项不改。按事项正文取邮件全文的调用方改读来源事件 |
| 抽取出口的候选 | 每条候选直接建事项，不经合并键 | 每条候选走与起草相同的合并路径（合并键取标题加发送者），可能并入 7 天内已有的建议 | 行为变化：同一件事在两段消息里各被抽出一次时不再是两条建议。输出只给标题、类别、摘要的旧模板仍被接受 |
| 来源规则的 `prescreen` | 值是提示词模板名 | `none` 是保留字，表示跳过预筛 | 用户包里名为 `none` 的提示词模板在加载时被拒绝，改名即可 |
| webhook 映射产出的事件 | 映射没写 `detail` 时原始 payload 只在接收记录里，30 天后清除 | 请求体另作为事件原文的 `original` 部分永久保存 | 只增；已落盘的事件不补。请求体上限成为运行配置 `max_webhook_body_mb`，默认值与此前写死的 1 MiB 相同 |
| 事件日志与订阅流 | 每条事件只有一个时刻 `at` | 新增可选的 `imported`、`recorded_at`；带 `imported` 的事件不进订阅流、注入增量与规则 | 只增字段；已有事件没有这两个字段，即普通事件。不导入就没有任何变化 |
| 授权单可授权的动作 | 不含 `item.progress`（文档此前已列，实现未放行）；没有导入 | 加入 `item.progress`；不含 `import.apply`（导入已取消，见下一行） | 已签发的单内容不可改，不会因此多出权限。**已取消（2026-11-20）**：授权单整套取消，见本表末行 |
| 带原始时间的导入（已取消，2026-11-20） | 工作台能力迭代曾提供 `import.apply`、`POST /import`、`GET /imports`、`apa import`、Web "设置 › 导入"与导入专用授权单 | 全部删除：`POST /import`、`GET /imports` 返回 404，`apa import` 为未知命令（退出码 2），签发带 `import` 段或 `import.apply` 的授权单返回 400（主文档 §7.1、决定 83） | 账本结构不变：导入批次表、对照表、`policy_grant.used_records`、事件的 `import_batch`、`recorded_at` 与事项的 `imported` 只读保留、不再写入；旧的导入专用单读出时原样带回 `import` 段，不放行任何命令；`import.completed` 与三个导入原因码保留为只读的历史值。按 `origin` 查询照常可用 |
| 回执的链接 | `--link` 一个 | 可多个；库内链接在报 `done` 时自动登记为产出 | 只给一个时行为同前，证据里的 `link` 字段不变，另增 `links[]`。未登记任何笔记库的部署没有自动登记 |
| 材料的形态 | 两种，由有没有仓库位置区分 | 三种，新增只读字段 `form`；`POST /materials` 带 `link` 时登记链接型 | 只增；已有材料的 `form` 在读取时推定，不改写 |
| 简报的写回结构 | `spec.sections[]` 每节必须有 `project` | 每节有 `project` 或 `title` 之一；`spec` 另有内核写入的 `since`、`until` | 已有简报原样可读。自定义的简报视图若按 `project` 分组，要能处理没有 `project` 的节 |
| 提醒事项来源的事件类型 | 只有 `reminder.due` | 配了 `undated_lists` 时另有 `reminder.open` | 不配就没有变化；按事件类型匹配的用户规则若要处理没有到期日的提醒，加上新类型 |
| `GET /connections` 的权限 | 只有 Web 登录 | observer 及以上的 token 读裁剪后的字段（不含 `config`、`secrets`、`detail`）；新增 `apa connection list` | 只增：Web 读到的内容不变；写命令仍只在 Web |
| 笔记库打开地址的编码 | `{vault}`、`{path}` 按路径段编码：`&`、`=`、`+`、`$`、`:`、`@` 不编码，与 Web 预览不一致 | 按 URI 组件编码，内核与 Web 预览一致（主文档 §12.8） | 只改渲染结果，不迁移、不改存储；路径含这些字符时原来生成的是坏链接，修复后正确；另外 `!`、`'`、`(`、`)`、`*` 由百分号编码改为原样，两种写法等价；其余路径的结果不变 |
| `GET /settings/ext-keys` | 没有最近调用时间 | 每把密钥另带 `last_seen_at` | 只增字段 |
| 钥匙读来源内容 | token 读来源原文按该来源的 `outbound.external` 裁剪（挂在事项上的不裁）；列表、挂靠消息、外发预览、入站记录、被拒行与事项、材料不看档位；简报运行的原文文件按 `outbound.external` 写 | 只看钥匙：内部钥匙、运行凭据与 Web 全文；外部钥匙（含导出 ext）按 `source_scopes` 读来源事件、事项与材料，不给的不出现或 403；入站记录、被拒行、外发预览只给 Web 与内部钥匙；简报原文文件一律全文；`outbound` 只管内核调模型 | **调用方要改的**：迁移后现有 token 都是外部，事项正文按档位裁，本人在 Web 把 APA 内部用的钥匙标成「APA 内部」后恢复全文；依赖 `outbound.external = full` 才能读原文的脚本，标内部后不再依赖它 |
| 导出与转发密钥的 Web 入口 | 独立设置页（"扩展程序"，`/settings-credentials`） | 设置 › 系统 › 对外接口（`/settings/outward-api`），与"提醒渠道"平级，不混列渠道钥匙；是长期对外能力（主文档 §4.1、决定 77）；内置输出已取消（决定 78），仓库不再附带导出与转发客户端（决定 79） | 旧地址不保留别名。已签发密钥、授权范围和 HTTP API 不变，无需迁移 |
| 随发行附带的导出 ext 与 webhook 转发 ext | 发布包的 `ext/` 下附带 | 不再附带，需要的人自己写客户端接对外接口（主文档决定 79） | 导出与转发密钥、事件订阅、只读 API 与 ext 协议不变；已在运行的旧版程序照常可用，无需迁移 |
| `apa connection list --direction` 与连接的方向 | 取值 `channel`、`output` | 只接受 `channel`；`output` 返回用法错误（退出码 2）。`GET /connections?direction=output` 返回空列表 | 从未有过输出类型的连接，账本里没有方向为 `output` 的连接记录或事件，无需迁移；`direction` 列保持原样（只增不改） |
| 跨来源合并一批的资料上限 | 借用 `generate_endpoint.max_material_chars`（字符，默认 24000） | `merge.budget_tokens`（估算 token，默认 16000 = 24000 字 ÷ 1.5，可设 2000–500000，与写作资料预算同一份范围），只在设置 › 跨来源合并改；`proposal.sweep` 只增 `deferred` | 已存的策略版本没有这个键，读作默认，不改写。默认值与旧缺省等价；本人此前改过生成端点资料预算的部署，合并不再跟着它走，要在合并设置里另设 |
| 跨来源合并的判定结构与批量（2026-11-24） | 每条只带规则挑的 top5 候选、一对一选四种结论；已有建议与本人忽略过的记录也作去处；批量默认 20、上限 50；消息协议 `max_tokens` 4096 | 成员互相全量可见，一次调用先分组再按优先级（并入事项 → 批内整合 → 保留）；去处只有进行中与已完成的事项，批内合并只在成员之间；同仓库不同 PR 号不合并；整合改写载体的标题与摘要，原文进 `proposal.integrated`；`proposal.sweep` 增 `group`、`topic`，`proposal.absorbed[]` 增 `summary`、`events`；批量默认 100、上限 100；`max_tokens` 16384；`merge-batch` 超时 300s | 只增事项 JSON 的键，不改账本结构、不迁移，旧条目没有这些键按没有处理。策略里 `batch_size` 为 0 的部署升级后按 100 成批（不写策略）。回退前先确认没有补打了但还没归类的条目，并把 `batch_size` 改回 50 以内（旧版读到 50 以上的策略会校验失败）；已整合的建议在旧版里撤销成员只退回那一条、不还原载体文本 |
| 跨来源合并规则初筛的相似度下限 | 写死 0.30 | `merge.similarity` 三档：`loose` 0.25、空串（默认）0.30、`strict` 0.40，在设置 › 跨来源合并改，下一批即生效 | 只增可选键：已存的策略版本没有它，读作默认档，行为与升级前相同；不改写旧版本、无账本结构迁移 |
| 跨来源合并的规则检索与批量（2026-11-24 v3） | 相似度不低于 `merge.similarity` 档位才作候选、每条至多 5 条，跨项目与同仓库不同 PR 号在检索时拦下；回答不完整降到 50 条 | 只按分数排序取前 `merge.candidates` 条（默认 20，可设 1–50），不设下限、检索不拦，禁止性规则全在落账前的第 6 层校验并记 `proposal.sweep.rejected`；降批为 `batch_size` 的一半 | 只增可选键 `merge.candidates`，旧版本没有它读作 20；`merge.similarity` 原样保留、不再参与；不改写旧版本、无账本结构迁移 |
| `POST /proposals/batch`、`GET /items?phase=proposed|dropped` 与 `item.phase_changed` 载荷 | 两种方式；列表只有 `items`、`last_rejections` | 另有 `ended`（按上游已结束忽略）与 `undo`（按批撤销）方式，结果带 `batch`；`phase=proposed` 的列表另带只读的 `origins`，`phase=dropped` 另带只读的 `dismissals`；批量忽略的事件载荷另有 `batch` | 只增：已有方式、字段与载荷含义不变；无账本结构迁移。升级前的忽略没有 `batch`，在"已忽略"里归为逐条忽略的，不回填 |
| 通知邮件来源事件的 `link`（回填） | 升级前入账的通知邮件 `link` 为空 | 本人可运行一次性回填 `go run ./sources/internal/mailbackfill -ledger <账本> [-apply]`：只给 `mail.received` 且 `link` 为空的事件补上它所说 PR 或 issue 的规范链接（取法与邮件适配器相同：线程头里的平台写法，或正文的「view it on GitHub」页脚且全文只说这一个对象），取不到的不写 | 默认只演练、只读；`-apply` 在一个事务里只改这一列，条件带 `link = ''`，可重复执行，第二遍 0 变更。不改正文、合并键、建议与事件日志，不记 Event；改动清单就是它输出的 JSON 行，执行前先 `apa backup create`，回退即恢复备份 |
| 生成端点的资料长度 | 资料原样拼进提示词，没有预算 | 受 `generate_endpoint.max_material_tokens` 约束（估算 token，默认 16000，可设 2000–500000），超出时裁剪并留痕 | 已存的策略版本没有这个键，读作默认值，不改写 |
| 写作资料预算的单位 | `generate_endpoint.max_material_chars`（字符，默认 24000） | `generate_endpoint.max_material_tokens`（估算 token，默认 16000 = 24000 ÷ 1.5） | 旧键读取时一次换算（按 1.5 向下取整、落进 2000–500000）进新键并清空，换算有了 token 值就不再动，重复读与存回后再读都不会二次除；默认值与旧缺省逐字等价 |
| `GET /items?phase=done` 的 `done_at` 与迁移来的已完成事项（2026-11-23） | 迁移事项没记 `ext.workbench.completed` 时取 `ext.workbench.updated`，再缺才用写入时刻，被算成"今天完成" | 这类事项不再给 `done_at`，列入新增的 `done_unknown`，页面标"完成日期不详" | 只增字段 `done_unknown`；账本结构与事件不变。历史数据补日期走一次性外部脚本 `tools/backfill-done-at`（只经 `PATCH /items/{id}` 写 `ext.workbench.completed`，出处是迁移原文里带日期的进展，幂等，默认只演练），没有出处的不写、保持不详；补过的事项归到那一天 |
| 权限模型第二版：三档与硬线（2026-11-20，主文档 §14、决定 84） | 四档代理授权：第一档凭对象绑定；第二档凭本人原话（`--evidence`、`X-APA-Evidence`）加会话引用与会话类型（`X-APA-Session-Kind: direct`），或授权单（`--grant`、`X-APA-Grant`，另有自动匹配）；只接受本人原话的管理命令；web_only 类别与模型初筛（退出码 22）；问题的超时默认选项（`default_on_timeout`）；一次性表单回答；材料按类别"直接写"（策略段 `material_write`）；本人只在 Web | 每条命令固定一档（B.1 三档表）：一档永远开；二档、3a 按 Agent 的档位（策略段 `agent_tier`，经 `PUT /settings/agents/{agent}/tier`）；3b 永不放开，只在 Web 且会话须刚输过口令（`POST /auth/reauth`，登录后 10 分钟内视为刚输过）；拒绝只有 `hard_line`（20）、`tier_closed`（23）、`out_of_scope`（4）三种；本人另可经已配对渠道、回答人名单内的账号回答内核投出的那张卡。删除：`apa grant list`（"列出调用者能用的有效授权单"）、全局 `--evidence` 与 `--grant`、`apa ask --default-on-timeout`、`GET /grants`、`/settings/grants`（签发与撤销）、`PUT /settings/agents/{harness}/grant`（替换 Agent 的长期授权单）、`POST /forms/{一次性令牌}`（一次性表单回答）、命令 `channel.form_answer`、`grant.issue`、`grant.list`、`grant.replace`、`grant.revoke`、`proposal.batch`（批量改为逐条调用单条命令）、策略段 `web_only`、`material_write`、`screening_timeout_ms`（`PUT` 带它们为 400）、自检码 `host_no_session_kind`。新增：`apa authz preview`、`POST /auth/reauth`、`PUT /settings/agents/{agent}/tier`、`GET /audit/rule-changes` 与两条撤回路由、`GET /agents` 的 `tier` 与 `tier_projects`、事件 Basis 的 `via`、`command`、`tier`、`agent`、自检项 `authz`；改动：`GET /forms/{令牌}` 改为 302 到 `/decisions`；`DELETE /auth/sessions*` 要求刚输过口令；新建材料一律直接生效，改已有材料看 Agent 档位，规范类恒留待确认；Agent 替本人的回答记 `resolved_via: agent` 并另推一条提醒 | 不改账本结构（结构版本仍为 9）、不改写旧行。**迁移**：新版本的 `apa serve` 第一次启动时在一个事务里自动切换一次（主文档 §16.5）：撤销全部有效授权单（原因 `permission model switch`，记 `grant.revoked`）；写一个不含 `web_only`、`screening_timeout_ms`、`material_write` 的新策略版本，并为每个现有 Agent 显式写 `organize`（只整理），记 `policy.changed`；清除未答问题上的初筛标记、风险类别与超时默认，并取消它们的超时定时；记 `authz.switched`。切换前可用 `apa authz preview` 在快照上预演。旧数据只读保留：`policy_grant` 表、旧事件里的原话、会话引用、会话类型、`grant`、`on_behalf_of`、`channel_form` 表、旧策略版本都不删不改。旧客户端带来的 `X-APA-Evidence`、`X-APA-Grant`、`X-APA-Session-Kind` 与 `--evidence`、`--grant` 被忽略，不报错也不采信；请求体里的 `default_on_timeout` 返回 400。**调用方要改的**：按"带原话或授权单即放行"写的插件与提示词改为按档位处理 23；需要 3b 的自动化改为给出设置页链接。**回退**：旧二进制能打开同一结构版本的账本，但切换写入的策略版本缺少旧模型必需的段，旧二进制按它运行会出错；回退等于恢复切换前的账本备份，切换后写入的数据会丢 |
