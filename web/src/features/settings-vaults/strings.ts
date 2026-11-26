import type { Dict } from "../../app/i18n";
import { shared } from "../../app/strings";

// 设置 › 笔记库：每个库一行，登记库名与「打开方式」（默认 Obsidian；自定义 URL 模板在高级里，占位符 {vault}、{path}）。
const zh = {
  ...shared.zh,
  title: "笔记库",
  summary: (n: number) => (n ? `已登记 ${n} 个` : "还没有登记"),
  lede: "把库里的文档挂到事项上，点链接回笔记应用读原文。",
  card: "我的笔记库",
  empty: "还没有登记笔记库",
  add: "＋ 添加笔记库",
  name: (n: number) => `库${n} 名称`,
  url: (n: number) => `库${n} 打开方式`,
  customUrl: (n: number) => `库${n} 自定义地址`,
  obsidian: "Obsidian",
  nameOnly: "只登记名字",
  custom: "自定义地址",
  advanced: "高级",
  del: (n: number) => `删除库 ${n}`,
  namePh: "库名（和笔记应用里显示的一样）",
  urlPh: "例：https://example.com/{vault}/{path}",
  preview: (n: number) => `库${n} 示例地址`,
  sample: "示例路径",
  hint: "{vault} 与 {path} 会换成库名和库内路径",
  nameEmpty: (n: number) => `第 ${n} 行的库名不能为空`,
  nameDup: (name: string) => `库名「${name}」重复`,
};

const en: typeof zh = {
  ...shared.en,
  title: "Note vaults",
  summary: (n) => (n ? `${n} registered` : "None registered"),
  lede: "Link documents to items and open the originals in your notes app.",
  card: "My note vaults",
  empty: "No note vaults registered",
  add: "+ Add a vault",
  name: (n) => `Vault ${n} name`,
  url: (n) => `Vault ${n} open with`,
  customUrl: (n) => `Vault ${n} custom link`,
  obsidian: "Obsidian",
  nameOnly: "Name only",
  custom: "Custom link",
  advanced: "Advanced",
  del: (n) => `Delete vault ${n}`,
  namePh: "Vault name (as shown in the notes app)",
  urlPh: "e.g. https://example.com/{vault}/{path}",
  preview: (n) => `Vault ${n} sample link`,
  sample: "Sample path",
  hint: "{vault} and {path} become the vault name and the path inside it",
  nameEmpty: (n) => `Row ${n} needs a vault name`,
  nameDup: (name) => `Vault name "${name}" is used twice`,
};

export const strings: Dict<typeof zh> = { zh, en };
