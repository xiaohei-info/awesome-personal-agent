import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  materials: "材料",
  noMaterials: "这个项目还没有材料",
  hosted: "在仓库里（只读）",
  owned: "在 APA 里",
  guidelines: "规范",
  noGuidelines: "该项目还没有规范",
  noActiveRevision: "还没有生效的版本，等你确认",
  detail: "材料详情",
  global: "全局",
  registered: (where: string) => `登记信息：${where}`,
  lastCommit: (c: string) => ` · 最近 commit ${c}`,
  reading: "读取中…",
  unavailable: (why: string) => `暂不可读：${why}`,
  defaultBranch: (c: string) => `默认分支内容 · commit ${c}`,
  change: "交给执行者去仓库改",
  executor: "执行者",
  choose: "请选择",
  request: "修改要求",
  started: "已交给执行者：",
};

const en: typeof zh = {
  ...common.en,
  materials: "Materials",
  noMaterials: "No materials in this project yet",
  hosted: "in the repository (read-only)",
  owned: "in APA",
  guidelines: "Guidelines",
  noGuidelines: "This project has no guideline yet",
  noActiveRevision: "No active revision yet (waiting for your confirmation)",
  detail: "Material detail",
  global: "global",
  registered: (where) => `Registered: ${where}`,
  lastCommit: (c) => ` · last commit ${c}`,
  reading: "Reading…",
  unavailable: (why) => `Not readable right now: ${why}`,
  defaultBranch: (c) => `Default branch · commit ${c}`,
  change: "Have an executor change it in the repo",
  executor: "Executor",
  choose: "Choose…",
  request: "What to change",
  started: "Handed to the executor: ",
};

export const strings: Dict<typeof zh> = { zh, en };
