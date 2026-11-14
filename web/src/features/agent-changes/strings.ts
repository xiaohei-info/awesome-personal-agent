import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "改动",
  onBehalfOf: (p: string) => `代 ${p}`,
  viaChat: "凭你在聊天里的原话",
  viaGrant: "按临时授权",
  reverted: " · 已撤销",
  revert: "撤销",
  revertWhat: "只撤销 APA 里的改动，已发出的消息、已提的 PR 不会收回。",
  confirmRevert: "确认撤销",
  showOlder: "显示更早的改动",
  empty: "agent 还没改过什么",
};

const en: typeof zh = {
  ...common.en,
  title: "Changes",
  onBehalfOf: (p) => `on behalf of ${p}`,
  viaChat: "on your words in chat",
  viaGrant: "under a temporary grant",
  reverted: " · undone",
  revert: "Undo",
  revertWhat: "Only undoes the change inside APA; messages already sent and PRs already opened stay.",
  confirmRevert: "Confirm undo",
  showOlder: "Show older changes",
  empty: "No agent changes yet",
};

export const strings: Dict<typeof zh> = { zh, en };
