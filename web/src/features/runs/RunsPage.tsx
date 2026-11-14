import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/runs?state=&limit=、GET /v1/runs/{id}（设计 §6.11）。只读展示，不做任何判定；失败处理走 needs_input，不提供重试。
interface Run {
  id: string;
  category: string;
  runner?: string;
  launch?: string;
  target: string;
  harness?: string;
  session_id?: string;
  access?: string;
  basis?: { policy_version: number; rule_id?: string };
  chain_depth: number;
  state: string;
  exit_code?: number;
  started_at?: string;
  finished_at?: string;
  timeout?: number;
}

const LIMIT = 200;

const at = (s?: string) => (s ? new Date(s).toLocaleString(locale()) : "—");

function Card({ r }: { r: Run }) {
  const { api } = useSession();
  const t = useT(strings);
  const [detail, setDetail] = useState<Run | null>(null);
  const [error, setError] = useState<string | null>(null);
  const d = detail ?? r;

  const open = async () => {
    setError(null);
    try {
      setDetail(await api.get<Run>(`/runs/${encodeURIComponent(r.id)}`));
    } catch (e) {
      setError(msg(e));
    }
  };

  return (
    <article aria-label={t.run(r.id)}>
      <h3>
        {r.category} → {r.target}
      </h3>
      <p className="meta">
        <strong className={"badge" + (r.state === "succeeded" ? " ok" : r.state === "failed" || r.state === "timeout" ? " bad" : "")}>{t.states[r.state] ?? r.state}</strong>
        {r.runner && ` · ${r.runner}/${r.launch ?? "—"}`}
        {r.harness && ` · ${r.harness}`}
      </p>
      <p>
        {t.session}
        {r.session_id ? <code>{r.session_id}</code> : t.none}
      </p>
      <p>
        {t.access}
        {r.access ? <code>{r.access}</code> : t.none}
      </p>
      {r.basis ? (
        <p>
          {t.basis}
          {t.basisRule(r.basis.rule_id ?? "—", r.basis.policy_version)}
        </p>
      ) : (
        <p>
          {t.basis}
          {t.none}
        </p>
      )}
      {!detail && <div className="actions"><button onClick={open}>{t.details}</button></div>}
      {detail && (
        <ul aria-label={t.stats}>
          <li>{t.depth(d.chain_depth)}</li>
          <li>{t.started(at(d.started_at))}</li>
          <li>{t.finished(at(d.finished_at))}</li>
          {d.exit_code !== undefined && <li>{t.exitCode(d.exit_code)}</li>}
          {d.timeout ? <li>{t.timeoutSec(d.timeout)}</li> : null}
        </ul>
      )}
      {!detail && r.exit_code !== undefined && <p>{t.exitCode(r.exit_code)}</p>}
      {error && <div role="alert">{error}</div>}
    </article>
  );
}

export function RunsPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [state, setState] = useState("");
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const q = `?limit=${LIMIT}` + (state ? `&state=${state}` : "");
      setRuns((await api.get<{ runs: Run[] }>("/runs" + q)).runs);
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api, state]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      <div className="filters">
      <label>
        {t.state}
        <select aria-label={t.state} value={state} onChange={(e) => setState(e.target.value)}>
          <option value="">{t.all}</option>
          {Object.entries(t.states).map(([k, v]) => (
            <option key={k} value={k}>
              {v}
            </option>
          ))}
        </select>
      </label>
      </div>
      {error && (
        <div role="alert">
          {error} <button onClick={load}>{t.reload}</button>
        </div>
      )}
      {runs === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : runs.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        [...runs].reverse().map((r) => <Card key={r.id} r={r} />)
      )}
    </main>
  );
}
