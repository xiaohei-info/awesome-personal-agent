import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "改动",
  onBehalfOf: (p: string) => `代 ${p}`,
  viaChat: "凭你在聊天里的原话",
  viaGrant: "按授权",
  runDid: (what: string, title: string) => `Agent 后台运行替你${what}「${title}」`,
  viaAgentGrant: (name: string) => `按你给 ${name} 的授权`,
  reverted: " · 已撤销",
  revert: "撤销",
  revertWhat: "只撤销 APA 里的改动，已发出的消息、已提的 PR 不会收回。",
  confirmRevert: "确认撤销",
  showOlder: "显示更早的改动",
  empty: "Agent 还没改过什么",
  draftFailed: "起草建议失败",
  byApa: "APA 自己",
  draftFailedWhy: "APA 调模型起草时出错，不是 Agent 后台运行；事项已按来源标题建好",
};

const en: typeof zh = {
  ...common.en,
  title: "Changes",
  onBehalfOf: (p) => `on behalf of ${p}`,
  viaChat: "on your words in chat",
  viaGrant: "under a temporary grant",
  runDid: (what: string, title: string) => `An Agent background run ${what} for you: "${title}"`,
  viaAgentGrant: (name: string) => `under the grant you gave ${name}`,
  reverted: " · undone",
  revert: "Undo",
  revertWhat: "Only undoes the change inside APA; messages already sent and PRs already opened stay.",
  confirmRevert: "Confirm undo",
  showOlder: "Show older changes",
  empty: "No Agent changes yet",
  draftFailed: "Drafting a suggestion failed",
  byApa: "APA itself",
  draftFailedWhy: "APA's call to the model failed; this is not an Agent background run. The item was still created from the source title",
};

export const strings: Dict<typeof zh> = { zh, en };
