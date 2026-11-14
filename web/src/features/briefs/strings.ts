import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "简报",
  brief: (t: string) => `简报 ${t}`,
  noSections: "暂无分节内容",
  needSchedule: "请选好时间",
  briefs: "简报与复盘",
  none: "还没有简报",
  suggest: "每天早上给我一份",
  instances: "定时简报",
  scheduled: "按时给我简报",
  instanceList: "已安排的",
  disabled: "（已停用）",
  pack: "做什么",
  schedule: "什么时候",
  whichDays: "哪几天",
  atTime: "几点",
  days: { "*": "每天", "1-5": "工作日", "1": "每周一", "2": "每周二", "3": "每周三", "4": "每周四", "5": "每周五", "6": "每周六", "0": "每周日" } as Record<string, string>,
  zone: (z: string) => `（${z}）`,
  projects: "只看这些项目（可不选）",
  advanced: "高级：直接写定时",
  rawSchedule: "cron 或时长",
  create: "安排",
};

const en: typeof zh = {
  ...common.en,
  title: "Briefs",
  brief: (t) => `Brief ${t}`,
  noSections: "No sections yet",
  needSchedule: "Pick a time",
  briefs: "Briefs and reviews",
  none: "No briefs yet",
  suggest: "Send me one every morning",
  instances: "Scheduled briefs",
  scheduled: "Send me briefs on a schedule",
  instanceList: "Scheduled",
  disabled: " (disabled)",
  pack: "What",
  schedule: "When",
  whichDays: "Days",
  atTime: "Time",
  days: { "*": "Every day", "1-5": "Weekdays", "1": "Mondays", "2": "Tuesdays", "3": "Wednesdays", "4": "Thursdays", "5": "Fridays", "6": "Saturdays", "0": "Sundays" } as Record<string, string>,
  zone: (z) => ` (${z})`,
  projects: "Only these projects (optional)",
  advanced: "Advanced: write the schedule",
  rawSchedule: "cron or duration",
  create: "Schedule it",
};

export const strings: Dict<typeof zh> = { zh, en };
