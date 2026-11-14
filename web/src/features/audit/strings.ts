import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "操作记录",
  lede: "谁在什么时候做了什么。",
  actor: "谁",
  anyone: "所有人",
  you: "你",
  type: "做了什么",
  anything: "全部",
  typeGroups: { items: "事项与决策", agents: "委派与后台运行", projects: "项目与材料", settings: "设置与授权", sources: "来源与提醒", login: "登录", other: "其他" },
  object: "对象（事项或材料的 id）",
  advanced: "按对象查",
  filter: "筛选",
  rejectedOnly: "只看被拒绝的操作",
  clear: "清除",
  empty: "没有符合条件的记录",
  events: "操作记录",
  byGrant: "按临时授权",
  hidden: "内容已删除",
  more: "加载更多",
};

const en: typeof zh = {
  ...common.en,
  title: "Activity log",
  lede: "Who did what, and when.",
  actor: "Who",
  anyone: "Anyone",
  you: "You",
  type: "What",
  anything: "Everything",
  typeGroups: { items: "Items & decisions", agents: "Delegations & background runs", projects: "Projects & materials", settings: "Settings & grants", sources: "Sources & reminders", login: "Sign-in", other: "Other" },
  object: "Object (item or material id)",
  advanced: "Filter by object",
  filter: "Filter",
  rejectedOnly: "Only rejected actions",
  clear: "Clear",
  empty: "Nothing matches",
  events: "Activity log",
  byGrant: "via a grant",
  hidden: "content deleted",
  more: "Load more",
};

export const strings: Dict<typeof zh> = { zh, en };
