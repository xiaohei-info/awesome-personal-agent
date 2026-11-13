import type { ReactNode } from "react";
import type { ApiClient } from "./api";

/** 导航分组，按此顺序显示；没声明 group 的页面归到最后一组。 */
export const GROUPS = ["今天", "工作", "接入", "规则", "系统"] as const;
export type Group = (typeof GROUPS)[number];

// 页面按 web/src/features/<名称>/index.ts(x) 注册：导出 `feature`。新增页面只加目录，不改中心路由表。
export interface Feature {
  id: string;
  title: string;
  /** 相对根的路径，如 "decisions"。 */
  path: string;
  /** 导航排序（组内），小的在前；默认 100。 */
  order?: number;
  /** 导航分组；缺省归到最后一组。 */
  group?: Group;
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

export const groupOf = (f: Feature): Group => f.group ?? GROUPS[GROUPS.length - 1];
