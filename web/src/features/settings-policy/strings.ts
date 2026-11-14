import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 设置里几张策略卡片共用的文案（shared.tsx）。
const sharedZh = {
  ...common.zh,
  saveCard: (title: string) => `保存${title}`,
  removeTag: (v: string) => `去掉 ${v}`,
  del: "删除",
  any: "任意",
};
const sharedEn: typeof sharedZh = {
  ...common.en,
  saveCard: (title) => `Save ${title}`,
  removeTag: (v) => `Remove ${v}`,
  del: "Delete",
  any: "Any",
};
export const shared: Dict<typeof sharedZh> = { zh: sharedZh, en: sharedEn };

// 设置 › 我来拍板：只能本人在网页上回答的类别（web_only），与 agent 写的文档哪些先给本人看（材料写入策略）。
const zh = {
  ...sharedZh,
  title: "我来拍板",
  summary: (n: number) => `${n} 类只能你确认`,
  lede: "这些事 agent 只能问，不能替你定。",
  webOnly: "只能我在网页上亲自确认",
  custom: "自定义类别",
  addCustom: "添加",
  rulesHead: "另外，以下事项也算：",
  rule: (n: number) => `规则 ${n}`,
  when: "当事项类型是",
  tag: "标签",
  project: "项目",
  counts: "算作",
  ruleKind: (n: number) => `规则${n} 类型`,
  ruleTag: (n: number) => `规则${n} 标签`,
  ruleProject: (n: number) => `规则${n} 项目`,
  ruleCategory: (n: number) => `规则${n} 类别`,
  delRule: (n: number) => `删除规则 ${n}`,
  addRule: "再加一条",
  materials: "agent 写的文档，哪些先给我过目",
  materialsHint: "直接生效的，可在 agent 动态一键撤回",
  other: "其他类型",
  locked: "始终先给我看",
};

const en: typeof zh = {
  ...sharedEn,
  title: "Your call",
  summary: (n) => `${n} kinds only you confirm`,
  lede: "Agents can ask about these, never decide them.",
  webOnly: "Only I confirm, on the web",
  custom: "Custom category",
  addCustom: "Add",
  rulesHead: "These items also count:",
  rule: (n) => `Rule ${n}`,
  when: "When the item type is",
  tag: "tag",
  project: "project",
  counts: "counts as",
  ruleKind: (n) => `Rule ${n} type`,
  ruleTag: (n) => `Rule ${n} tag`,
  ruleProject: (n) => `Rule ${n} project`,
  ruleCategory: (n) => `Rule ${n} category`,
  delRule: (n) => `Delete rule ${n}`,
  addRule: "Add another",
  materials: "Documents agents write: which I see first",
  materialsHint: "Ones that take effect can be undone in Agent activity",
  other: "Other types",
  locked: "Always shown to me first",
};

export const strings: Dict<typeof zh> = { zh, en };
