import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "改动",
  runDid: (what: string, title: string) => `Agent 后台运行替你${what}「${title}」`,
  answered: (title: string) => `Agent 替你答了「${title}」`,
  resumed: "回答后已为它续接了运行；撤销只撤回答，已拉起的运行撤不回",
  legacy: "旧模型记录",
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
  runDid: (what: string, title: string) => `An Agent background run ${what} for you: "${title}"`,
  answered: (title: string) => `An Agent answered for you: "${title}"`,
  resumed: "A run was continued after the answer; undoing removes the answer, not the run that already started",
  legacy: "old-model record",
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
