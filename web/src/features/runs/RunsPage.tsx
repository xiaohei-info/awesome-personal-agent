import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { Link } from "react-router-dom";
import { ErrorNote } from "../../app/errors";
import { useItemTitle, useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { strings } from "./strings";
import { RUN_STATE } from "../../app/ids";
import { Pick, Val, useLabel } from "../../app/values";

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

function Target({ r }: { r: Run }) {
  const source = useNames("source");
  const title = useItemTitle(r.category === "source" ? null : r.target);
  return r.category === "source" ? <>{source(r.target)}</> : <Link to={"/items/" + r.target}>{title}</Link>;
}

const took = (a?: string, b?: string) => {
  if (!a || !b) return "";
  const s = Math.max(0, Math.round((new Date(b).getTime() - new Date(a).getTime()) / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60 ? ` ${s % 60}s` : ""}`;
};

function Card({ r }: { r: Run }) {
  const { api } = useSession();
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const [detail, setDetail] = useState<Run | null>(null);
  const [error, setError] = useState<unknown>(null);
  const d = detail ?? r;

  const open = async () => {
    setError(null);
    try {
      setDetail(await api.get<Run>(`/runs/${encodeURIComponent(r.id)}`));
    } catch (e) {
      setError(e);
    }
  };

  return (
    <article aria-label={t.run(r.id)} className="panel">
      <h3>
        <Val k="runCategory" id={r.category} /> → <Target r={r} />
      </h3>
      <p className="meta">
        <strong className={"badge" + (r.state === "succeeded" ? " ok" : r.state === "failed" || r.state === "timeout" ? " bad" : "")}><Val k="runState" id={r.state} /></strong>
        {r.runner && <> · <Val k="runner" id={r.runner} />{r.launch && <> · <Val k="launch" id={r.launch} /></>}</>}
        {r.access && ` · ${t.access(label("perm", r.access))}`}
        {took(r.started_at, r.finished_at) && ` · ${t.took(took(r.started_at, r.finished_at))}`}
      </p>
      {!detail && <div className="actions"><button onClick={open}>{t.details}</button></div>}
      {detail && (
        <ul aria-label={t.stats} className="sub-list">
          {d.basis?.rule_id && <li>{t.rule(d.basis.rule_id)}</li>}
          {d.session_id && <li>{t.session(d.session_id)}</li>}
          <li>{t.started(at(d.started_at))}</li>
          <li>{t.finished(at(d.finished_at))}</li>
          {d.chain_depth > 0 && <li>{t.depth(d.chain_depth)}</li>}
          {d.exit_code !== undefined && <li>{t.exitCode(d.exit_code)}</li>}
          {d.timeout ? <li>{t.timeoutSec(d.timeout)}</li> : null}
        </ul>
      )}
      {raw && <code className="raw-id">{[r.id, r.harness, d.basis ? `policy v${d.basis.policy_version}` : ""].filter(Boolean).join(" · ")}</code>}
      {error != null && <ErrorNote error={error} />}
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
        <Pick k="runState" label={t.state} value={state} onChange={setState} ids={RUN_STATE} empty={t.all} />
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
