# 文档索引

APA 是架在 agent harness 之上的开源、自部署个人 agent 层，它让只在被调用时才动的 harness 具备主观能动性，成为真正的个人助手，而不是对话工具或开发工具。

v0.1 已实现并在主干运行；这里是 v0.2.0 的设计，也是 v0.2.0 实现与验收的依据。本版支持一个宿主，接口按多 harness 设计，第二个宿主是本版之后的第一优先（§20）。主文档不写厂商和产品的名字，被适配的对象见附录 A 与附录 D。权威顺序：`docs/` 设计文档 > [`AGENTS.md`](../AGENTS.md) > CI 与代码 > [`README.md`](../README.md)。

## 文档清单

| 文档 | 内容 | 什么时候读 |
|---|---|---|
| [solution-design.md](solution-design.md) | 主设计文档：本修订采用的决定、为什么做（定位、背景、问题、价值）、目标与承诺、场景、架构、领域模型、模块详设、判断分层与概率判断协议、安全、同类对照、版本路线、已定决定与风险 | 必读 |
| [appendix-a-harness.md](appendix-a-harness.md) | 插件约定、无头运行声明、各 harness 的标志与退出码、钩子全表、首批宿主的实测结论、其余 harness 的调研结论 | 开发 harness 插件时 |
| [appendix-b-api-cli.md](appendix-b-api-cli.md) | CLI 逐条清单、HTTP 路由、错误响应、标准事件类型、协议版本、v0.2.0 对已有契约的改动 | 开发内核、CLI、Web、ext 时 |
| [appendix-c-capability-pack.md](appendix-c-capability-pack.md) | `pack.yaml` 字段、来源规则的出口与字段条件、枚举动作、提示词模板字段、校验规则与 v0.1 包的兼容、示例 | 编写或加载能力包时 |
| [appendix-d-adapters.md](appendix-d-adapters.md) | 本版适配的对象（宿主、渠道、来源工具、校验方案）、各工具的专属约定与实测结论、模型端点的实测写回位置 | 接入或更换来源工具、渠道时 |
| [changes-v0.2.0.md](changes-v0.2.0.md) | v0.2.0 相对 v0.1 设计的变更清单：契约与写入路径变更、新增能力、默认值变更、删除或收窄，逐条的迁移方式，与评审结论不一致之处，设计确认时的决定 | 评审 v0.2.0 改了什么、升级已有部署时 |

## 用户文档

[`user/`](user/README.md) 是随发布物交付的用户文档：快速开始、三档运行形态、故障排查、安全说明、升级说明、卸载说明与变更记录。它与设计文档分开，写给装机和使用的人。

## 图

每张图都有可交互的 HTML 和渲染好的 PNG。

| 图 | 文件 | 正文位置 |
|---|---|---|
| 系统架构 | [HTML](diagrams/architecture.html) · [PNG](diagrams/architecture.png) | §5.1 |
| 领域模型 | [HTML](diagrams/domain-model.html) · [PNG](diagrams/domain-model.png) | §6.1 |
| 事项生命周期 | [HTML](diagrams/item-lifecycle.html) · [PNG](diagrams/item-lifecycle.png) | §6.4 |
| 判断分层 | [HTML](diagrams/judgment-layers.html) · [PNG](diagrams/judgment-layers.png) | §8.1 |
| 入站流水线 | [HTML](diagrams/inbound-pipeline.html) · [PNG](diagrams/inbound-pipeline.png) | §9 |
| 后台执行 | [HTML](diagrams/background-execution.html) · [PNG](diagrams/background-execution.png) | §13.6 |

## 界面截图

[`screenshots/`](screenshots/) 存放 README 引用的 Web 中台截图，取自临时实例的演示数据。

## 阅读顺序

| 读者 | 建议顺序 |
|---|---|
| 初次了解 APA 的读者 | §1–§4 → §19.5 |
| 评审者 | 主文档开头的决定表 → §1–§5 → §8 → §9.3 → §14 → §20–§21 |
| 内核与 Web 开发者 | §5 → §6–§10 → §12–§15 → §17 → 附录 B、C |
| harness 插件开发者 | §5 → §6.13 → §11 → §13.6 → §14 → 附录 A |
| ext 与 Source 开发者 | §5 → §9 → §10 → §17 → 附录 B、D |

## 目录约定

- 主文档章节编号保持稳定，其他文档用 `§` 引用。
- 新增文档必须登记到本页；附录数量保持克制。
- 主文档不写厂商和商业产品名，用类别称呼；名字只出现在附录 A、附录 D 和各自的适配目录。
- 标"需实测"的结论先在主文档 §21.3 登记编号和证实方。
- 用户文档（快速开始、三档运行形态、故障排查、安全说明、升级说明、变更记录）放在 `docs/user/`，随发布物交付（§16.6），不属于设计文档。
- `diagrams/<名称>.html` 与同名 `.png` 成对存在，一起更新。
- 文档写当前设计，用现在时；已定决定写回相应章节，主文档 §21.1 只留结论与理由。
- 改行为时的同步要求见 [`AGENTS.md`](../AGENTS.md)。
