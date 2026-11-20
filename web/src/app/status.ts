import { useEffect, useState } from "react";
import type { ApiClient } from "./api";
import { COUNTS_EVENT } from "./page";

// 形状对应 GET /v1/health（api/health.Report，附录 B.2）：十三项信号、顺序固定，判定全在服务端；这里只归类给界面用。
export interface Signal {
  id: string;
  ok: boolean;
  count: number;
  severity?: string; // "error" 或 "warning"
  why?: string;
  next: string;
  detail?: unknown;
  rejections?: { pack: string; source: string; reason: string }[];
  /** receipt_failures 的逐条原因（api/health.ReceiptRejection）：命令、退出码、原因码、凭据。 */
  receipts?: { object: string; command: string; exit: number; code?: string; reason?: string; credential: string }[];
  /** other_alerts 的逐条通知：动作没能完成的类别码、机制参数与受影响的事项（dropped：规则建的那一份已作废）。 */
  notices?: { object: string; rule?: string; code?: string; params?: Record<string, string>; item?: string; dropped?: boolean }[];
}
export interface HealthReport {
  healthy: boolean;
  signals: Signal[];
}

/**
 * 状态页的一行（界面设计 §5.3）：一个对象上的一个问题。act 为真是「要你处理」（计数、亮灯），否则「仅供知晓」（不计数）。
 * kind 决定文案；obj 是对象 id（来源、事项、规则……），n 是条数；to 是主按钮的落点，没有网页上的动作时为空。
 */
export interface Row {
  signal: string;
  kind: string;
  act: boolean;
  bad: boolean;
  obj?: string;
  n: number;
  to?: string;
  /** 原因码：运行故障的 fault.code（outcall）或被拒的原因码（receipt_failures）；展示层按它说人话。 */
  code?: string;
  params?: Record<string, string>;
  /** 被拒的操作者凭据与命令（receipt_failures）。 */
  who?: string;
  cmd?: string;
}

export interface Status {
  /** bad：有要处理的错误；warn：只有要处理的提醒级；paused：模型已暂停、别无他事；ok：运行正常。 */
  kind: "ok" | "paused" | "warn" | "bad";
  /** 要你处理的行（按对象计数：3 个来源连不上是 3 行）。 */
  act: Row[];
  /** 仅供知晓的行：本人自己的选择、会自己恢复的、已经没有可做的事。 */
  info: Row[];
  /** 正常的信号。 */
  fine: Signal[];
  paused: boolean;
  /** 暂停期间积下、还没判断的消息条数。 */
  pausedBacklog: number;
  /** 自动重判窗口（天），见 rejudgeDays。 */
  rejudgeDays?: number;
}

const num = (d: unknown, k: string) => Number((d as Record<string, unknown> | undefined)?.[k] ?? 0);
const list = <T>(d: unknown) => (Array.isArray(d) ? (d as T[]) : []);
const enc = encodeURIComponent;

/** 一条信号拆成行。信号本身、判定与清除条件全在服务端（§15.2），这里只按对象拆开并归类。 */
export function rowsOf(s: Signal): Row[] {
  if (s.ok) return [];
  const bad = s.severity !== "warning";
  const row = (kind: string, act: boolean, n: number, to?: string, obj?: string): Row => ({ signal: s.id, kind, act, bad, obj, n, to });
  const d = s.detail;
  switch (s.id) {
    case "source_failures":
      return list<{ source: string; enabled: boolean; consecutive_failures: number; kind?: string; needs_input_item?: string }>(d).map((x) =>
        !x.enabled
          ? row("sourceOff", false, 0, "/sources?open=" + enc(x.source), x.source) // 本人停用的：仅供知晓（同暂停模型）
          : row(x.kind === "auth_or_config" || x.needs_input_item ? "sourceAuth" : "sourceFail", true, x.consecutive_failures, "/sources?open=" + enc(x.source), x.source),
      );
    case "outcall": {
      const q = (d ?? {}) as { long?: { queued: number }; short?: { queued: number }; timeouts?: string[]; failures?: { run: string; target: string; fault?: { code: string; params?: Record<string, string> } }[] };
      const queued = (q.long?.queued ?? 0) + (q.short?.queued ?? 0);
      const runs = list<string>(q.timeouts);
      // Agent 运行故障（2026-11-20）：每件事一行，带故障类别码；运行故障是事件，不在「等你决定」里。
      const failures = list<{ run: string; target: string; fault?: { code: string; params?: Record<string, string> } }>(q.failures).map((f) => ({
        ...row("runFail", true, 1, "/items/" + enc(f.target) + "#agent", f.target), code: f.fault?.code, params: f.fault?.params,
      }));
      const timeouts = runs.length ? runs.map((r) => row("outcall", true, 1, "/runs", r)) : failures.length ? [] : [row("outcall", true, s.count, "/runs")];
      return [...timeouts, ...failures, ...(queued ? [row("queue", false, queued)] : [])];
    }
    case "delivery_backlog":
      return [row("delivery", true, s.count, "/settings/channels")];
    case "chain_trips": {
      const ids = list<string>(d).filter(Boolean);
      return ids.length ? ids.map((id) => row("chain", true, 1, "/items/" + enc(id), id)) : [row("chain", true, s.count, "/items")];
    }
    case "agentsmd_drift":
      return [row("drift", true, s.count, "/projects")];
    case "pack_rejections":
      return [row("packs", true, s.count)]; // 动作在本机：修好包文件后自动重新加载
    case "model_unavailable": {
      const m = (d ?? {}) as { judge?: string; generate?: string; screening?: unknown[]; judgements?: { endpoint?: string }[] };
      const out: Row[] = [];
      for (const k of ["generate", "judge"] as const) {
        const fails = list<{ endpoint?: string }>(m.judgements).filter((j) => (j.endpoint || "judge") === k).length;
        if (m[k] === "not_configured" || m[k] === "failing" || fails) out.push(row(m[k] === "not_configured" ? "modelUnset:" + k : "modelFail:" + k, true, fails, "/settings/models"));
      }
      const scr = list(m.screening).length;
      if (scr) out.push(row("screening", true, scr, "/settings/models"));
      return out.length ? out : [row("modelFail:generate", true, s.count, "/settings/models")];
    }
    case "unjudged_backlog":
      return [
        ...(num(d, "outbound_unset") ? [row("waitScope", true, num(d, "outbound_unset"), "/sources")] : []),
        ...(num(d, "restored") ? [row("restored", true, num(d, "restored"), "/sources")] : []),
        ...(num(d, "model_unavailable") + num(d, "undecided") ? [row("waitModel", false, num(d, "model_unavailable") + num(d, "undecided"))] : []),
        ...(num(d, "models_paused") ? [row("waitPaused", false, num(d, "models_paused"), "/sources")] : []),
        ...(num(d, "expired") ? [row("expired", false, num(d, "expired"))] : []),
      ];
    case "rule_fallbacks": {
      const ids = list<string>(d).filter(Boolean);
      return ids.length ? ids.map((k) => row("fallback", true, 1, "/rules#rule-" + enc(k), k)) : [row("fallback", true, s.count, "/rules")];
    }
    case "receipt_failures": {
      // 逐条说清谁的什么操作被拒、为什么、下一步（原因码在服务端给出）；没有逐条明细的旧接口退回一行计数。
      const rs = s.receipts ?? [];
      return rs.length
        ? rs.map((x) => ({ ...row("receipt", true, 1, "/audit", x.object), code: x.code, params: x.reason ? { reason: x.reason } : undefined, who: x.credential, cmd: x.command }))
        : [row("receipts", true, s.count, "/audit")];
    }
    case "other_alerts": {
      // 有类别码的逐条说清（哪件事、为什么、下一步）；升级前的告警没有类别码，合成一行计数去操作记录看。
      const ns = (s.notices ?? []).filter((x) => x.code);
      const rest = s.count - ns.length;
      return [
        ...ns.map((x) => ({ ...row(x.dropped ? "notice:dropped" : "notice", true, 1, x.item ? "/items/" + enc(x.item) : "/audit?object=" + enc(x.object), x.item ?? x.object), code: x.code, params: x.params })),
        ...(rest > 0 ? [row("others", true, rest, "/audit")] : []),
      ];
    }
    case "stalled_delegations": {
      const ids = list<string>(d).filter(Boolean);
      return ids.length ? ids.map((id) => row("stalled", true, 1, "/delegation#d-" + enc(id), id)) : [row("stalled", true, s.count, "/delegation")];
    }
    case "clock_lag":
      return [row("lag", false, s.count)];
    default:
      return [row("unknown:" + s.id, true, s.count)];
  }
}

/** 暂停模型、停用来源是本人的选择，不算要处理（§8.7）；按对象计数，没有网页动作的信号仍要说明，但只有「要你处理」亮灯。 */
export function assess(r: HealthReport): Status {
  const model = r.signals.find((s) => s.id === "model_unavailable");
  const paused = (model?.detail as { paused?: boolean } | undefined)?.paused === true;
  const backlog = r.signals.find((s) => s.id === "unjudged_backlog");
  const pausedBacklog = num(backlog?.detail, "models_paused");
  const rows = r.signals.flatMap(rowsOf);
  const act = rows.filter((x) => x.act);
  const info = rows.filter((x) => !x.act);
  const fine = r.signals.filter((s) => s.ok);
  const kind = act.some((x) => x.bad) ? "bad" : act.length ? "warn" : paused ? "paused" : "ok";
  return { kind, act, info, fine, paused, pausedBacklog, rejudgeDays: rejudgeDays(r) };
}

/** 自动重判窗口（天）：unjudged_backlog.detail.rejudge_days，0 表示关闭（§9.3）；没有这个键时为 undefined，界面不说之后会怎样。 */
export function rejudgeDays(r: { signals: { id: string; detail?: unknown }[] }): number | undefined {
  const d = r.signals.find((s) => s.id === "unjudged_backlog")?.detail as { rejudge_days?: unknown } | undefined;
  return typeof d?.rejudge_days === "number" ? d.rejudge_days : undefined;
}

/** 读一次健康视图并归类；取不到时为 null（不显示数字，也不显示「正常」）。 */
export const loadStatus = (api: ApiClient) => api.get<HealthReport>("/health").then(assess).catch(() => null);

/** 单独取状态的页面用（注意力汇总里已含状态，外壳不再单独取）。 */
export function useStatus(api: ApiClient, pathname: string): Status | null {
  const [status, setStatus] = useState<Status | null>(null);
  useEffect(() => {
    const load = () => loadStatus(api).then(setStatus);
    load();
    window.addEventListener(COUNTS_EVENT, load);
    return () => window.removeEventListener(COUNTS_EVENT, load);
  }, [api, pathname]);
  return status;
}
