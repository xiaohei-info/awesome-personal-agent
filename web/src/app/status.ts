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
}
export interface HealthReport {
  healthy: boolean;
  signals: Signal[];
}

export interface Status {
  /** bad：有异常；warn：只有待你处理的提醒；paused：模型已暂停、别无他事；ok：一切正常。 */
  kind: "ok" | "paused" | "warn" | "bad";
  /** 需要处理的信号（不含只因暂停模型而积下的积压）。 */
  needs: Signal[];
  /** 正常的信号。 */
  fine: Signal[];
  paused: boolean;
  /** 暂停期间积下、还没判断的消息条数。 */
  pausedBacklog: number;
}

const num = (d: unknown, k: string) => Number((d as Record<string, unknown> | undefined)?.[k] ?? 0);

/** 暂停模型是本人的选择，不算要处理（§8.7）：暂停时模型信号本身为正常；积压里若只剩暂停期间积下的，也并进"已暂停"一项。 */
export function assess(r: HealthReport): Status {
  const model = r.signals.find((s) => s.id === "model_unavailable");
  const paused = (model?.detail as { paused?: boolean } | undefined)?.paused === true;
  const backlog = r.signals.find((s) => s.id === "unjudged_backlog");
  const pausedBacklog = num(backlog?.detail, "models_paused");
  const onlyPaused = (s: Signal) => paused && s.id === "unjudged_backlog" && !s.ok && s.count === pausedBacklog;
  const needs = r.signals.filter((s) => !s.ok && !onlyPaused(s));
  const fine = r.signals.filter((s) => s.ok);
  const kind = needs.some((s) => s.severity !== "warning") ? "bad" : needs.length ? "warn" : paused ? "paused" : "ok";
  return { kind, needs, fine, paused, pausedBacklog };
}

/** 导航上的状态灯：换页面或页面里数据变了（COUNTS_EVENT）时重取；取不到时为 null，不显示。 */
export function useStatus(api: ApiClient, pathname: string): Status | null {
  const [status, setStatus] = useState<Status | null>(null);
  useEffect(() => {
    const load = () => api.get<HealthReport>("/health").then(assess).then(setStatus, () => setStatus(null));
    load();
    window.addEventListener(COUNTS_EVENT, load);
    return () => window.removeEventListener(COUNTS_EVENT, load);
  }, [api, pathname]);
  return status;
}
