// 建议页的分组与折叠（主文档 §7.2）：只按结构化字段（kind、优先级、起草的 urgent、GET /items?phase=proposed 的 origins），不读标题与正文。

/** origins 里的一条：第一条来源事件的类型、所属上游对象、对象已结束的依据（内核判定，附录 B.2）。 */
export interface Origin { type?: string; object?: string; ended?: { event: string; type: string; at: string } }

export interface Groupable {
  id: string;
  kind?: string;
  priority?: string;
  origin?: { source: string };
  proposal?: { urgent?: boolean };
}

/** 需要本人拍板的：决定与承诺、起草判为紧急、优先级高或紧急。任何分组与折叠都不收起它们。 */
export const needsYou = (i: Groupable): boolean =>
  i.kind === "decision" || i.kind === "commitment" || i.proposal?.urgent === true || i.priority === "high" || i.priority === "urgent";

/** 来源事件类型的大类（类型约定 <对象>.<动作>，取对象）；没有来源事件的为空串。 */
export const family = (type?: string): string => (type ? type.split(".")[0] : "");

export interface ObjectGroup<T> { object: string; ended?: Origin["ended"]; items: T[] }

/** 按出现顺序分组，组按条数从多到少。 */
export function groupBy<T>(items: T[], key: (i: T) => string): [string, T[]][] {
  const m = new Map<string, T[]>();
  for (const i of items) {
    const k = key(i);
    m.set(k, [...(m.get(k) ?? []), i]);
  }
  return [...m.entries()].sort((a, b) => b[1].length - a[1].length);
}

/** 拆成三块：需要拍板的（置顶）、代码托管通知（属于上游对象的，按对象合组）、其余。 */
export function arrange<T extends Groupable>(items: T[], origins: Record<string, Origin>) {
  const pinned = items.filter(needsYou);
  const hosted = items.filter((i) => !needsYou(i) && origins[i.id]?.object);
  const rest = items.filter((i) => !needsYou(i) && !origins[i.id]?.object);
  const objects: ObjectGroup<T>[] = groupBy(hosted, (i) => origins[i.id].object!).map(([object, its]) => ({
    object,
    ended: its.map((i) => origins[i.id].ended).find(Boolean),
    items: its,
  }));
  return { pinned, hosted, objects, rest };
}
