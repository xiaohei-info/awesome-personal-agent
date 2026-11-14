import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "简报",
  brief: (t: string) => `简报 ${t}`,
  noSections: "暂无分节内容",
  needSchedule: "请填写定时",
  briefs: "简报与复盘",
  none: "还没有简报",
  instances: "实例管理",
  scheduled: "定时实例",
  instanceList: "实例",
  disabled: "（已停用）",
  pack: "能力包",
  schedule: "定时",
  projects: "项目（逗号分隔，可空）",
  create: "创建实例",
};

const en: typeof zh = {
  ...common.en,
  title: "Briefs",
  brief: (t) => `Brief ${t}`,
  noSections: "No sections yet",
  needSchedule: "Enter a schedule",
  briefs: "Briefs and reviews",
  none: "No briefs yet",
  instances: "Instances",
  scheduled: "Scheduled instances",
  instanceList: "Instances",
  disabled: " (disabled)",
  pack: "Capability pack",
  schedule: "Schedule",
  projects: "Projects (comma-separated, optional)",
  create: "Create instance",
};

export const strings: Dict<typeof zh> = { zh, en };
