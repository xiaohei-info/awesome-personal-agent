// Package injection 是 flow-data-not-instructions（设计 §14.10 资料不是指令）的真实依赖验收（ACT-INJ-01..05）。
//
// 真实：apa 二进制、内核、SQLite 账本、Web 设置接口、Hermes（任务型、后台运行与主会话都由真实 Hermes 执行，
// 经 plugins/hermes 的 apa-core、apa-tools、apa-headless 插件接入）。唯一的替身是 MODEL_STUB：验收文档要求它来
// 记录判断型的完整请求（ACT-INJ-01），它只承载内核的判断型调用，不替代 Hermes。
//
// 运行方式见 README.md；只在 `-tags=live` 下编译，缺少真实 Hermes 时失败而不是跳过。
package injection
