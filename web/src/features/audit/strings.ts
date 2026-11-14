import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "审计",
  lede: "账本里的每一条事件：谁、从哪个入口、对什么做了什么。只读。",
  actor: "操作者筛选",
  object: "对象筛选",
  type: "事件类型筛选",
  filter: "筛选",
  rejectedOnly: "只看被拒绝的命令",
  clear: "清除",
  empty: "没有符合条件的事件",
  events: "审计事件",
  grant: (g: string) => ` · 授权单 ${g}`,
  hidden: " · （内容已隐藏）",
  more: "加载更多",
};

const en: typeof zh = {
  ...common.en,
  title: "Audit",
  lede: "Every event in the ledger: who did what to which object, through which entry point. Read-only.",
  actor: "Filter by actor",
  object: "Filter by object",
  type: "Filter by event type",
  filter: "Filter",
  rejectedOnly: "Rejected commands only",
  clear: "Clear",
  empty: "No matching events",
  events: "Audit events",
  grant: (g) => ` · grant ${g}`,
  hidden: " · (content hidden)",
  more: "Load more",
};

export const strings: Dict<typeof zh> = { zh, en };
