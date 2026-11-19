# 导出与转发密钥移到「对外接口」页

基线 `7164adc7f`，分支 `feat/outward-api-page`，代码提交 `3d8a78b5c`。已快进合并到 `~/app/apa/src`、重建并重启本地 APA；未 push。

## 1. 新页的名字与位置

- 名字：**对外接口**（英文 Outward API）；路径 `/settings/outward-api`；设置 › **系统** 组，排在「存储」之后、「导入」之前。
- 代码：`web/src/features/settings-outward-api/`（`index.tsx` 注册、`SettingsOutwardApiPage.tsx` 由原 `OutputKeys.tsx` 搬来、`strings.ts`）；分组登记在 `web/src/app/registry.ts:SETTINGS_GROUPS`。
- 行为与上一轮折叠区一致：签发（用途：导出到文件夹 / 转发到外部系统；导出可带名称；备注）、列表（只列 `export*`、`forward`，标出已停用）、吊销、明文只在签发后显示一次，离开页面、点「我已保存」或再次签发即清除。API、权限、存储没动，不需迁移。
- 设置首页该项带摘要「N 把在用」。
- 「提醒渠道」页的折叠区已删；`/settings/channels` 其余不动，底部「用自己的程序接入」（渠道钥匙）行为不变。没有恢复 `settings-credentials` 旧路径或任何别名。

## 2. 文案前后对照

| 位置 | 之前 | 之后 |
|---|---|---|
| 对外接口 页标题 / 副标题 | （无此页；折叠区标题「导出与转发密钥」，说明「给导出到文件夹或向外部系统转发更新的程序发密钥。」） | 对外接口 / 外部程序来取你的事项、或接收转发，用这里的钥匙。 |
| 对外接口 页说明 | — | 程序由你自己写，APA 只提供接口和钥匙。 |
| 对外接口 页分界提示 | — | 要接收提醒并回答的程序，不在这里，去「设置 › 提醒渠道」。 |
| 提醒渠道 页副标题 | 提醒送到哪里，你在哪里回答。 | APA 把提醒推到这些渠道，你在渠道里回答。 |
| 提醒渠道 「给自己写的程序发钥匙」 | 三行说明（适用场景、签了不等于接通、与「添加渠道」的区别） | 同上，另加一行：想让程序来取事项或接转发？那是另一回事，去「设置 › 对外接口」。 |

英文文案同步（`strings.ts` 两处）。

## 3. 设计文档逐条改动

| 位置 | 改动 |
|---|---|
| §4.1 设置结构表 | 系统组：模型、登录与会话、存储、**对外接口**、导入、永久删除 |
| §4.1 提醒渠道正文 | 末尾加：本页只管「APA 把提醒推给外部并接收回答」，导出与转发密钥在「设置 › 对外接口」；折叠区说法已删 |
| §4.1 新增「设置 › 对外接口」段 | 替换原「独立折叠区」段：职责、用途与明文规则、与渠道页互相指向，以及「不与渠道混列」的理由（方向、权限范围、风险不同，挂在渠道下会被当成推送渠道） |
| 决定 53 | 「各类 ext 的钥匙都在提醒渠道页管理，导出与转发单独折叠」→「渠道 ext 的钥匙在提醒渠道页管理，导出与转发的钥匙在"对外接口"页（决定 77）」 |
| **§20.3 W25.4** | 交付列：内置输出就绪前导出与转发密钥由「对外接口」页签发、查看、吊销；就绪后对应 ext 下线，已签发密钥在接替页可见、可吊销，地址改写过去。**验收列恢复红线**：「本项就绪前导出与转发密钥保持可签发、可查看、可吊销（只是位置在"对外接口"页），"对外接口"页不单独删除、不做没有替代的下线」。上一轮删掉的是原文「本项就绪前"扩展程序"页不单独删除」，现按新位置的名字写回，意思不变 |
| **新增决定 77** | 记录这次位置变更与理由：渠道钥匙＝APA 推提醒并接收回答；导出与转发密钥＝外部来向 APA 要数据；曾放进折叠区易被当成推送渠道，故独立成页、不再合并回渠道页；红线不因位置变化放宽。§21.1 末尾 |
| 附录 B.6 | 「导出与转发密钥的 Web 入口」行改为：独立设置页（扩展程序）→ 设置 › 系统 › 对外接口；旧地址不保留别名；密钥、范围、API 不变，无需迁移 |
| `.omac/apa.yaml` | 节点 `web-settings-output-keys` 的标题、目标、`scope_paths`、验证命令改指新目录 |

## 4. 测试与构建

- `npm --prefix web run test -- --run`：**50 个文件，579 项通过**（原 576，新增：渠道页不再有导出与转发入口 1、英文分界文案 1、系统组新页地址 1；原 5 个密钥用例迁到新目录并改为整页挂载，「收起再展开」改为「离开页面再回来不显示明文」）。
- `npm --prefix web run build` 通过（仅既有的 chunk 体积提示）；`tsc --noEmit` 无输出。
- `go build ./...`、`go run ./tools/redline`（ok）、`go test ./web/... ./tests/conformance/... ./cli/serve/...` 通过。
- 未跑：全量 `go test ./...` 与 e2e（本次未动 Go 代码，只动文档对照项；e2e 按 CI 约定只在发版跑）。
- 全局搜索：「导出与转发」「outputKeys」「settings-credentials」在代码里只剩新页、两页的测试断言和文档说明；没有两处入口、没有悬挂引用（`.omac/apa.yaml` 中 `api-settings-credentials` 是后端路由节点，与此无关）。

## 5. 上线

| 项 | 结果 |
|---|---|
| 合并 | `~/app/apa/src`（detached HEAD，`7164adc7f`）`git merge --ff-only feat/outward-api-page` → `3d8a78b5c` |
| 构建 | `npm --prefix web run build`；`go build -o ~/.local/bin/apa ./cli/apa` |
| 重启 | `launchctl kickstart -k gui/$(id -u)/info.xiaohei.apa` |
| PID | 重启前 **48532** → 重启后 **83044** |
| `curl -I http://127.0.0.1:7777/` | `HTTP/1.1 200 OK`（重启前亦为 200） |
| 首页与构建产物 | 首页引用 `assets/index-LFkYdzmp.js`，与 `web/dist` 里的文件逐字节一致（`cmp`）；产物含 `settings/outward-api` 与「对外接口」 |

未登录实例做真实点击验收（签发 / 吊销是 Web 写操作，留给本人）。

## 6. 这个家是否语义正确

是。判据只有一个：谁发起。渠道钥匙是 APA 主动推提醒、外部把回答递回来；导出与转发密钥是外部程序来拉、来接，APA 只出接口和钥匙。放进「系统」组、与「提醒渠道」平级，符合本人的定位，也不再和推送渠道混列。两页的文案互相指向，决定 77 写明了不要合并回去。

遗留一处张力，请本人定：W25.4 原设计是内置输出就绪后对应 ext 下线、旧页由「导出与转发」配置页接替。而按「APA 提供对外接口，外部谁想接谁自己接」的定位，「来取数据」的接口与密钥可能是长期存在的，不止是过渡。这次保持原设计不动（只恢复红线、换位置）；若要让「对外接口」页长期保留，应另改 W25.4 与决定 77。

## 7. 定位定稿：长期对外能力

本人定 A 方案：「对外接口」是长期对外能力，内置输出与它并行、互不替代。代码未动（页面本来就是这个形态），只改设计文档与验收清单。提交 `2caaf5e40`。

### 逐处改动

| 位置 | 改动要点 |
|---|---|
| §20.3 W25.4 交付列 | 删去「内置输出就绪前继续使用对应 ext…就绪后对应 ext 下线…地址改写过去」整条过渡链；改为：内置输出是 APA 自己的推送与导出能力，与「对外接口」并行、互不替代；对外接口页、导出与转发 ext 及密钥是长期对外能力，不随本项上线而下线、退役或失效。内置输出的配置页另立，与对外接口页不混列（原写的「"导出与转发"配置页」与对外接口易混，已改称「内置输出的配置页」） |
| §20.3 W25.4 验收列 | 「本项就绪前…保持可签发」改为长期不变式：本项上线前后，导出与转发密钥都可签发、可查看、可吊销，已签发密钥与现有 ext 照常工作；对外接口页与其密钥不随内置输出上线而删除或失效，若要下线或迁移，必须有明确替代并经本人决定 |
| 决定 77 | 改名为「对外接口：导出与转发密钥的长期归宿」。去掉待定含义，写死长期保留：外部系统谁想接谁自己接，APA 只提供接口说明和密钥，不做对接方；与内置输出并行。理由新增：内置输出只解决「APA 自己把内容推出去」，不覆盖「别的系统自己来取」，后者的对接方是外部系统，所以接口与密钥不能以「内置输出就绪」为由收掉。并写明「不做没有替代的下线」是长期不变式，不是过渡措辞 |
| §4.1 对外接口段 | 新增一句：长期对外能力，不是过渡形态；内置输出与本页并行；本页与密钥若要下线或迁移，必须有明确替代并经本人决定。设置结构表只列页名，无需改 |
| §10.3（三类 ext 之后新增一段） | 「导出与转发是长期对外能力」：导出 ext、webhook 转发 ext 对应对外接口，随发行附带的是参考实现；内置输出与之并行，上线不使它们或密钥下线、失效 |
| §4.4 ext 说明（`docs/solution-design.md` 约 600 行） | 原「对应的内置连接插件就绪后它们下线」改为只对渠道 ext 成立；导出与事件转发 ext 长期随发行附带，不随内置输出下线 |
| §16.6 发布物表的 ext 行（约 3034 行） | 同上：只有渠道 ext 在内置渠道就绪后不再随发行附带 |
| 附录 B.6 | 「导出与转发密钥的 Web 入口」行补一句：长期对外能力，与内置输出并行，不随其上线而下线。B 里没有其他把这套描述成过渡的内容 |
| `.omac/apa.yaml` | 清单里没有引用 W25.4 的节点；与此相关的是 `web-settings-output-keys`，给它的 `non_goals` 加一条：不因内置输出上线而下线对外接口页、使密钥失效或退役对应 ext |

保留的保障：「不做没有替代的下线」仍在，理由从「内置输出未就绪」换成「这是长期对外能力」，并加了「经本人决定」这个门槛。

### 与别处设计原则有没有冲突

- **没有硬冲突**：不触红线（对外接口只经 HTTP API，密钥只在 Web 签发，内核不认识具体产品）；与 §2.2 非目标一致（导出单向，不做双向同步）；与决定 57 一致（它说的是渠道 ext 被内置渠道取代，导出与转发没被它覆盖）。`go run ./tools/redline` 与 `tests/conformance` 通过。
- **一处需要你知晓的张力**：定位说「APA 不做对接方」，而发行仍附带导出、转发两个 ext，且内置输出还会再做同类插件。我按「ext 是对外接口的参考实现，内置输出是 APA 自己的能力」写，不矛盾；但这意味着同一功能（Markdown 导出、webhook 转发）长期有两种实现。若觉得重复，将来可单独决定是否让参考实现停止随发行附带，那也属于「下线」，须按新不变式有替代并经你决定。
- **一处文字取舍**：W25.4 里原先的「"导出与转发"配置页」与对外接口页名字太像，我改称「内置输出的配置页」，具体页名等做 W25.4 时再定。

### 上线

`~/app/apa/src` 快进到 `2caaf5e40`；`npm --prefix web run build`、`go build -o ~/.local/bin/apa ./cli/apa`、`launchctl kickstart -k`。PID **83044 → 96667**；`curl -I http://127.0.0.1:7777/` 返回 `HTTP/1.1 200 OK`；首页引用的 `assets/index-LFkYdzmp.js` 与 `web/dist` 逐字节一致（代码没变，产物哈希同上一次）。未 push。

## 8. W25.4 标记取消

本人定：内置输出取消，条款保留并标为已取消。只改设计文档与 `.omac`，代码未动。提交 `56532475b`。

### 逐处改动

| 位置 | 改动要点 |
|---|---|
| §20.3 W25.4 | 行保留，名称改为「W25.4 内置输出（已取消，2026-11-19）」；正文一句话理由：对外接口加随发行附带的导出 ext、webhook 转发 ext 已完整覆盖「导出到哪、转发给谁」，再内置一套同类实现会长期维护两份代码；原范围一并取消；验收列写「不适用」，只留「对外接口页与密钥始终可签发、可查看、可吊销，下线或迁移须有明确替代并经本人决定」这条保障（上一轮恢复的红线，没有丢） |
| 决定 77 | 去掉「与内置输出并行」这层依赖，只讲：对外接口是长期对外能力，APA 不做对接方，两个 ext 是可直接使用的客户端也可自己写；该页与密钥不随任何内置能力下线，下线或迁移须有替代并经本人决定；「不做没有替代的下线」是长期不变式 |
| 新增决定 78 | 取消内置输出：不做输出接口、推送器与两个内置插件，导出与转发走对外接口加 ext；W25.4 保留并标为已取消；连接接口的「输出」方向目前没有内置类型 |
| §4.1 对外接口段 | 「与内置输出并行」改为：随发行附带的两个 ext 是现成客户端，APA 不再内置同类输出（W25.4 已取消，决定 78），本页不会被内置能力替代 |
| §4.4 ext 说明 | 渠道 ext 仍在内置渠道就绪后下线；导出、事件转发 ext 是随发行附带、可直接使用的客户端，长期保留。去掉「参考实现」 |
| §10.3 | 同上，并注明 APA 不内置同类输出（已取消） |
| §16.6 发布物表 | 导出、转发 ext 长期随发行附带，是对外接口的现成客户端；去掉「不随内置输出下线」 |
| 附录 B.6 | 入口行改为「长期对外能力；内置输出已取消（决定 78）」 |
| 附录 B（`apa connection list` 行） | 「（内置渠道与输出）」改为「（内置渠道；输出方向目前没有内置类型）」，与 CLI 实际行为一致 |
| 其余暗示内置输出存在的说法 | §4 产品形态的连接模块行（约 474）、§7.6 连接登记行、§9.6（约 1976）、§20.2（约 3273）、§14.2 连接凭据行（约 2529）、§5.3 扩展层行（约 644）、决定 57：去掉「输出」，或写明内置输出已取消 |
| `.omac/apa.yaml` | `web-settings-output-keys` 的 non_goals 保留，改注：对外接口是长期对外能力，原内置输出里程碑 W25.4 已取消（2026-11-19，决定 78） |

两个 ext 的定位：随发行附带、可直接使用的客户端，也可以自己写程序接对外接口；文中不再有「参考实现」。

### 残留检查

- `内置输出`：全文剩 6 处，都带「已取消」或明确说没有内置输出（§4 连接行、§20.2、W25.4 行、决定 57、决定 78、附录 B.6）。`.omac` 一处注明 W25.4 已取消。
- `并行、互不替代`、`与内置输出并行`、`参考实现`：全仓库 0 处。
- `docs/`、`AGENTS.md`、`README.md`、`ext/`、`connectors/`、`cli/` 里没有别处把内置输出写成未来能力。
- `go run ./tools/redline` 与 `tests/conformance`、`cli/connections` 通过。

### 有意保留、请你知晓

连接接口本身仍有「输出」方向：内核代码（`kernel/outcall/conn` 的 `DirectionOutput`）、`apa connection list --direction channel|output`，以及 §5.2 原则 2 与「只送达本人」条款、§14（约 2509）与 `AGENTS.md` 架构红线里的「把账本变化写到本人配置的输出」。这些是内核对外边界的通用写法，没有承诺任何内置类型，我没动，也没动代码和 `AGENTS.md`（红线文本属手工维护）。若想彻底收掉输出方向，需要另做一次改动：删接口方向与 CLI 取值，并改这几处红线文字；要不要做，请你定。

### 上线

`~/app/apa/src` 快进到 `56532475b`；`npm --prefix web run build`、`go build -o ~/.local/bin/apa ./cli/apa`、`launchctl kickstart -k`。PID **96667 → 26213**；`curl -I http://127.0.0.1:7777/` 返回 `HTTP/1.1 200 OK`；首页引用的 `assets/index-LFkYdzmp.js` 与 `web/dist` 逐字节一致（前端代码未变）。未 push。

## 9. 收掉输出方向与删除附带 ext

本人定：连接接口的「输出」方向作为死代码删除；`ext/export` 与 `ext/webhookforward` 整目录删除；一起上线。提交 `a772adecf`。

### 一、输出方向

**先查数据（只读）**：对运行中的 `~/.apa/ledger.db` 用 `sqlite3 -readonly` 查询：`connections_connection` 里 `direction` 只有 `channel` 一行（1 条），`id LIKE 'out-%'` 为 0；`eventlog_event` 里 `connection.*` 事件共 3 条（created / enabled / disabled），载荷里没有 `"direction":"output"` 或 `out-` 前缀。连接目录里只有一种连接类型，方向为 `channel`（`connectors/bark/connector.yaml`），没有任何输出类型能被登记过。**结论：没有历史数据使用 output 方向，无需停下，也没有迁移。**`direction` 列保持原样（迁移只增不改）。

**枚举与 CLI**

| 位置 | 改动 |
|---|---|
| `kernel/outcall/conn/conn.go` | 删除 `DirectionOutput`；常量组改为单个 `DirectionChannel` |
| `kernel/outcall/conn/decl.go:Decl.Check` | 方向校验只认 `DirectionChannel` |
| `kernel/connections/connections.go:newID` | 去掉 `direction` 参数与 `out-` 前缀分支，调用处同步 |
| `cli/connections/connections.go:list` | 用法提示改为 `[--direction channel]`；`--direction` 取其他值（含 `output`）返回用法错误（退出码 2）。`cli/connections/connections_test.go` 加了 `--direction output` 的用例 |
| 注释 | `kernel/connections`、`api/connections`、`connectors` 包注释去掉「输出」 |
| `docs/appendix-b-api-cli.md` | `apa connection list` 与 `GET /connections` 两行去掉 output；B.6 新增一行：取值收窄、`?direction=output` 返回空列表、无数据迁移 |
| `docs/user/changelog.md` | 新增迁移项 M-21 |

**设计文档**：§5.2 原则 2 的「连接接口（来源拉取、渠道发送、输出写出）」改为「（来源拉取、渠道发送）」；「只送达本人」条款从「两种动作：送渠道、写输出」改为「只有一种：把提醒送到本人配置的渠道；账本变化只经对外接口（事件订阅与只读 API）由对方来取」；§14.2 同步；决定 78 写明「输出」方向（枚举值与 CLI 取值）一并删除。

**AGENTS.md 红线文字变更（用户已批准）**，`AGENTS.md` 第 30 行「内核只经连接接口对外」，只改了第一句：

改动前：

> - **内核只经连接接口对外。** 内核可以主动把提醒送到本人配置的渠道、把账本变化写到本人配置的输出，但只经外呼调用器、按统一的连接接口调用；接了哪些渠道、渠道怎么用，内核不管。连接插件在 `connectors/`，内核不 import 它；厂商名只出现在插件里。接口上没有收件人参数，规则（何时、送哪、内容、能否回答）全部在调用前由内核判定；插件拿不到账本，只拿到这一次调用的内容、本连接的配置和本连接声明过的凭据。以本人名义对第三方行事（发信、评论、付款）不属于内核与连接插件。连接的写命令只在 Web；读连接现状对 observer 开放，但不含配置值与凭据。凭据密文进账本，明文只在外呼调用器里解出。自写程序经 ext 协议接入：ext 是独立程序，只经 HTTP 接入，持独立密钥。渠道（内置或 ext）不复用任何 harness 的网关，也不依赖任何 harness。

改动后：

> - **内核只经连接接口对外。** 内核可以主动把提醒送到本人配置的渠道；账本变化只经对外接口读取，或送到本人配置的渠道。送到渠道只经外呼调用器、按统一的连接接口调用；接了哪些渠道、渠道怎么用，内核不管。连接插件在 `connectors/`，内核不 import 它；厂商名只出现在插件里。接口上没有收件人参数，规则（何时、送哪、内容、能否回答）全部在调用前由内核判定；插件拿不到账本，只拿到这一次调用的内容、本连接的配置和本连接声明过的凭据。以本人名义对第三方行事（发信、评论、付款）不属于内核与连接插件。连接的写命令只在 Web；读连接现状对 observer 开放，但不含配置值与凭据。凭据密文进账本，明文只在外呼调用器里解出。自写程序经 ext 协议接入：ext 是独立程序，只经 HTTP 接入，持独立密钥。渠道（内置或 ext）不复用任何 harness 的网关，也不依赖任何 harness。

其余句子逐字未动。

### 二、删除两个 ext

**删除**：`ext/export`（9 个文件：README、config、export、export_test、ext.yaml、flow_test、main、render、state）、`ext/webhookforward`（4 个文件：ext.yaml、forward、forward_test、main），共 **13 个文件**。`ext/telegram` 未动，`ext/` 下只剩它。同一提交里另删 7 个文件：`tests/e2e/events/export_test.go`、`export_v2_test.go`、`tests/e2e/proposals/ext_test.go`，以及 `.omac` 里 4 份已失去对象的节点状态文件。全提交合计 50 个文件、约 4.5 千行净删除。

**引用清扫**

| 位置 | 处理 |
|---|---|
| `tools/release/pack.go` | 必备文件清单去掉两个 ext 的 `ext.yaml`；包内验收原来跑 `apa ext run export`，改为：签发一把导出密钥并用它读 `GET /v1/events`，要求 200（不再依赖附带客户端）；同步 `pack_test.go` |
| `tests/release/release_flow_test.go` | ACT-REL-06 清单去掉两个 ext；ACT-REL-07 同样改为用导出密钥读事件流 |
| `tests/e2e/events` | 删两个依赖 ext 的测试文件；保留导出密钥只读的权限用例，改为不依赖 ext 的 `outward_key_test.go:TestOutwardExportKeyReadOnly`（写事项与 ack 被拒） |
| `tests/e2e/proposals` | 删 `forwardOnlyCreated` 与 ext 测试文件；`ACT-PRH-18` 保留事件订阅断言，去掉「随包订阅方」部分；`judgeYes` 挪进 `helpers_test.go` |
| `tests/release/closeout-evidence.json` | 去掉 `flow-export-ext` 条目（`TestCloseoutEvidenceIsTraceable` 要求与验收文档一致） |
| `.gitignore` | 去掉 `/ext/export/export` |
| `.omac` | `apa.yaml`、`apa-v02.yaml` 删节点 `ext-export`、`ext-webhook-forward`、`export-ext-v2`、`e2e-export` 及所有 blocked_by、consumes、acceptance 引用；`apa.acceptance.yaml`、`apa-v02.acceptance.yaml` 删 `flow-export-ext` 整流程；保留的节点 `action-links`、`e2e-events-export`、`api-deliveries` 改写描述与命令（去掉 `go test ./ext/export/...`）；`apa-v02.design/OMAC-NOTES.md` 标注已移除；删节点状态文件。用 PyYAML（含重复键检查）解析四份文件：`apa.yaml` 206 个节点、`apa-v02.yaml` 120 个，没有悬挂的 `blocked_by` 或 `producer` |
| `docs/solution-design.md` | §4.1、§4.4、§10.3（原「Markdown 目录导出」整段配置表与手改检测规则换成「自写导出与转发程序的约束」四条：导出范围、单向、动作链接不含凭据、事件订阅语义）、§10.4、§16.6、§14.2 数据流向表、M6、W11、W25.4、风险 31、决定 77、78 改写，新增**决定 79**；凡「随发行附带导出 ext、webhook 转发 ext」改为「不再附带，需自行实现」；`docs/appendix-d-adapters.md` D.1 删两行；`docs/user/README.md`、`run-tiers.md`（与 §2.4 保持一致）同步 |
| 保留 | 对外接口页、导出与转发密钥、事件订阅与只读 API、ext 协议、`apa ext check`、`ext/telegram` 及其 conformance 测试 |

**清扫命令与命中数**

```sh
grep -rIEn 'ext/export|ext/webhookforward|webhookforward|ext run export|APA_EXPORT_DIR|APA_WEBHOOK_|flow-export-ext|ACT-EXP-(0[0-9]|1[0-9])|e2e-export|ext-export|ext-webhook-forward|export-ext-v2|DirectionOutput' \
  --exclude-dir=node_modules --exclude-dir=.git --exclude-dir=dist --exclude-dir=reports --exclude=BRIEF-OUTWARD-API.md .
```

最终命中 **4 处**，都是有意保留的说明或历史记录，不指向存在的路径：`docs/solution-design.md` 决定 79（写明删除了这两个目录）、`.omac/apa-v02.design/OMAC-NOTES.md`（标注「已移除」）、`tests/release/evidence/go-test-all-linux.txt` 两行（留存的历史测试输出，不改写）。清扫前同一命令命中数百处（仅 `.omac` 里就有 105 行）。

**没有改的**：`docs/diagrams/architecture.html` 与 `.png` 里的「导出扩展」「Webhook 转发扩展」两个方框表示「自己写的 ext 的类别」，不是附带程序，没有动（改图要成对重绘 HTML 与 PNG）。如果你希望图上也去掉，需要另做。`kernel/auth` 等处代码注释里的「导出 ext 密钥」指密钥种类，保留。

### 三、构建与测试

- `go build ./...`、`go vet ./...`、`go run ./tools/redline`：通过（redline: ok）。
- `go test ./...`：全部通过，除两项与本次改动无关：`kernel/sources/runner` 的 `TestTimeoutIsTransientFailure` 在并行全量跑时偶发失败（单独跑与全包重跑均通过）；`tests/doubles/stubharness` 的 `TestPromptHangChildStaysAliveAndNeverReceipts` 读 `/proc`，本机（macOS）上本来就不通过，与本次无关。初次全量运行中暴露的 `tests/release`（`closeout-evidence.json` 与 `run-tiers.md` 两处）已修正，修正后 `tests/release`、`tests/conformance`、`tools/...`、`cli/...`、`api/...` 全部通过；`ext/telegram` 的 conformance 随 `./...` 通过。
- `go vet -tags e2e ./tests/e2e/...` 通过；`go test -tags e2e ./tests/e2e/events/`（5.9s）与 `./tests/e2e/proposals/`（19.3s，`-run Handling|ACT-PRH`）通过；`.github/e2e/pkgs.sh check` 通过（包清单仍完整）。
- 前端：`npm --prefix web run test -- --run` 50 个文件、579 项通过；`npm --prefix web run build` 通过。

### 四、上线

`~/app/apa/src` 快进到 `a772adecf`（`ext/` 下只剩 telegram）；`npm --prefix web run build`、`go build -o ~/.local/bin/apa ./cli/apa`、`launchctl kickstart -k gui/$(id -u)/info.xiaohei.apa`。PID **26213 → 59040**；`curl -I http://127.0.0.1:7777/` 返回 `HTTP/1.1 200 OK`；首页引用的 `assets/index-LFkYdzmp.js` 与 `web/dist` 逐字节一致（前端代码未变）。上线后 `apa connection list --direction output` 返回 `direction must be channel` 与用法提示；运行中的账本仍是 1 条 `channel` 连接。未 push。

### 五、需要你知晓

- 发行包里不再有导出与转发客户端；之前若有人在本机自己起过 `apa ext run export` 或 `webhookforward`，那是从仓库的 `ext/` 目录或旧发行包启动的，新二进制不再能找到它们（`apa ext run export` 会失败）。对外接口页签发的密钥和事件订阅接口不变，旧程序若已在别处运行仍可使用。
- 全文已没有任何地方暗示内置输出或附带客户端将来会上线（设计文档里相关的「内置输出」仅 6 处，全带「已取消」或明确说没有）。
