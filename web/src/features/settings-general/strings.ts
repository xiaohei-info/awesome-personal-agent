import type { Dict } from "../../app/i18n";
import { common } from "../../app/strings";
import { shared } from "../settings-policy/strings";

const zh = {
  ...common.zh,
  ...shared.zh,
  title: "通用",
  language: "界面语言",
  auto: "跟随浏览器",
  autoNow: (name: string) => `（现在是 ${name}）`,
  scope: "只对这台浏览器生效",
  raw: "显示原始值",
  rawHint: "在中文名旁显示系统里的 ID",
  persona: "助手的语气",
  personaHint: "只影响说话方式，不改变任何权限",
  personaUsed: (n: number, max: number) => `约 ${n} / ${max}`,
  summary: (lang: string, persona: boolean) => lang + (persona ? " · 已设助手语气" : ""),
};

const en: typeof zh = {
  ...common.en,
  ...shared.en,
  title: "General",
  language: "Interface language",
  auto: "Follow the browser",
  autoNow: (name) => ` (currently ${name})`,
  scope: "Applies to this browser only",
  raw: "Show raw values",
  rawHint: "Show system IDs next to names",
  persona: "Assistant's tone",
  personaHint: "Changes how it talks, never what it may do",
  personaUsed: (n, max) => `about ${n} / ${max}`,
  summary: (lang, persona) => lang + (persona ? " · tone set" : ""),
};

export const strings: Dict<typeof zh> = { zh, en };
