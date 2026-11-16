import type { Dict } from "../../app/i18n";
import { shared } from "../settings-policy/strings";

// 设置 › 笔记库：每个库一行，登记库名与「打开方式」（URL 模板，占位符 {vault}、{path}）。
const zh = {
  ...shared.zh,
  title: "笔记库",
  summary: (n: number) => (n ? `已登记 ${n} 个` : "还没有登记"),
  lede: "登记文档所在的库和打开方式。",
  card: "我的笔记库",
  empty: "还没有登记笔记库",
  add: "＋ 添加笔记库",
  name: (n: number) => `库${n} 名称`,
  url: (n: number) => `库${n} 打开方式`,
  del: (n: number) => `删除库 ${n}`,
  namePh: "库名",
  urlPh: "例：https://example.com/{vault}/{path}",
  preview: (n: number) => `库${n} 示例地址`,
  sample: "示例路径",
  hint: "{vault} 与 {path} 会换成库名和库内路径",
  nameEmpty: (n: number) => `第 ${n} 行的库名不能为空`,
  nameDup: (name: string) => `库名「${name}」重复`,
  urlEmpty: (n: number) => `第 ${n} 行的打开方式不能为空`,
};

const en: typeof zh = {
  ...shared.en,
  title: "Note vaults",
  summary: (n) => (n ? `${n} registered` : "None registered"),
  lede: "Register where my documents live and how to open them.",
  card: "My note vaults",
  empty: "No note vaults registered",
  add: "+ Add a vault",
  name: (n) => `Vault ${n} name`,
  url: (n) => `Vault ${n} open link`,
  del: (n) => `Delete vault ${n}`,
  namePh: "Vault name",
  urlPh: "e.g. https://example.com/{vault}/{path}",
  preview: (n) => `Vault ${n} sample link`,
  sample: "Sample path",
  hint: "{vault} and {path} become the vault name and the path inside it",
  nameEmpty: (n) => `Row ${n} needs a vault name`,
  nameDup: (name) => `Vault name "${name}" is used twice`,
  urlEmpty: (n) => `Row ${n} needs an open link`,
};

export const strings: Dict<typeof zh> = { zh, en };
