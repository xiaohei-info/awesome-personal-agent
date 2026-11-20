import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";

const zh = {
  ...common.zh,
  title: "通用",
  language: "界面语言",
  auto: "跟随浏览器",
  autoNow: (name: string) => `（现在是 ${name}）`,
  scope: "只对这台浏览器生效",
  raw: "显示原始值",
  rawHint: "在中文名旁显示系统里的 ID",
  summary: (lang: string) => lang,
};

const en: typeof zh = {
  ...common.en,
  title: "General",
  language: "Interface language",
  auto: "Follow the browser",
  autoNow: (name) => ` (currently ${name})`,
  scope: "Applies to this browser only",
  raw: "Show raw values",
  rawHint: "Show system IDs next to names",
  summary: (lang) => lang,
};

export const strings: Dict<typeof zh> = { zh, en };
