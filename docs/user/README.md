# APA 用户文档

适用版本：v0.2.0。这些文档随发布物交付，与 `docs/` 下的设计文档分开：设计文档说明系统为什么这样做，这里只说明怎么装、怎么用、坏了怎么办。

| 文档 | 内容 | 什么时候读 |
|---|---|---|
| [quickstart.md](quickstart.md) | 安装、初始化、启动、第一次看到价值 | 第一次使用 |
| [run-tiers.md](run-tiers.md) | 三档运行形态：只有内核、加模型端点、加首批宿主，各自可用与不可用的能力 | 想知道手里有什么就能用什么 |
| [troubleshooting.md](troubleshooting.md) | 用 `apa doctor` 定位问题，常见症状与处理 | 装不上、跑不起来、没有提醒 |
| [security.md](security.md) | 远程访问、登录与会话、外发范围、凭据与备份的保管 | 把 Web 暴露到本机之外之前 |
| [upgrade.md](upgrade.md) | 备份、升级、回退，升级后本机验证清单 L1–L12 | 升级已有部署 |
| [uninstall.md](uninstall.md) | 干净地卸载 | 不再使用时 |
| [changelog.md](changelog.md) | 各版本变更，含界面语言限制与需要迁移的变更 | 升级之前 |

发布包里的二进制与配置样例见发布物清单（设计文档 §16.6）：主程序 `apa`、来源程序与工具适配器、渠道/导出/webhook 转发 ext、首批宿主插件包、`deploy/` 下的部署样例和每个文件的哈希清单。用 `apa version` 查看版本号、构建提交、各数据结构版本与协议版本。
