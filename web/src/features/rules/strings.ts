import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "自动规则",
  groups: { action: "替我动手", other: "其他" } as Record<string, string>,
  remindMe: "提醒我",
  when: "什么时候提醒",
  hint: "试运行只记录，不动手。",
  none: "还没有加载任何规则",
  mode: (name: string) => `${name}：开关`,
  week: (n: number) => (n ? `本周本来会做 ${n} 次 ›` : "本周还没触发"),
  hide: "收起",
  observations: (name: string) => `${name}：本来会做的事`,
  noObservations: "还没有记录",
  observed: (title: string, at: string) => `${at} · ${title}`,
  launchOf: (name: string) => `${name}：Agent 在哪里做`,
  launchHow: { inherit: "跟随默认", headless: "单独在后台做", wake: "在对话里接着做" } as Record<string, string>,
};

const en: typeof zh = {
  ...common.en,
  title: "Automatic rules",
  groups: { action: "Act for me", other: "Other" },
  remindMe: "Remind me",
  when: "When it reminds",
  hint: "Trial only records; it does nothing.",
  none: "No rules are loaded yet",
  mode: (name) => `${name}: switch`,
  week: (n) => (n ? `Would have run ${n}× this week ›` : "Not triggered this week"),
  hide: "Hide",
  observations: (name) => `${name}: what it would have done`,
  noObservations: "Nothing recorded yet",
  observed: (title, at) => `${at} · ${title}`,
  launchOf: (name) => `${name}: where the Agent works`,
  launchHow: { inherit: "Follow the default", headless: "On its own in the background", wake: "Continue in your chat" },
};

export const strings: Dict<typeof zh> = { zh, en };
