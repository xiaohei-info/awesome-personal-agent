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
  personaLang: "助手说哪种语言",
  personaLangSystem: "跟随系统",
  personaLangHint: "决定默认语气的语言；跟随系统按助手所在机器的设置",
  personaDefault: "默认",
  personaRestore: "恢复默认",
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
  personaLang: "Assistant's language",
  personaLangSystem: "Follow the system",
  personaLangHint: "Sets the default tone's language; the system means the assistant's machine",
  personaDefault: "Default",
  personaRestore: "Restore default",
  personaUsed: (n, max) => `about ${n} / ${max}`,
  summary: (lang, persona) => lang + (persona ? " · tone set" : ""),
};

export const strings: Dict<typeof zh> = { zh, en };
