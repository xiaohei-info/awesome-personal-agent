import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 GET /v1/health（api/health.Report）：十项信号、顺序固定，判定全在服务端；这里只呈现。
export interface Signal {
  id: string;
  ok: boolean;
  count: number;
  why?: string;
  next: string;
  detail?: unknown;
}
export interface HealthReport {
  healthy: boolean;
  signals: Signal[];
}

// 信号 id → 标题与可跳转的修复入口（设计 §15.2）。能力包加载失败的修复是"修包后热加载"，没有 Web 页面，只显示接口给出的说明。
const META: Record<string, { title: string; links: { to: string; label: string }[] }> = {
  source_failures: { title: "来源失败", links: [{ to: "/sources", label: "来源" }] },
  outcall: { title: "外呼超时与排队", links: [{ to: "/runs", label: "运行记录" }, { to: "/settings", label: "策略与人格" }] },
  delivery_backlog: { title: "投递积压", links: [{ to: "/settings", label: "策略与人格" }] },
  chain_trips: { title: "链路熔断", links: [{ to: "/items", label: "事项" }] },
  agentsmd_drift: { title: "AGENTS.md 漂移", links: [{ to: "/projects", label: "项目" }] },
  pack_rejections: { title: "能力包加载失败", links: [] },
  model_unavailable: { title: "模型不可用与未判定积压", links: [{ to: "/settings", label: "策略与人格" }, { to: "/sources", label: "来源" }] },
  rule_fallbacks: { title: "规则回退 observe", links: [{ to: "/rules", label: "规则模式" }] },
  receipt_failures: { title: "回执与捕获失败", links: [{ to: "/audit", label: "审计" }, { to: "/settings", label: "策略与人格" }, { to: "/decisions", label: "决策待办" }] },
  other_alerts: { title: "其他健康告警", links: [{ to: "/audit", label: "审计" }] },
};

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

interface Queue {
  running: number;
  queued: number;
  longest_wait_seconds: number;
}

function Detail({ s }: { s: Signal }) {
  const d = s.detail as any;
  if (d == null || s.ok) return null;
  if (s.id === "outcall") {
    const q = (name: string, x: Queue) => <li key={name}>{`${name}：运行 ${x.running}，排队 ${x.queued}，最长等待 ${x.longest_wait_seconds} 秒`}</li>;
    return (
      <ul>
        {q("长时运行", d.long)}
        {q("短任务", d.short)}
        {d.timeouts?.length > 0 && <li>超时运行：{d.timeouts.join("、")}</li>}
      </ul>
    );
  }
  if (s.id === "source_failures")
    return (
      <ul>
        {(d as { source: string; enabled: boolean; consecutive_failures: number; needs_input_item?: string }[]).map((x) => (
          <li key={x.source}>
            {x.source}：连续失败 {x.consecutive_failures} 次{x.enabled ? "" : "，已停用"}
            {x.needs_input_item ? `，待补充输入事项 ${x.needs_input_item}` : ""}
          </li>
        ))}
      </ul>
    );
  if (s.id === "model_unavailable")
    return (
      <ul>
        {d.screening?.length > 0 && <li>初筛失败或超时：{d.screening.join("、")}</li>}
        {d.unjudged?.length > 0 && <li>未判定来源事件：{d.unjudged.join("、")}</li>}
      </ul>
    );
  return Array.isArray(d) ? <p>涉及：{d.join("、")}</p> : null;
}

export function HealthPage() {
  const { api } = useSession();
  const [report, setReport] = useState<HealthReport | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    setError(null);
    return api.get<HealthReport>("/health").then(setReport, (e) => setError(msg(e)));
  }, [api]);

  useEffect(() => {
    load();
  }, [load]);

  const bad = report?.signals.filter((s) => !s.ok).length ?? 0;
  return (
    <main>
      {report === null ? (
        <PageHead>健康</PageHead>
      ) : (
        <PageHead lede={`${report.signals.length} 项信号${bad ? `里有 ${bad} 项需要看一眼。` : "都正常。"}`}>
          {report.healthy ? "一切正常" : `存在 ${bad} 类异常`}
        </PageHead>
      )}
      {error && (
        <div role="alert">
          {error} <button onClick={load}>重试</button>
        </div>
      )}
      {report === null ? (
        !error && <p role="status">加载中…</p>
      ) : (
        <>
          {report.signals.map((s) => {
            const m = META[s.id] ?? { title: s.id, links: [] };
            return (
              <article key={s.id} aria-label={m.title} className="signal">
                <h3>
                  {m.title}
                  <span className={"badge " + (s.ok ? "ok" : "bad")}>{s.ok ? "正常" : `异常（${s.count}）`}</span>
                </h3>
                {!s.ok && s.why && <p>{s.why}</p>}
                <Detail s={s} />
                <p className="sub">
                  修复入口：{s.next}
                  {!s.ok && m.links.map((l) => (
                    <span key={l.to}>
                      {" "}
                      <Link to={l.to}>{l.label}</Link>
                    </span>
                  ))}
                </p>
              </article>
            );
          })}
        </>
      )}
    </main>
  );
}
