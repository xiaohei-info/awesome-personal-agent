import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "agent 变更",
  lede: "撤销只会写一条反向命令恢复账本状态，原记录保留；外部已发生的副作用不会被撤销。",
  onBehalfOf: (p: string) => `代 ${p}`,
  viaChat: "聊天代答",
  viaGrant: (g: string) => `授权单代答 · 授权单 ${g}`,
  reverted: " · 已撤销",
  revert: "撤销",
  showOlder: "显示更早的变更",
  empty: "没有 agent 变更",
};

const en: typeof zh = {
  ...common.en,
  title: "Agent changes",
  lede: "Undo only writes a reverse command that restores the ledger state; the original record stays, and side effects that already happened outside are not undone.",
  onBehalfOf: (p) => `on behalf of ${p}`,
  viaChat: "answered in chat",
  viaGrant: (g) => `answered under grant ${g}`,
  reverted: " · undone",
  revert: "Undo",
  showOlder: "Show older changes",
  empty: "No agent changes",
};

export const strings: Dict<typeof zh> = { zh, en };
