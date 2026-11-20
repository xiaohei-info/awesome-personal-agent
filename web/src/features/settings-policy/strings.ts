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

// 设置 › 我来拍板（设计 §14.3）：每个 Agent 能做到哪一档的总览，与只能本人做的那条硬线。改档位在各自的 Agent 页。
const zh = {
  ...sharedZh,
  title: "我来拍板",
  summary: (n: number) => (n ? `${n} 个 Agent 放开了拍板` : "Agent 都只整理"),
  lede: "Agent 替你做到哪一档，由你在它的页面定。",
  tiers: "三档",
  agents: "每个 Agent 现在的档位",
  noAgents: "还没有 Agent",
  change: "去改",
  hardLine: "只有你本人能做",
  hardLineWhat: "发钥匙、填凭据、改工具权限、接入新来源、外发范围、渠道回答人、物理删除",
  hardLineHow: "在网页上做，且要再输一次口令",
};

const en: typeof zh = {
  ...sharedEn,
  title: "Your call",
  summary: (n) => (n ? `${n} Agent${n === 1 ? "" : "s"} may decide` : "Agents only organize"),
  lede: "How far each Agent may go for you; set it on its page.",
  tiers: "Three tiers",
  agents: "Each Agent's tier now",
  noAgents: "No Agents yet",
  change: "Change",
  hardLine: "Only you can do",
  hardLineWhat: "Issue keys, enter credentials, change tool access, connect new sources, outbound scope, channel answerers, permanent deletion",
  hardLineHow: "On the web, after entering your password again",
};

export const strings: Dict<typeof zh> = { zh, en };
