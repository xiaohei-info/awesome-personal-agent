import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// 钥匙给谁用与逐来源档位（§14.2、§14.14）：签发与编辑钥匙时用；来源送外部模型端点的范围在「设置 › 模型」，不在这里。
const zh = {
  ...common.zh,
  audience: "这把钥匙给谁用",
  internal: "APA 内部",
  internalCan: "主 Agent、辅 Agent、本机命令行：读全部内容",
  external: "外部程序",
  externalCan: "只读下面给了的来源，按你选的档",
  chooseAudience: "先选给谁用",
  sources: "能读哪些来源",
  sourcesHint: "同时决定它读到的事项正文与材料",
  setAll: "全部设为",
  tierNone: "不给",
  tierMeta: "只有标题",
  tierExcerpt: "摘要和开头",
  tierFull: "全部内容",
  noSources: "还没有来源",
  readsAll: "APA 内部：读全部内容",
  readsNothing: "外部：什么来源都不给",
  reads: (list: string) => `外部：${list}`,
  rest: (n: number) => `其余 ${n} 个不给`,
  sep: "、",
  edit: "改",
  saveScope: "保存",
};

const en: typeof zh = {
  ...common.en,
  audience: "Who uses this key",
  internal: "Inside APA",
  internalCan: "Main and helper Agents, the local CLI: reads everything",
  external: "An outside program",
  externalCan: "Reads only the sources given below, at the level you pick",
  chooseAudience: "Choose who uses it first",
  sources: "Sources it may read",
  sourcesHint: "Also decides the item text and materials it reads",
  setAll: "Set all to",
  tierNone: "Nothing",
  tierMeta: "Title only",
  tierExcerpt: "Summary and opening",
  tierFull: "Everything",
  noSources: "No sources yet",
  readsAll: "Inside APA: reads everything",
  readsNothing: "Outside: no source given",
  reads: (list) => `Outside: ${list}`,
  rest: (n) => `the other ${n} not given`,
  sep: ", ",
  edit: "Change",
  saveScope: "Save",
};

export const strings: Dict<typeof zh> = { zh, en };
