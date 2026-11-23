import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "已完成",
  lede: "这一周推进了什么、做成了哪些事：完成按完成那天、进展按记的那天算，周一开始。",
  prev: "上一周",
  next: "下一周",
  thisWeek: "本周",
  period: (from: string, to: string) => `${from.slice(5)} – ${to.slice(5)}`,
  stats: (progress: number, done: number, projects: number) => `进展 ${progress} 条 · 完成 ${done} 项 · 涉及项目 ${projects} 个`,
  groupStats: (progress: number, done: number) => `进展 ${progress} 条 · 完成 ${done} 项`,
  doneOn: (d: string) => `${d} 完成`,
  progressOf: (title: string) => `${title} 这周的进展`,
  doneUnknown: "完成日期不详",
  unknownTitle: "完成日期不详",
  unknownNote: (n: number) => `${n} 件从工作台迁移来的事已完成，但没有记下完成那天，不算在任何一周里。`,
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
  lede: "What moved and what got done this week: completions by the day they were finished, progress by the day it was recorded for. Weeks start on Monday.",
  prev: "Previous week",
  next: "Next week",
  thisWeek: "This week",
  period: (from, to) => `${from.slice(5)} – ${to.slice(5)}`,
  stats: (progress, done, projects) => `${progress} progress notes · ${done} done · ${projects} projects`,
  groupStats: (progress, done) => `${progress} progress notes · ${done} done`,
  doneOn: (d) => `done ${d}`,
  progressOf: (title) => `Progress on ${title} this week`,
  doneUnknown: "completion date unknown",
  unknownTitle: "Completion date unknown",
  unknownNote: (n) => `${n} migrated item${n === 1 ? " is" : "s are"} done but never recorded the day, so they are not counted in any week.`,
  noProject: "No project",
  on: (w, d) => `${w} ${d.slice(5)}`,
  weekdays: ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"],
  emptyThis: "Nothing finished this week yet. When you finish something it shows up here, on the day it was done.",
  emptyPast: "Nothing was finished that week",
  seeLast: "See last week",
};

export const strings: Dict<typeof zh> = { zh, en };
