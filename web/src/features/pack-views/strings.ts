import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "能力包视图",
  none: "没有能力包声明视图",
  view: "视图",
  filter: (f: string) => `筛选 ${f}`,
  all: "全部",
  noMatch: "没有符合条件的事项",
  table: "视图列表",
  cards: "视图卡片",
  card: (t: string) => `卡片 ${t}`,
};

const en: typeof zh = {
  ...common.en,
  title: "Pack views",
  none: "No capability pack declares a view",
  view: "View",
  filter: (f) => `Filter ${f}`,
  all: "All",
  noMatch: "No matching items",
  table: "View table",
  cards: "View cards",
  card: (t) => `Card ${t}`,
};

export const strings: Dict<typeof zh> = { zh, en };
