import { useCallback, useEffect, useState } from "react";
import { EmptyState, ErrorBoundary, Skeleton } from "../../app/ui";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { Val } from "../../app/values";
import { strings } from "./strings";

// 形状对应 GET /v1/executors、GET /v1/items?kind=delegation、GET /v1/audit/events?type=delegation.reported、
// GET /v1/dispatches?resumable=true、DELETE /v1/executors/{id}、PUT /v1/executors/{id}/overrides（只 Web）。
// 分组与判定都只按服务端给的字段，不在前端推断：能不能由 APA 直接派活（startable）、能换哪些参数与取值（overridable）都来自声明。
interface Label { zh: string; en: string }
interface Overridable { label: Label; choices: string[] }
interface Executor {
  id: string; kind: string; host?: string; harness?: string; label?: string;
  startable?: boolean; overridable?: Record<string, Overridable>; overrides?: Record<string, string>;
}
interface Child { id: string; phase: string; title: string; assignee?: string }
interface Evidence { commit?: string; pr?: string; branch?: string; link?: string }
interface Step { seq: number; object: string; at: string; payload?: { report?: { outcome: string; summary: string; evidence: Evidence } } }
interface Dispatch { id: string; item: string; executor: string; session?: string; state: string; resumable: boolean }

const PAGE = 500;
const TERMINAL = new Set(["done", "dropped"]);

interface Card { e: Executor; listed: boolean; open: Child[]; last?: Step; sessions: Dispatch[] }

export function ExecutorsPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [cards, setCards] = useState<Card[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const [{ executors }, { items }, { dispatches }] = await Promise.all([
        api.get<{ executors: Executor[] }>("/executors"),
        api.get<{ items: Child[] }>("/items?kind=delegation"),
        api.get<{ dispatches: Dispatch[] }>("/dispatches?resumable=true&limit=200"),
      ]);
      const latest = new Map<string, Step>();
      for (let after = 0; ; ) {
        const { events } = await api.get<{ events: Step[] }>(`/audit/events?type=delegation.reported&after=${after}&limit=${PAGE}`);
        for (const s of events) latest.set(s.object, s);
        if (events.length < PAGE) break;
        after = events[events.length - 1].seq;
      }
      const byId = new Map<string, Card>(executors.map((e) => [e.id, { e, listed: true, open: [], sessions: [] }]));
      const card = (id: string) => {
        if (!byId.has(id)) byId.set(id, { e: { id, kind: "" }, listed: false, open: [], sessions: [] });
        return byId.get(id)!;
      };
      for (const c of items) {
        if (!c.assignee) continue;
        const k = card(c.assignee);
        if (!TERMINAL.has(c.phase)) k.open.push(c);
        const r = latest.get(c.id);
        if (r && (!k.last || r.seq > k.last.seq)) k.last = r;
      }
      for (const d of dispatches) if (d.state !== "lost") card(d.executor).sessions.push(d);
      setCards([...byId.values()].sort((a, b) => b.open.length - a.open.length || (a.e.label || a.e.id).localeCompare(b.e.label || b.e.id)));
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);

  useEffect(() => {
    load();
  }, [load]);

  const setDefault = async (e: Executor, key: string, value: string) => {
    try {
      await api.put("/executors/" + encodeURIComponent(e.id) + "/overrides", { values: { ...(e.overrides ?? {}), [key]: value } });
      await load();
    } catch (err) {
      setError(msg(err));
    }
  };

  const remove = async (id: string) => {
    try {
      await api.del("/executors/" + encodeURIComponent(id));
      await load();
    } catch (e) {
      setError(msg(e));
    }
  };

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error && <div role="alert">{error}</div>}
      {cards === null ? (
        !error && <Skeleton label={t.loading} />
      ) : cards.length === 0 ? (
        <EmptyState title={t.empty} />
      ) : (
        cards.map(({ e, listed, open, last, sessions }) => (
          <ErrorBoundary key={e.id}>
          <article className="card" aria-label={e.label || e.id}>
            <h2>{e.label || e.id}</h2>
            <p className="sub">
              {[e.host, e.harness].filter(Boolean).join(" · ")}
              {e.kind && <> · <Val k="executorKind" id={e.kind} /></>}
              {!listed && t.notListed}
              {e.startable && <> · <span className="badge">{t.startable}</span></>}
            </p>
            {e.startable && Object.keys(e.overridable ?? {}).length > 0 && (
              <div className="row">
                {Object.entries(e.overridable!).sort(([a], [b]) => a.localeCompare(b)).map(([key, o]) => (
                  <label key={key}>
                    {locale().startsWith("zh") ? o.label.zh : o.label.en}{" "}
                    <select value={e.overrides?.[key] ?? ""} onChange={(ev) => void setDefault(e, key, ev.target.value)}>
                      <option value="">{t.asDeclared}</option>
                      {o.choices.map((c) => <option key={c} value={c}>{c}</option>)}
                    </select>
                  </label>
                ))}
              </div>
            )}
            <h3>{t.inHand}</h3>
            {open.length ? (
              <ul className="plain">{open.map((c) => <li key={c.id}><Link to={"/items/" + c.id}>{c.title}</Link></li>)}</ul>
            ) : (
              <p className="sub">{t.noneInHand}</p>
            )}
            <h3>{t.lastResult}</h3>
            {last?.payload?.report ? (
              <p>
                <Link to={"/items/" + last.object}>{last.payload.report.summary}</Link>{" "}
                <span className="sub">{new Date(last.at).toLocaleString(locale())}</span>
                {Object.entries(last.payload.report.evidence ?? {}).filter(([, v]) => v).map(([k, v]) => (
                  <span key={k} className="sub"> · {v!.startsWith("http") ? <a href={v} target="_blank" rel="noreferrer">{k}</a> : v}</span>
                ))}
              </p>
            ) : (
              <p className="sub">{t.noResult}</p>
            )}
            {sessions.length > 0 && (
              <>
                <h3>{t.sessions}</h3>
                <ul className="plain">{sessions.map((d) => <li key={d.id}><Link to={"/items/" + d.item}>{d.session ?? d.id}</Link></li>)}</ul>
              </>
            )}
            {listed && open.length === 0 && (
              <div><button className="danger" onClick={() => remove(e.id)}>{t.remove}</button></div>
            )}
          </article>
          </ErrorBoundary>
        ))
      )}
    </main>
  );
}
