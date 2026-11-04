# awesome-personal-agent（APA）

APA 是架在 agent harness 之上的开源、自部署个人 agent 层，它让只在被调用时才工作的 harness 能主动感知、主动跟进、了解用户，使人的注意力回到自己真正需要关注的事情上。

*APA is an open-source, self-hosted personal agent layer on top of an agent harness. It lets a tool that only works when called notice what is happening, follow up on its own and understand your projects, tasks and decisions, so your attention goes back to the things that actually need it. The first release supports Hermes; the interfaces are designed for multiple harnesses.*

## 状态

设计已定稿，尚无代码。首版支持 Hermes，接口按多 harness 设计；dsh、Claude Code、Codex CLI 及其他 harness 在 Hermes 端到端验证完成后接入（[设计文档 §20](docs/solution-design.md#20-开发分阶段建议)）。

## 为什么做

harness 已经很强，但它是被动的工具：被调用才工作，会话之间不保留事项状态，也不知道用户在等什么。于是人成了 agent 之间的调度器和传话筒：事项散在各个会话和渠道，跟进靠人记，几个 session 同时等人决定，同样的上下文一遍遍重复输入。

Grok、Meta 的 Muse 这类商业个人 agent 把主动式体验做成了产品，但运行环境、模型和入口都由厂商决定。APA 面向已经在用 harness 的人：不换工具链和模型，在自己的 harness 上获得类似的能力。首版只支持 Hermes，在用其他 harness 的人要等相应插件接入。

## APA 提供什么

- **主动感知**：定时拉取或接收邮件、日历、Issue 等外部事件，筛过之后只把值得关注的变成建议。
- **主动跟进**：时钟在内核，委派出去的工作、等别人的回复、到期的事都由系统跟踪；本人批准规则后，后台运行可以接着处理。
- **了解用户**：了解的是工作现场。账本记着本人在跟的事、承诺和决定，项目带材料和规范，接受和丢弃建议的记录反映本人的取舍，新会话不用重述背景。这些都是本人可查、可改、可撤销的记录；个人长期记忆仍由 harness 负责，APA 不重复保存。
- **一个工作台**：工作、生活和日常事务进同一本账本，Web、CLI、已接入的 harness 和渠道看到同一份状态。
- **注意力回归**：需要决定的事进同一个待办流，一次一个具体问题；打扰按静默、合并、阈值控制。

## 设计原则

APA 由一本账本、一套命令模型、一个时钟、一个 Web 中台和一个 agent native CLI 组成。首版支持 Hermes，接口按多 harness 设计。

1. **执行留在 harness，状态留在账本。** APA 是 harness 之上的一层，不是又一个 agent：不做 agent loop，不管额度，不寄生记忆。
2. **账本是真理源，Web 是首要视图。** 事项与跟着项目走的材料都进账本，任意端看到同一份状态。
3. **内核只改账本、只提供接口。** 外部副作用留在 harness 和 ext 各自的权限体系内；内核有且只有一个外呼调用器，不主动推送。
4. **判断交专属 agent，采集交程序。** 规则优先、模型兜底；需要本人决定的事，由内核保证能挂起、能问到、能追溯。
5. **主动但默认安静。** 自动规则默认只观察，切换只在 Web；高风险决策只能由本人在 Web 确认。
6. **扩展不动内核。** 新增自动化写能力包，新增渠道写 ext，接入 harness 写插件。
7. **外部对接优先复用现成工具，并放在通用契约之后。** 邮件、日历等来源和渠道能用成熟的 CLI 或库就不自实现，协议和鉴权留给工具。工具可替换、不唯一；来源、渠道、触达、账本不依赖某一个 harness 或某一个具体工具才能工作，渠道 ext 是 APA 自己的独立进程，不复用 harness 的网关。

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
