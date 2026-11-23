// 投递矩阵（§7.4、附录 B.2 disturb.delivery）的形状；各行在各自的页面上改（reminders.ts 的 EDITORS），
// 提醒渠道卡只用这里的 rowsUsing 显示"推到这里的有"。行键与内核 kernel/policy/delivery.go 的 FixedRows 一致。

export interface Target { agent: string; user: string; else?: string; by_priority?: Record<string, Target> }
export interface Delivery { default_channel: string; rows: Record<string, Target> }

export const TASK_TYPES = ["brief", "review", "routine"] as const;
const TASK_ROWS = [...TASK_TYPES, "other"];

/** 矩阵的六组与各组的固定行（顺序即页面顺序）；按实例覆盖的行另按实例列在 D 组里。 */
const GROUPS: { id: "A" | "B" | "C" | "D" | "E" | "F"; rows: string[] }[] = [
  { id: "A", rows: ["decision", "agent_answered"] },
  { id: "B", rows: ["scheduled"] },
  { id: "C", rows: ["overdue", "followup", "stale", "rule_reminder"] },
  { id: "D", rows: [...TASK_ROWS.map((t) => "task_result:" + t), ...TASK_ROWS.map((t) => "task_failed:" + t)] },
  { id: "E", rows: ["source_failing", "run_failed", "health"] },
  { id: "F", rows: ["new_proposals", "source_alert"] },
];

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
