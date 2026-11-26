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

// 设置 › 权限总览（设计 §14.5）：每个 Agent 的权限卡一览，只看；点名字去它自己的页面改。下方是有事要你拍板时怎么告诉你。
const zh = {
  ...sharedZh,
  title: "权限总览",
  summary: (n: number) => (n ? `${n} 个 Agent 能替你拍板` : "Agent 都不能替你拍板"),
  lede: "每个 Agent 能替你做什么。点名字去它的页面改。",
  noAgents: "还没有 Agent，先在「设置 › Agent」添加",
  allowed: "允许",
  denied: "不允许",
  allowedUntil: (at: string) => `允许到 ${at}`,
  ownerOnly: "只有你本人能做：改权限卡、发和收回钥匙、改登录口令、指定谁能在渠道里代你回答。",
  notifyCard: "有事要你拍板时怎么告诉你",
};

const en: typeof zh = {
  ...sharedEn,
  title: "Permissions",
  summary: (n) => (n ? `${n} Agent${n === 1 ? "" : "s"} may decide for you` : "No Agent may decide for you"),
  lede: "What each Agent may do for you. Open an Agent to change it.",
  noAgents: "No Agents yet; add one in Settings › Agents",
  allowed: "Allowed",
  denied: "Not allowed",
  allowedUntil: (at) => `Allowed until ${at}`,
  ownerOnly: "Only you can: change permission cards, issue and revoke keys, change the login password, pick who may answer for you in a channel.",
  notifyCard: "How you hear about decisions",
};

export const strings: Dict<typeof zh> = { zh, en };
