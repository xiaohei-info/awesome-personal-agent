import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  actions: { done: "完成事项", progress: "写进展", plan: "改计划日期" },
  unsupported: (a: string) => `不支持的动作：${a}`,
  missingDo: "（缺少 do 参数）",
  home: "回到首页",
  phase: (p: string) => `阶段：${p}`,
  planned: (d: string) => `计划日期：${d}`,
  alreadyDone: "该事项已经是完成状态",
  doneReason: "Web 动作链接确认",
  doneOk: "已完成",
  confirmDone: "确认完成",
  progressOk: "已记录进展",
  planOk: "已改计划日期",
  progress: "进展",
  date: "日期（可选）",
  saveProgress: "保存进展",
  plannedDate: "计划日期",
  savePlan: "保存计划日期",
};

const en: typeof zh = {
  ...common.en,
  actions: { done: "Complete item", progress: "Add progress", plan: "Change planned date" },
  unsupported: (a) => `Unsupported action: ${a}`,
  missingDo: "(missing do parameter)",
  home: "Back to home",
  phase: (p) => `Phase: ${p}`,
  planned: (d) => `Planned date: ${d}`,
  alreadyDone: "This item is already done",
  doneReason: "confirmed from a Web action link",
  doneOk: "Done",
  confirmDone: "Confirm done",
  progressOk: "Progress recorded",
  planOk: "Planned date changed",
  progress: "Progress",
  date: "Date (optional)",
  saveProgress: "Save progress",
  plannedDate: "Planned date",
  savePlan: "Save planned date",
};

export const strings: Dict<typeof zh> = { zh, en };
