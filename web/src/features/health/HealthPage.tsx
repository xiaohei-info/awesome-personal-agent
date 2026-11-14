import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { useLabel } from "../../app/values";
import { assess, type HealthReport, type Signal } from "../../app/status";
import { strings } from "./strings";

// 诊断 › 状态（原「健康」，设计 §5.26）：形状对应 GET /v1/health 与 GET /v1/doctor（附录 B.2），判定全在服务端，这里只呈现。
// 只列需要处理的信号，每条一句结论加去处；正常的折叠；本人暂停模型是中性的「已暂停」，不算要处理（§8.7）。
export type { HealthReport, Signal } from "../../app/status";

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

type Go = keyof (typeof strings)["zh"]["go"];
interface Line { text: string; to?: string; go?: Go }

const n = (d: unknown, k: string) => Number((d as Record<string, unknown> | undefined)?.[k] ?? 0);
const list = <T,>(d: unknown) => (Array.isArray(d) ? (d as T[]) : []);

/** 一条信号的结论与去处：按 id 与 detail 生成（附录 B.2 的 detail 形状）。 */
function useLines(): (s: Signal, paused: boolean) => Line[] {
  const t = useT(strings);
  const source = useNames("source");
  return (s, paused) => {
    const d = s.detail;
    switch (s.id) {
      case "source_failures":
        return list<{ source: string; enabled: boolean; consecutive_failures: number; needs_input_item?: string }>(d).map((x) =>
          x.needs_input_item
            ? { text: t.sourceFail(source(x.source), x.consecutive_failures), to: "/items/" + x.needs_input_item, go: "fill" }
            : { text: x.enabled ? t.sourceFail(source(x.source), x.consecutive_failures) : t.sourceOff(source(x.source)), to: "/sources", go: "sources" },
        );
      case "outcall": {
        const q = (d as { long?: { queued: number; longest_wait_seconds: number }; short?: { queued: number; longest_wait_seconds: number } }) ?? {};
        const queued = (q.long?.queued ?? 0) + (q.short?.queued ?? 0);
        const wait = Math.max(q.long?.longest_wait_seconds ?? 0, q.short?.longest_wait_seconds ?? 0);
        return [{ text: t.outcall(s.count), to: "/runs", go: "runs" }, ...(queued ? [{ text: t.queue(queued, wait) }] : [])];
      }
      case "delivery_backlog":
        return [{ text: t.delivery(s.count), to: "/settings/notify", go: "notify" }];
      case "chain_trips": {
        const ids = list<string>(d);
        return [{ text: t.chain(s.count), to: ids.length === 1 ? "/items/" + ids[0] : "/items", go: "items" }];
      }
      case "agentsmd_drift":
        return [{ text: t.drift(s.count), to: "/projects", go: "projects" }];
      case "pack_rejections":
        return [{ text: t.packs(s.count) }, ...(s.rejections ?? []).map((r) => ({ text: `${r.pack}：${r.reason}` }))];
      case "model_unavailable": {
        const m = (d ?? {}) as { judge?: string; generate?: string; screening?: unknown[]; judgements?: unknown[] };
        const ends = (["generate", "judge"] as const).flatMap((k): Line[] => {
          const say = t.endpoint[m[k] ?? ""];
          return say ? [{ text: say(t.models[k]), to: "/settings/models", go: "models" }] : [];
        });
        const scr = list(m.screening).length, failed = list(m.judgements).length;
        return [
          ...ends,
          ...(scr ? [{ text: t.screening(scr) }] : []),
          ...(failed ? [{ text: t.judgeFails(failed), to: "/settings/models", go: "models" as Go }] : []),
        ];
      }
      case "unjudged_backlog":
        return [
          ...(n(d, "model_unavailable") + n(d, "undecided") ? [{ text: t.waitModel(n(d, "model_unavailable") + n(d, "undecided")), to: "/settings/models", go: "models" as Go }] : []),
          ...(n(d, "outbound_unset") ? [{ text: t.waitScope(n(d, "outbound_unset")), to: "/sources", go: "scope" as Go }] : []),
          // 暂停期间积下的：暂停时并进上面的「已暂停」卡；恢复后在这里列出，按重判期限自动判断。
          ...(!paused && n(d, "models_paused") ? [{ text: t.waitPaused(n(d, "models_paused")), to: "/sources", go: "sources" as Go }] : []),
          ...(n(d, "restored") ? [{ text: t.restored(n(d, "restored")), to: "/sources", go: "release" as Go }] : []),
          ...(n(d, "expired") ? [{ text: t.expired(n(d, "expired")) }] : []),
        ];
      case "rule_fallbacks":
        return [{ text: t.fallbacks(s.count), to: "/rules", go: "rules" }];
      case "receipt_failures":
        return [{ text: t.receipts(s.count), to: "/audit", go: "audit" }, { text: "", to: "/settings/access", go: "access" }];
      case "other_alerts":
        return [{ text: t.others(s.count), to: "/audit", go: "audit" }];
      case "stalled_delegations":
        return [{ text: t.stalled(s.count), to: "/delegation", go: "delegation" }];
      case "clock_lag":
        return [{ text: t.lag(s.count) }];
      default:
        return [{ text: s.why ?? s.id }];
    }
  };
}

function Card({ dot, title, children, raw }: { dot: string; title: string; children: ReactNode; raw?: string }) {
  return (
    <article aria-label={title} className="panel signal">
      <h2>
        <span className={"dot " + dot} aria-hidden="true" /> {title}
      </h2>
      {children}
      {raw && <code className="raw-id">{raw}</code>}
    </article>
  );
}

export function HealthPage() {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const lines = useLines();
  const { api } = useSession();
  const [report, setReport] = useState<HealthReport | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [doctor, setDoctor] = useState<DoctorReport | null>(null);

  const load = useCallback(() => {
    setError(null);
    // 自检是补充信息：取不到不影响状态的展示。
    api.get<DoctorReport>("/doctor").then(setDoctor, () => setDoctor(null));
    return api.get<HealthReport>("/health").then(setReport, (e) => setError(e));
  }, [api]);
  useEffect(() => {
    load();
  }, [load]);

  const st = report && assess(report);
  const head = !st ? t.title : st.kind === "ok" || st.kind === "paused" ? t.head[st.kind] : t.head[st.kind](st.needs.length);
  return (
    <main className="status-page">
      <PageHead>{head}</PageHead>
      {error != null && (
        <>
          <ErrorNote error={error} />
          <button onClick={load}>{t.retry}</button>
        </>
      )}
      {!st ? (
        error == null && <p role="status">{t.loading}</p>
      ) : (
        <>
          {st.paused && (
            <Card dot="paused" title={t.paused}>
              <p className="sub">{t.pausedHeld(st.pausedBacklog)}</p>
              <div className="actions">
                <Link className="btn" to="/settings/models">{t.resume}</Link>
              </div>
            </Card>
          )}
          {st.needs.map((s) => {
            const ls = lines(s, st.paused);
            return (
              <Card key={s.id} dot={s.severity === "warning" ? "warn" : "bad"} title={label("healthSignal", s.id)} raw={raw ? `${s.id} · ${s.why ?? ""} · ${s.next}` : undefined}>
                <ul className="lines">
                  {ls.map((l, i) => (
                    <li key={i}>
                      {l.text && <span>{l.text}</span>}
                      {l.to && l.go && <Link className="btn" to={l.to}>{t.go[l.go]}</Link>}
                    </li>
                  ))}
                </ul>
              </Card>
            );
          })}
          {st.fine.length > 0 && (
            <details className="panel">
              <summary>
                <span className="dot ok" aria-hidden="true" /> {st.needs.length || st.paused ? t.fine(st.fine.length) : t.allFine(st.fine.length)}
              </summary>
              <ul className="sub-list">
                {st.fine.map((s) => <li key={s.id}>{label("healthSignal", s.id)}</li>)}
              </ul>
            </details>
          )}
          {doctor && (
            <details className="panel">
              <summary>{t.doctor}</summary>
              {doctor.checks.map((c) => (
                <article key={c.name} aria-label={label("doctorCheck", c.name)} className="signal">
                  <h3>
                    {label("doctorCheck", c.name)}
                    <span className={"badge " + STATUS_BADGE[c.status]}>{t.status[c.status]}</span>
                  </h3>
                  <p lang="en" className="sub">{c.why}</p>
                  {c.details?.map((d) => (
                    <p key={d.subject} lang="en" className="sub">
                      {t.detail(d.subject, t.status[d.status], d.why)}
                    </p>
                  ))}
                  {c.status !== "ok" && <p lang="en" className="sub">{c.next}</p>}
                </article>
              ))}
            </details>
          )}
        </>
      )}
    </main>
  );
}
