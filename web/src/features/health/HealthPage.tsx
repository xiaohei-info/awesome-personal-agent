import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/health（api/health.Report）：十一项信号、顺序固定，判定全在服务端；这里只呈现。
// severity 为 warning 的是待本人处理的提醒（如未判定积压），不算异常。
export interface Signal {
  id: string;
  ok: boolean;
  count: number;
  severity?: string; // "error" 或 "warning"
  why?: string;
  next: string;
  detail?: unknown;
}
export interface HealthReport {
  healthy: boolean;
  signals: Signal[];
}

// 信号 id → 标题与可跳转的修复入口（设计 §15.2）。能力包加载失败的修复是"修包后热加载"，没有 Web 页面，只显示接口给出的说明。
type Page = keyof (typeof strings)["zh"]["pages"];
const META: Record<string, { to: string; page: Page }[]> = {
  source_failures: [{ to: "/sources", page: "sources" }],
  outcall: [{ to: "/runs", page: "runs" }, { to: "/settings/models", page: "models" }],
  delivery_backlog: [{ to: "/settings", page: "policy" }],
  chain_trips: [{ to: "/items", page: "items" }],
  agentsmd_drift: [{ to: "/projects", page: "projects" }],
  pack_rejections: [],
  model_unavailable: [{ to: "/settings/models", page: "models" }],
  unjudged_backlog: [{ to: "/sources", page: "sources" }],
  rule_fallbacks: [{ to: "/rules", page: "rules" }],
  receipt_failures: [{ to: "/audit", page: "audit" }, { to: "/settings", page: "policy" }, { to: "/decisions", page: "decisions" }],
  other_alerts: [{ to: "/audit", page: "audit" }],
};

// 形状对应 GET /v1/doctor（kernel/doctor.Report）：内核一侧的自检，结论与 `apa doctor` 的对应项同源；这里只呈现。
export interface DoctorDetail {
  subject: string;
  status: "ok" | "warning" | "error";
  why: string;
  next?: string;
}
export interface DoctorCheck {
  name: string;
  status: "ok" | "warning" | "error";
  why: string;
  next: string;
  details?: DoctorDetail[];
}
export interface DoctorReport {
  ok: boolean;
  checks: DoctorCheck[];
}
const STATUS_BADGE = { ok: "ok", warning: "warn", error: "bad" } as const;


interface Queue {
  running: number;
  queued: number;
  longest_wait_seconds: number;
}

function Detail({ s }: { s: Signal }) {
  const t = useT(strings);
  const d = s.detail as any;
  if (d == null || s.ok) return null;
  if (s.id === "outcall") {
    const q = (name: string, x: Queue) => <li key={name}>{t.queue(name, x.running, x.queued, x.longest_wait_seconds)}</li>;
    return (
      <ul>
        {q(t.long, d.long)}
        {q(t.short, d.short)}
        {d.timeouts?.length > 0 && <li>{t.timeouts(d.timeouts.join(t.listSep))}</li>}
      </ul>
    );
  }
  if (s.id === "source_failures")
    return (
      <ul>
        {(d as { source: string; enabled: boolean; consecutive_failures: number; needs_input_item?: string }[]).map((x) => (
          <li key={x.source}>
            {t.sourceFail(x.source, x.consecutive_failures)}
            {x.enabled ? "" : t.disabled}
            {x.needs_input_item ? t.needsInput(x.needs_input_item) : ""}
          </li>
        ))}
      </ul>
    );
  if (s.id === "model_unavailable")
    return (
      <ul>
        {d.endpoint === "not_configured" && <li>{t.notConfigured}</li>}
        {d.endpoint === "failing" && <li>{t.failing}</li>}
        {d.screening?.length > 0 && <li>{t.screening(d.screening.join(t.listSep))}</li>}
        {d.judgements?.length > 0 && <li>{t.judgeFails(d.judgements.length)}</li>}
      </ul>
    );
  if (s.id === "unjudged_backlog")
    return (
      <ul>
        {d.model_unavailable > 0 && <li>{t.waitModel(d.model_unavailable)}</li>}
        {d.undecided > 0 && <li>{t.undecided(d.undecided)}</li>}
        {d.sources?.length > 0 && <li>{t.bySource(d.sources.map((x: { source: string; count: number }) => t.sourceCount(x.source, x.count)).join(t.listSep))}</li>}
        {d.expired > 0 && <li>{t.expired(d.expired)}</li>}
      </ul>
    );
  return Array.isArray(d) ? <p>{t.involved(d.join(t.listSep))}</p> : null;
}

export function HealthPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [report, setReport] = useState<HealthReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [doctor, setDoctor] = useState<DoctorReport | null>(null);

  const load = useCallback(() => {
    setError(null);
    // 自检是补充信息：取不到不影响健康信号的展示。
    api.get<DoctorReport>("/doctor").then(setDoctor, () => setDoctor(null));
    return api.get<HealthReport>("/health").then(setReport, (e) => setError(msg(e)));
  }, [api]);

  useEffect(() => {
    load();
  }, [load]);

  const bad = report?.signals.filter((s) => !s.ok && s.severity !== "warning").length ?? 0;
  const warn = report?.signals.filter((s) => !s.ok && s.severity === "warning").length ?? 0;
  return (
    <main>
      {report === null ? (
        <PageHead>{t.title}</PageHead>
      ) : (
        <PageHead lede={t.lede(report.signals.length, bad, warn)}>
          {t.head(report.healthy, bad, warn)}
        </PageHead>
      )}
      {error && (
        <div role="alert">
          {error} <button onClick={load}>{t.retry}</button>
        </div>
      )}
      {report === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : (
        <>
          {report.signals.map((s) => {
            const title = t.signals[s.id] ?? s.id;
            const links = META[s.id] ?? [];
            return (
              <article key={s.id} aria-label={title} className="signal">
                <h3>
                  {title}
                  <span className={"badge " + (s.ok ? "ok" : s.severity === "warning" ? "warn" : "bad")}>
                    {s.ok ? t.ok : s.severity === "warning" ? t.pending(s.count) : t.abnormal(s.count)}
                  </span>
                </h3>
                {!s.ok && s.why && <p>{s.why}</p>}
                <Detail s={s} />
                <p className="sub">
                  {t.fix(s.next)}
                  {!s.ok && links.map((l) => (
                    <span key={l.to}>
                      {" "}
                      <Link to={l.to}>{t.pages[l.page]}</Link>
                    </span>
                  ))}
                </p>
              </article>
            );
          })}
          {doctor && (
            <section aria-label={t.doctor}>
              <h2>{t.doctor}</h2>
              {doctor.checks.map((c) => (
                <article key={c.name} aria-label={t.doctorCheck(c.name)} className="signal">
                  <h3>
                    {c.name}
                    <span className={"badge " + STATUS_BADGE[c.status]}>{t.status[c.status]}</span>
                  </h3>
                  <p>{c.why}</p>
                  {c.details?.map((d) => (
                    <p key={d.subject} className="sub">
                      {t.detail(d.subject, t.status[d.status], d.why)}
                    </p>
                  ))}
                  {c.status !== "ok" && <p className="sub">{t.next(c.next)}</p>}
                </article>
              ))}
            </section>
          )}
        </>
      )}
    </main>
  );
}
