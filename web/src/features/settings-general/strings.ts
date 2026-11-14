import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "通用",
  lede: "这台浏览器上的界面偏好。",
  language: "界面语言",
  auto: "跟随浏览器",
  autoNow: (name: string) => `（现在是 ${name}）`,
  scope: "只对这台浏览器生效",
  raw: "显示原始值",
  rawHint: "在中文名旁显示系统里的 ID",
};

const en: typeof zh = {
  ...common.en,
  title: "General",
  lede: "Interface preferences for this browser.",
  language: "Interface language",
  auto: "Follow the browser",
  autoNow: (name) => ` (currently ${name})`,
  scope: "Applies to this browser only",
  raw: "Show raw values",
  rawHint: "Show system IDs next to names",
};

export const strings: Dict<typeof zh> = { zh, en };
