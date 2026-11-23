// 投递矩阵（§7.4、附录 B.2 disturb.delivery）的形状与只读摘要；设置 › 提醒与推送是唯一可编辑处，
// 提醒渠道卡、定时安排卡只用这里的函数显示"用来送 / 结果送到"。行键与内核 kernel/policy/delivery.go 的 FixedRows 一致。

export interface Target { agent: string; user: string; else?: string; by_priority?: Record<string, Target> }
export interface Delivery { default_channel: string; rows: Record<string, Target> }

export const TASK_TYPES = ["brief", "review", "routine"] as const;
const TASK_ROWS = [...TASK_TYPES, "other"];

/** 矩阵的六组与各组的固定行（顺序即页面顺序）；按实例覆盖的行另按实例列在 D 组里。 */
export const GROUPS: { id: "A" | "B" | "C" | "D" | "E" | "F"; rows: string[] }[] = [
  { id: "A", rows: ["decision", "agent_answered"] },
  { id: "B", rows: ["scheduled"] },
  { id: "C", rows: ["overdue", "followup", "stale", "rule_reminder"] },
  { id: "D", rows: [...TASK_ROWS.map((t) => "task_result:" + t), ...TASK_ROWS.map((t) => "task_failed:" + t)] },
  { id: "E", rows: ["source_failing", "run_failed", "health"] },
  { id: "F", rows: ["new_proposals", "source_alert"] },
];

/** 两层都不推时只记在 APA、不进今天页的提醒（其余行仍留一条）：例行任务的结果。 */
export const recordOnly = (row: string, pack?: string) => row === "task_result:routine" || (row.startsWith("instance:") && pack === "routine");

/** 没存这一行时的出厂值（与 kernel/policy.FactoryDelivery 一致）：只用于"单独设"时给一个起点。 */
export const OFF: Target = { agent: "off", user: "off" };

/** 这一行（含按优先级的覆盖）有没有用到 route：本人层直接指向它、指向默认渠道而默认渠道是它、或主会话唤醒不了时改送它。 */
export function uses(t: Target, d: Delivery, route: string): boolean {
  const hit = (v?: string) => !!v && (v === route || (v === "default" && d.default_channel === route));
  return hit(t.user) || (t.agent === "main" && hit(t.else)) || Object.values(t.by_priority ?? {}).some((p) => uses(p, d, route));
}

/** 用到 route 的行键（按矩阵顺序，按实例的行排在最后）。 */
export function rowsUsing(d: Delivery, route: string): string[] {
  const fixed = GROUPS.flatMap((g) => g.rows);
  const keys = [...fixed, ...Object.keys(d.rows).filter((k) => !fixed.includes(k)).sort()];
  return keys.filter((k) => d.rows[k] && uses(d.rows[k], d, route));
}

/** 实例的结果落在哪一行：存了按实例的覆盖就是它，否则跟随类型。 */
export function instanceTarget(d: Delivery, id: string, pack: string): { target?: Target; own: boolean } {
  const own = d.rows["instance:" + id];
  if (own) return { target: own, own: true };
  return { target: d.rows["task_result:" + ((TASK_TYPES as readonly string[]).includes(pack) ? pack : "other")], own: false };
}

/** 一行去处的一句话摘要（只读处用）：两层各一段；都不推时说放在哪。 */
export function summary(t: Target | undefined, l: { off: string; main: string; next_chat: string; record: string; web: string; defaultChannel: string; channel: string }, record: boolean): string {
  if (!t) return record ? l.record : l.web;
  const parts = [t.agent === "main" ? l.main : t.agent === "next_chat" ? l.next_chat : "", t.user === "default" ? l.defaultChannel : t.user !== "off" ? l.channel : ""].filter(Boolean);
  return parts.length ? parts.join(" + ") : record ? l.record : l.web;
}
