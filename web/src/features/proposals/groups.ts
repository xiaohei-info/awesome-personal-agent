// 建议页的分组（主文档 §7.2）：分组由内核按合并键、来源类别与标题现算，随 GET /items?phase=proposed 的 groups 给出；
// 本页只按它摆放、折叠，算批量的条数，不自己判类。

/** origins 里的一条：第一条来源事件的类型、所属上游对象、对象已结束的依据（内核判定，附录 B.2）。 */
export interface Origin { type?: string; object?: string; ended?: { event: string; type: string; at: string } }

/** groups 里的一组：kind 为 pinned | topic | object | hosted | history | rest；items 按建立先后；bundles 是接受时合成一个事项的几条。 */
export interface Group { key: string; kind: string; topic?: string; items: string[]; bundles?: string[][]; first: string; last: string }

/** 旧服务端没给 groups 时：全部逐条列出，不折叠任何一条。 */
export const fallback = (ids: string[]): Group[] => (ids.length ? [{ key: "rest", kind: "rest", items: ids, first: "", last: "" }] : []);

/** 按出现顺序分组，组按条数从多到少。 */
export function groupBy<T>(items: T[], key: (i: T) => string): [string, T[]][] {
  const m = new Map<string, T[]>();
  for (const i of items) {
    const k = key(i);
    m.set(k, [...(m.get(k) ?? []), i]);
  }
  return [...m.entries()].sort((a, b) => b[1].length - a[1].length);
}

/**
 * 批量接受 ids 会建几个事项：同一份（bundle）里选中的 k 条合成 1 个，其余各 1 个。与内核同一口径（内核执行时按账本现状重算）。
 * 返回 { items: 新建事项数, merged: 被合进别处的条数, parts: 合成了几个 }。
 */
export function acceptPlan(ids: string[], bundles: string[][]) {
  const chosen = new Set(ids);
  let merged = 0;
  let parts = 0;
  for (const b of bundles) {
    const k = b.filter((id) => chosen.has(id)).length;
    if (k > 1) {
      merged += k - 1;
      parts++;
    }
  }
  return { items: chosen.size - merged, merged, parts };
}

/** 上游对象的可读名：PR 与 Issue 写编号，认不出的为空（调用方改用标题）。路径只在悬停里给。 */
export function objectName(path: string): string {
  const m = path.match(/\/(pull|pulls|merge_requests|issues)\/(\d+)\/?$/);
  if (!m) return "";
  return (m[1] === "issues" ? "Issue #" : "PR #") + m[2];
}
