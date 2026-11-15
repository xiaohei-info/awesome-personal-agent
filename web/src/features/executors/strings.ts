import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

// Agent 动态 › 执行者：谁在替你干活（§6.17）。执行者由 Agent 在委派时登记，这里只看、只移除。
const zh = {
  ...common.zh,
  title: "执行者",
  empty: "还没有人替你干活",
  inHand: "手上的事",
  noneInHand: "手上没有事",
  lastResult: "最近交回",
  noResult: "还没交回过",
  sessions: "可续接的会话",
  remove: "移除",
  notListed: "未登记",
};

const en: typeof zh = {
  ...common.en,
  title: "Executors",
  empty: "Nobody is working for you yet",
  inHand: "In hand",
  noneInHand: "Nothing in hand",
  lastResult: "Last result",
  noResult: "Nothing handed back yet",
  sessions: "Sessions to resume",
  remove: "Remove",
  notListed: "Not registered",
};

export const strings: Dict<typeof zh> = { zh, en };
