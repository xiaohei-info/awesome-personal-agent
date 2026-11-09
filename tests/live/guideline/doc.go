// Package guideline 是 flow-agents-md、flow-no-repo-guideline、flow-guideline-draft（设计 §12.5、§12.6）的真实依赖验收。
//
// 真实：apa 二进制与内核、SQLite 账本、Web 会话接口、真实 Hermes（任务型起草、主会话）、真实 GitHub 专用测试仓库
// （APA_E2E_TEST_REPO，缺省 xiaohei-info/apa-live-test）与随发行附带的 github-source、本机 git。
// 除验收文档规定的 MODEL_STUB（承载内核的判断型/生成型调用）外没有替身。
// 另一仓库（ACT-AGM-08）与 ACT-DRF 的仓库用本机 bare 仓库作远程：它们不依赖 GitHub，git 行为与真实远程一致。
//
// 运行方式见 README.md；只在 `-tags=live` 下编译，缺少真实依赖时失败而不是跳过。
package guideline
