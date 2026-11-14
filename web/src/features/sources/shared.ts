import { locale } from "../../app/i18n";

// 形状对应 GET /v1/sources（domain.Source，stats 为 runner.Stats）与 /sources/{id}/events（domain.SourceEvent）。
export interface Stats {
  runs?: number;
  consecutive_failures?: number;
  timeouts?: number;
  last_success_at?: string;
  last_failure_at?: string;
  last_error?: string;
}
// 外发范围（§14.14）：external 为 null 表示未选择，没有缺省值。
export interface Outbound {
  local: string;
  external: string | null;
}
export interface Source {
  id: string;
  type: "pull" | "script" | "webhook";
  source_type?: string;
  command?: string;
  schedule?: string;
  timeout?: number;
  cursor?: string;
  enabled: boolean;
  stats?: Stats;
  outbound?: Outbound;
}
export interface SourceEvent {
  id: string;
  type: string;
  subject?: string;
  summary?: string;
  occurred_at: string;
  disposition: string;
  item?: string;
  unjudged_reason?: string;
  rejudges?: number;
  score?: number; // 影子打分的预估分数（§9.3）；没打过分时没有
}

export const SCOPES = ["full", "excerpt", "meta", "none"] as const;

const set = (s?: string) => !!s && !s.startsWith("0001");

/** 本地化时间；零值与空为 "—"。 */
export const when = (s?: string) => (set(s) ? new Date(s!).toLocaleString(locale()) : "—");

/** 相对时间（「3 分钟前」）；零值与空为 null。 */
export function ago(s?: string, now = Date.now()): string | null {
  if (!set(s)) return null;
  const sec = Math.round((new Date(s!).getTime() - now) / 1000);
  const f = new Intl.RelativeTimeFormat(locale(), { numeric: "auto" });
  const abs = Math.abs(sec);
  if (abs < 60) return f.format(sec, "second");
  if (abs < 3600) return f.format(Math.round(sec / 60), "minute");
  if (abs < 86400) return f.format(Math.round(sec / 3600), "hour");
  return f.format(Math.round(sec / 86400), "day");
}

/** 周期 "15m"、"1h"、"30s" 拆成数和单位；cron 等其他写法为 null。 */
export function period(s?: string): { n: string; unit: "s" | "m" | "h" } | null {
  const m = /^(\d+)(s|m|h)$/.exec(s ?? "");
  return m ? { n: m[1], unit: m[2] as "s" | "m" | "h" } : null;
}

/**
 * 积压里一条消息为什么还没判（kernel/domain/entities.go 的未判定原因）。旧数据的空原因与 undecided 由内核按
 * model_unavailable 对待并参与自动重判（docs/changes-v0.2.0.md 迁移 K-08），所以显示成同一种。
 */
export function backlogReason(v: Pick<SourceEvent, "disposition" | "unjudged_reason">): string {
  if (v.disposition === "expired") return "expired";
  const r = v.unjudged_reason;
  return !r || r === "undecided" ? "model_unavailable" : r;
}
