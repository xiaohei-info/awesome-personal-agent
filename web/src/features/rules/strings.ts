import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "规则",
  groups: { reminder: "提醒我", action: "替我动手", other: "其他" },
  hint: "试运行只记录，不动手。",
  none: "还没有加载任何规则",
  mode: (name: string) => `${name}：开关`,
  week: (n: number) => (n ? `本周本来会做 ${n} 次 ›` : "本周还没触发"),
  hide: "收起",
  observations: (name: string) => `${name}：本来会做的事`,
  noObservations: "还没有记录",
  observed: (title: string, at: string) => `${at} · ${title}`,
};

const en: typeof zh = {
  ...common.en,
  title: "Rules",
  groups: { reminder: "Remind me", action: "Act for me", other: "Other" },
  hint: "Trial only records; it does nothing.",
  none: "No rules are loaded yet",
  mode: (name) => `${name}: switch`,
  week: (n) => (n ? `Would have run ${n}× this week ›` : "Not triggered this week"),
  hide: "Hide",
  observations: (name) => `${name}: what it would have done`,
  noObservations: "Nothing recorded yet",
  observed: (title, at) => `${at} · ${title}`,
};

export const strings: Dict<typeof zh> = { zh, en };
