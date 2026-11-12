# 文档索引

APA 是架在 agent harness 之上的开源、自部署个人 agent 层，它让只在被调用时才动的 harness 具备主观能动性，成为真正的个人助手，而不是对话工具或开发工具。

APA 的设计已定稿，尚无代码。首版支持 Hermes，接口按多 harness 设计，其他 harness 在 Hermes 端到端验证完成后接入（§20）。权威顺序：`docs/` 设计文档 > [`AGENTS.md`](../AGENTS.md) > CI 与代码 > [`README.md`](../README.md)。

## 文档清单

| 文档 | 内容 | 什么时候读 |
|---|---|---|
| [solution-design.md](solution-design.md) | 主设计文档：为什么做（定位、背景、问题、价值）、目标、场景、架构、领域模型、模块详设、安全、与商业产品的对照、路线、已定决定与风险 | 必读 |
| [appendix-a-harness.md](appendix-a-harness.md) | 插件约定、无头运行声明、各 harness 的标志与退出码、钩子全表、dsh 接入面（后续参考） | 开发 harness 插件时 |
| [appendix-b-api-cli.md](appendix-b-api-cli.md) | CLI 逐条清单、HTTP 路由、错误响应、标准事件类型 | 开发内核、CLI、Web、ext 时 |
| [appendix-c-capability-pack.md](appendix-c-capability-pack.md) | `pack.yaml` 字段、枚举动作、提示词约定、校验规则、示例 | 编写或加载能力包时 |

## 图

每张图都有可交互的 HTML 和渲染好的 PNG。

| 图 | 文件 | 正文位置 |
|---|---|---|
| 系统架构 | [HTML](diagrams/architecture.html) · [PNG](diagrams/architecture.png) | §5.1 |
| 领域模型 | [HTML](diagrams/domain-model.html) · [PNG](diagrams/domain-model.png) | §6.1 |
| 事项生命周期 | [HTML](diagrams/item-lifecycle.html) · [PNG](diagrams/item-lifecycle.png) | §6.4 |
| 入站流水线 | [HTML](diagrams/inbound-pipeline.html) · [PNG](diagrams/inbound-pipeline.png) | §9 |
| 后台执行 | [HTML](diagrams/background-execution.html) · [PNG](diagrams/background-execution.png) | §13.6 |

## 界面截图

[`screenshots/`](screenshots/) 存放 README 引用的 Web 中台截图，取自临时实例的演示数据。

## 阅读顺序

| 读者 | 建议顺序 |
|---|---|
| 初次了解 APA 的读者 | §1–§4 → §19.5 |
| 评审者 | §1–§5 → §13.6 → §14 → §20–§21 |
| 内核与 Web 开发者 | §5 → §6–§10 → §12–§15 → §17 → 附录 B、C |
| harness 插件开发者 | §5 → §6.13 → §11 → §13.6 → §14 → 附录 A |
| ext 与 Source 开发者 | §5 → §9 → §10 → §17 → 附录 B |

## 目录约定

- 主文档章节编号保持稳定，其他文档用 `§` 引用。
- 新增文档必须登记到本页；附录数量保持克制。
- `diagrams/<名称>.html` 与同名 `.png` 成对存在，一起更新。
- 文档写当前设计，用现在时；已定决定写回相应章节，主文档 §21.1 只留结论与理由。
- 改行为时的同步要求见 [`AGENTS.md`](../AGENTS.md)。
