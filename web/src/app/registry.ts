import type { ReactNode } from "react";
import type { ApiClient } from "./api";

/** 导航入口：一个入口下的多个页面在入口里以页签切换（设计 §4.1）。short 是手机顶栏上的短名。 */
export const SECTIONS = {
  today: { title: "今天" },
  decisions: { title: "决策待办", short: "决策" },
  proposals: { title: "建议流", short: "建议" },
  items: { title: "事项" },
  projects: { title: "项目" },
  agents: { title: "agent 动态" },
  sources: { title: "来源" },
  automation: { title: "规则与判断" },
  health: { title: "健康" },
  settings: { title: "设置" },
} satisfies Record<string, { title: string; short?: string }>;
export type SectionId = keyof typeof SECTIONS;

/** 入口分组，按此顺序显示；"设置"不在组里，固定在导航底部。 */
export const NAV: { group: string; sections: SectionId[] }[] = [
  { group: "处理", sections: ["today", "decisions", "proposals"] },
  { group: "工作", sections: ["items", "projects", "agents"] },
  { group: "运营", sections: ["sources", "automation", "health"] },
];
export const SETTINGS: SectionId = "settings";
/** 手机顶栏直接显示的入口（§4.1 手机主路径），其余收进"更多"。 */
export const MOBILE: SectionId[] = ["today", "decisions", "proposals", "items"];

// 页面按 web/src/features/<名称>/index.ts(x) 注册：导出 `feature`。新增页面只加目录，不改中心路由表。
export interface Feature {
  id: string;
  title: string;
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

/** 导航上的一个入口：页面已按入口内顺序排好，第一个是默认页。 */
export interface Entry {
  id: string;
  title: string;
  short?: string;
  features: Feature[];
}
export interface NavModel {
  groups: { group: string; entries: Entry[] }[];
  settings?: Entry;
  /** 全部入口，按显示顺序（设置在最后）。 */
  entries: Entry[];
}

/** 把注册的页面（已排序、已去掉 hidden）归到入口；没有页面的入口不显示。 */
export function buildNav(list: Feature[]): NavModel {
  const entry = (id: SectionId): Entry | undefined => {
    const fs = list.filter((f) => f.section === id);
    return fs.length ? { id, ...SECTIONS[id], features: fs } : undefined;
  };
  const groups = NAV.map((g) => ({ group: g.group, entries: g.sections.flatMap((s) => entry(s) ?? []) }));
  const orphans = list.filter((f) => !f.section).map((f): Entry => ({ id: f.id, title: f.title, features: [f] }));
  if (orphans.length) groups[groups.length - 1].entries.push(...orphans);
  const settings = entry(SETTINGS);
  const visible = groups.filter((g) => g.entries.length);
  return { groups: visible, settings, entries: [...visible.flatMap((g) => g.entries), ...(settings ? [settings] : [])] };
}

/** 当前地址落在哪个页面：取路径匹配最长的那个（settings/grants 优先于 settings）。 */
export function featureAt(list: Feature[], pathname: string): Feature | undefined {
  const p = pathname.replace(/^\/+/, "");
  return list
    .filter((f) => p === f.path || p.startsWith(f.path + "/"))
    .sort((a, b) => b.path.length - a.path.length)[0];
}
