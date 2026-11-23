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

// 设置 › 我来拍板（设计 §14.3）：每个 Agent 能替你做到哪一档，就在这里改（唯一可编辑处）；下方是只能本人做的那条硬线。
const zh = {
  ...sharedZh,
  title: "我来拍板",
  summary: (n: number) => (n ? `${n} 个 Agent 放开了拍板` : "Agent 都只整理"),
  lede: "每个 Agent 能替你做到哪一档，就在这里改。",
  noAgents: "还没有 Agent，先在「设置 › Agent」添加",
  tier: (name: string) => `${name}能做到哪一档`,
  tierWhere: "拍板限在哪些项目",
  allProjects: "全部项目",
  save: "保存档位",
  hardLine: "只有你本人能做",
  hardLineWhat: "发钥匙、填凭据、改工具权限、接入新来源、外发范围、渠道回答人、物理删除",
  hardLineHow: "在网页上做，且要再输一次口令",
  notifyCard: "有事要你拍板时怎么告诉你",
};

const en: typeof zh = {
  ...sharedEn,
  title: "Your call",
  summary: (n) => (n ? `${n} Agent${n === 1 ? "" : "s"} may decide` : "Agents only organize"),
  lede: "How far each Agent may go for you; change it right here.",
  noAgents: "No Agents yet; add one in Settings › Agents",
  tier: (name) => `How far ${name} may go`,
  tierWhere: "Decide only in these projects",
  allProjects: "All projects",
  save: "Save tier",
  hardLine: "Only you can do",
  hardLineWhat: "Issue keys, enter credentials, change tool access, connect new sources, outbound scope, channel answerers, permanent deletion",
  hardLineHow: "On the web, after entering your password again",
  notifyCard: "How you hear about decisions",
};

export const strings: Dict<typeof zh> = { zh, en };
