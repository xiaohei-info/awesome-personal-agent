# awesome-personal-agent（APA）

APA 是一个自部署的个人事务中台：一本账本、一套命令模型、一个时钟、一个 Web 中台和一个 agent native CLI。任何 agent harness 都能接入，执行留在 harness，状态留在账本。

*A self-hosted personal operations hub: one ledger for items, projects and materials that any agent harness can plug into. Execution stays in the harness; state stays in the ledger.*

## 状态

设计已定稿，尚无代码。

## 核心理念

1. **账本是真理源，Web 是首要视图。** 事项与跟着项目走的材料都进账本，任意端看到同一份状态。
2. **内核只改账本、只提供接口。** 外部副作用留在 harness 和 ext 各自的权限体系内；内核有且只有一个外呼调用器，不主动推送。
3. **判断交专属 agent，采集交程序。** 规则优先、模型兜底；需要本人决定的事，由内核保证能挂起、能问到、能追溯。
4. **主动但默认安静。** 自动规则默认只观察，切换只在 Web；高风险决策只能由本人在 Web 确认。
5. **扩展不动内核。** 新增自动化写能力包，新增渠道写 ext，接入 harness 写插件；不做 agent loop，不管额度，不寄生记忆。

![APA 系统架构](docs/diagrams/architecture.png)

## 文档

| 文档 | 内容 |
|---|---|
| [文档索引](docs/README.md) | 阅读顺序与目录约定 |
| [业务解决方案设计](docs/solution-design.md) | 主设计文档，也是实现时的权威依据 |
| [附录 A：harness 接口与无头运行](docs/appendix-a-harness.md) | 插件约定、无头运行声明、钩子全表 |
| [附录 B：API 与 CLI 清单](docs/appendix-b-api-cli.md) | CLI 命令、HTTP 路由、错误响应 |
| [附录 C：能力包规范](docs/appendix-c-capability-pack.md) | `pack.yaml` 字段、枚举动作、示例 |
| [AGENTS.md](AGENTS.md) | 给在本仓库工作的 coding agent 的规则 |

## 许可

[MIT](LICENSE)
