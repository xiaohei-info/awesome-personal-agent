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
  typeGroups: { items: "事项与决策", agents: "委派与 Agent 后台运行", projects: "项目与材料", settings: "设置与权限", sources: "来源与提醒", login: "登录", other: "其他" },
  object: "对象（事项或材料的 id）",
  advanced: "按对象查",
  filter: "筛选",
  rejectedOnly: "只看被拒绝的操作",
  clear: "清除",
  empty: "没有符合条件的记录",
  events: "操作记录",
  basis: { owner_web: "你在网页上做的", owner_channel: "你在渠道里答的", organize: "整理（可撤回）", allowance: "按它的档位", rule: "按你开的规则" } as Record<string, string>,
  legacy: "旧模型记录",
  hidden: "内容已删除",
  more: "加载更多",
  imported: (n: number) => `导入 ${n} 条`,
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
  typeGroups: { items: "Items & decisions", agents: "Delegations & Agent background runs", projects: "Projects & materials", settings: "Settings & permissions", sources: "Sources & reminders", login: "Sign-in", other: "Other" },
  object: "Object (item or material id)",
  advanced: "Filter by object",
  filter: "Filter",
  rejectedOnly: "Only rejected actions",
  clear: "Clear",
  empty: "Nothing matches",
  events: "Activity log",
  basis: { owner_web: "done by you on the web", owner_channel: "answered by you in a channel", organize: "organizing (undoable)", allowance: "under its tier", rule: "by a rule you turned on" } as Record<string, string>,
  legacy: "old-model record",
  hidden: "content deleted",
  more: "Load more",
  imported: (n: number) => `Imported ${n}`,
};

export const strings: Dict<typeof zh> = { zh, en };
