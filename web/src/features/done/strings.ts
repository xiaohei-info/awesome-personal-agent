import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "已完成",
  lede: "这一周做成了哪些事：按完成那天算，周一开始。",
  prev: "上一周",
  next: "下一周",
  thisWeek: "本周",
  period: (from: string, to: string) => `${from.slice(5)} – ${to.slice(5)}`,
  total: (n: number, p: number) => `共 ${n} 件，分属 ${p} 个项目`,
  totalOne: (n: number) => `共 ${n} 件`,
  count: (n: number) => `${n} 件`,
  noProject: "没归项目的",
  on: (w: string, d: string) => `周${w} ${d.slice(5)}`,
  weekdays: ["一", "二", "三", "四", "五", "六", "日"],
  emptyThis: "这周还没有完成的事。做完一件，它会按完成那天记在这里。",
  emptyPast: "这一周没有完成的事",
  seeLast: "看上一周",
};

const en: typeof zh = {
  ...common.en,
  title: "Done",
  lede: "What got done this week, by the day it was finished. Weeks start on Monday.",
  prev: "Previous week",
  next: "Next week",
  thisWeek: "This week",
  period: (from, to) => `${from.slice(5)} – ${to.slice(5)}`,
  total: (n, p) => `${n} done across ${p} projects`,
  totalOne: (n) => `${n} done`,
  count: (n) => `${n}`,
  noProject: "No project",
  on: (w, d) => `${w} ${d.slice(5)}`,
  weekdays: ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"],
  emptyThis: "Nothing finished this week yet. When you finish something it shows up here, on the day it was done.",
  emptyPast: "Nothing was finished that week",
  seeLast: "See last week",
};

export const strings: Dict<typeof zh> = { zh, en };
