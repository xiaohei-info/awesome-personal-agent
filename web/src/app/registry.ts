import type { ReactNode } from "react";
import type { ApiClient } from "./api";
import type { Dict, Lang } from "./i18n";

/** 一段两种语言的界面文字。 */
export type Text = Dict<string>;

/** 导航入口：一个入口下的多个页面在入口里以页签切换（设计 §4.1）。short 是手机顶栏上的短名。 */
export const SECTIONS = {
  today: { title: { zh: "今天", en: "Today" } },
  decisions: { title: { zh: "决策待办", en: "Decisions" }, short: { zh: "决策", en: "Decide" } },
  proposals: { title: { zh: "建议流", en: "Proposals" }, short: { zh: "建议", en: "Proposals" } },
  items: { title: { zh: "事项", en: "Items" } },
  projects: { title: { zh: "项目", en: "Projects" } },
  agents: { title: { zh: "agent 动态", en: "Agent activity" } },
  sources: { title: { zh: "消息来源", en: "Sources" } },
  automation: { title: { zh: "自动规则", en: "Automation" } },
  settings: { title: { zh: "设置", en: "Settings" } },
  diag: { title: { zh: "诊断", en: "Diagnostics" } },
} satisfies Record<string, { title: Text; short?: Text }>;
export type SectionId = keyof typeof SECTIONS;

/** 入口分组，按此顺序显示；"设置"与"诊断"不在组里，固定在导航底部（诊断是排障区，字色弱化）。 */
export const NAV: { group: Text; sections: SectionId[] }[] = [
  { group: { zh: "处理", en: "Act" }, sections: ["today", "decisions", "proposals"] },
  { group: { zh: "工作", en: "Work" }, sections: ["items", "projects", "agents"] },
  { group: { zh: "自动化", en: "Automate" }, sections: ["sources", "automation"] },
];
export const SETTINGS: SectionId = "settings";
export const DIAG: SectionId = "diag";
/** 设置里的分组（设计 §3.1）：桌面上是设置页的第二列，手机上是「设置首页」列表。按页面 id 归组，没列出的页面排在最后一组。 */
export const SETTINGS_GROUPS: { title: Text; ids: string[] }[] = [
  { title: { zh: "日常", en: "Everyday" }, ids: ["settings-general", "settings-notify", "settings-policy"] },
  { title: { zh: "agent", en: "Agents" }, ids: ["settings-access", "settings-grants", "settings-credentials", "settings-exec"] },
  { title: { zh: "系统", en: "System" }, ids: ["settings-models", "settings-security", "purge"] },
];
/** 手机上的「设置首页」：设置入口在窄屏上落到这里。 */
export const SETTINGS_HOME = "settings/home";
/** 手机顶栏直接显示的入口（§4.1 手机主路径），其余收进"更多"。 */
export const MOBILE: SectionId[] = ["today", "decisions", "proposals", "items"];

// 页面按 web/src/features/<名称>/index.ts(x) 注册：导出 `feature`。新增页面只加目录，不改中心路由表。
export interface Feature {
  id: string;
  title: Text;
  /** 相对根的路径，如 "decisions"。 */
  path: string;
  /** 入口内排序（页签顺序），小的在前；默认 100。入口的默认页是其中最靠前的页面。 */
  order?: number;
  /** 所属入口；缺省时页面自成一个入口，排在最后一组末尾。 */
  section?: SectionId;
  /** 旧地址或服务端拼出的地址（不含前导 /），打开时改写到 path，保留查询串与锚点。 */
  aliases?: string[];
  /** 导航上显示的数量（如待决数）；取不到或为 0 不显示。 */
  count?: (api: ApiClient) => Promise<number>;
  /** 设置首页上这一项的当前值摘要（如「每天最多 20 条」）；取不到时不显示。 */
  summary?: (api: ApiClient, lang: Lang) => Promise<string>;
  element: ReactNode;
  /** 无需登录的页面（如一次性表单）：不进导航、不经登录门，由令牌自身授权。 */
  public?: boolean;
  /** 只作链接目标（如动作确认页）：路由有效，但不进导航、跳转面板和默认落点。 */
  hidden?: boolean;
}

export function collectFeatures(modules: Record<string, { feature?: Feature }>): Feature[] {
  const out = Object.values(modules).flatMap((m) => (m.feature ? [m.feature] : []));
  const ids = new Set<string>();
  for (const f of out) {
    if (ids.has(f.id)) throw new Error(`duplicate feature id: ${f.id}`);
    ids.add(f.id);
  }
  return out.sort((a, b) => (a.order ?? 100) - (b.order ?? 100) || a.id.localeCompare(b.id));
}

export const features: Feature[] = collectFeatures(
  import.meta.glob<{ feature?: Feature }>("../features/*/index.{ts,tsx}", { eager: true }),
);

/** 导航上的一个入口（文字已按当前语言取好）：页面已按入口内顺序排好，第一个是默认页。 */
export interface Entry {
  id: string;
  title: string;
  short?: string;
  features: Feature[];
}
export interface NavModel {
  groups: { group: string; entries: Entry[] }[];
  settings?: Entry;
  diag?: Entry;
  /** 全部入口，按显示顺序（设置、诊断在最后）。 */
  entries: Entry[];
}

/** 把注册的页面（已排序、已去掉 hidden）归到入口；没有页面的入口不显示。 */
export function buildNav(list: Feature[], lang: Lang): NavModel {
  const entry = (id: SectionId): Entry | undefined => {
    const fs = list.filter((f) => f.section === id);
    const s: { title: Text; short?: Text } = SECTIONS[id];
    return fs.length ? { id, title: s.title[lang], short: s.short?.[lang], features: fs } : undefined;
  };
  const groups = NAV.map((g) => ({ group: g.group[lang], entries: g.sections.flatMap((s) => entry(s) ?? []) }));
  const orphans = list.filter((f) => !f.section).map((f): Entry => ({ id: f.id, title: f.title[lang], features: [f] }));
  if (orphans.length) groups[groups.length - 1].entries.push(...orphans);
  const settings = entry(SETTINGS);
  const diag = entry(DIAG);
  const visible = groups.filter((g) => g.entries.length);
  const foot = [settings, diag].flatMap((e) => e ?? []);
  return { groups: visible, settings, diag, entries: [...visible.flatMap((g) => g.entries), ...foot] };
}

/** 当前地址落在哪个页面：取路径匹配最长的那个（settings/grants 优先于 settings）。 */
export function featureAt(list: Feature[], pathname: string): Feature | undefined {
  const p = pathname.replace(/^\/+/, "");
  return list
    .filter((f) => p === f.path || p.startsWith(f.path + "/"))
    .sort((a, b) => b.path.length - a.path.length)[0];
}

/** 设置入口里的页面按 SETTINGS_GROUPS 分组（组内保持入口内顺序）；没有页面的组不显示。 */
export function settingsGroups(entry: Entry, lang: Lang): { title: string; features: Feature[] }[] {
  const known = new Set(SETTINGS_GROUPS.flatMap((g) => g.ids));
  const groups = SETTINGS_GROUPS.map((g) => ({ title: g.title[lang], features: entry.features.filter((f) => g.ids.includes(f.id)) }));
  groups[groups.length - 1].features.push(...entry.features.filter((f) => !known.has(f.id)));
  return groups.filter((g) => g.features.length);
}
