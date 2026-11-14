import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  materials: "材料",
  noMaterials: "该项目下没有材料",
  hosted: "托管型（只读）",
  owned: "自有型",
  guidelines: "规范",
  noGuidelines: "该项目还没有规范",
  noActiveRevision: "尚无生效的修订（等待本人确认）",
  detail: "材料详情",
  global: "全局",
  registered: (where: string) => `登记信息：${where}`,
  lastCommit: (c: string) => ` · 最近 commit ${c}`,
  reading: "读取中…",
  unavailable: (why: string) => `暂不可读：${why}`,
  defaultBranch: (c: string) => `默认分支内容 · commit ${c}`,
  change: "发起修改",
  changeHint: "文档在仓库里改：这会生成委派子事项，由执行者开分支提 PR。",
  executor: "执行者",
  choose: "请选择",
  request: "修改要求",
  started: "已发起修改委派：子事项 ",
  parent: "，父事项 ",
};

const en: typeof zh = {
  ...common.en,
  materials: "Materials",
  noMaterials: "No materials in this project",
  hosted: "hosted (read-only)",
  owned: "owned",
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
  change: "Request a change",
  changeHint: "The document lives in a repository: this creates a delegated child item, and the executor opens a branch and a PR.",
  executor: "Executor",
  choose: "Choose…",
  request: "What to change",
  started: "Change delegated: child item ",
  parent: ", parent item ",
};

export const strings: Dict<typeof zh> = { zh, en };
