import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/rules、PATCH /v1/rules/{id}/mode（附录 B.2）与 GET /v1/audit/events（§14.8）。
// 谁能切、切到什么、观察记录怎么产生都由服务端判定；本页只搬运选择并原样显示拒绝原因，被拒绝时模式保持原值。只改模式，不编辑规则内容。
const MODES = ["off", "observe", "auto"];
interface Rule { key: string; pack: string; group: string; id: string; mode: string }
interface Observed { seq: number; object: string; at: string; reason?: string; payload?: { would_do?: unknown } }


function Observations({ ruleKey }: { ruleKey: string }) {
  const { api } = useSession();
  const t = useT(strings);
  const [events, setEvents] = useState<Observed[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    const q = new URLSearchParams({ type: "rule.observed", actor: "rule:" + ruleKey, limit: "100" });
    api.get<{ events: Observed[] }>("/audit/events?" + q).then((r) => setEvents(r.events), (e) => setError(msg(e)));
  }, [api, ruleKey]);
  if (error) return <p role="alert">{error}</p>;
  if (!events) return <p role="status">{t.loading}</p>;
  if (!events.length) return <p className="sub">{t.noObservations}</p>;
  return (
    <ul aria-label={t.observations(ruleKey)} className="rows">
      {events.map((e) => (
        <li key={e.seq}>
          #{e.seq} · {e.object} · {new Date(e.at).toLocaleString(locale())}
          {e.reason ? ` · ${e.reason}` : ""}
          {e.payload?.would_do !== undefined ? t.wouldDo(JSON.stringify(e.payload.would_do)) : ""}
        </li>
      ))}
    </ul>
  );
}

export function RulesPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [rules, setRules] = useState<Rule[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setRules((await api.get<{ rules: Rule[] }>("/rules")).rules);
      setError(null);
    } catch (e) {
      setRules(null);
      setError(msg(e));
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);

  async function setMode(r: Rule, mode: string) {
    try {
      const next = await api.patch<Rule>(`/rules/${encodeURIComponent(r.key)}/mode`, { mode });
      setRules((l) => l!.map((x) => (x.key === r.key ? next : x)));
      setError(null);
    } catch (e) {
      setError(msg(e)); // 受控 select 的值仍是 r.mode，即服务端未改动的原模式
    }
  }

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error && <p role="alert">{error}</p>}
      {!rules && !error && <p role="status">{t.loading}</p>}
      {rules && !rules.length && <p className="empty">{t.none}</p>}
      {rules?.map((r) => (
        <section key={r.key} aria-label={r.key} className="panel">
          <h3>{r.key}</h3>
          <p className="sub">{r.pack} · {r.group}</p>
          <div className="actions">
            <select aria-label={t.mode(r.key)} value={r.mode} onChange={(e) => void setMode(r, e.target.value)}>
              {MODES.map((m) => <option key={m}>{m}</option>)}
            </select>
            <button type="button" aria-label={t.review(r.key)} onClick={() => setOpen(open === r.key ? null : r.key)}>
              {open === r.key ? t.hideObs : t.showObs}
            </button>
          </div>
          {open === r.key && <Observations ruleKey={r.key} />}
        </section>
      ))}
    </main>
  );
}
