import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useItemTitle, useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { useLabel, Val } from "../../app/values";
import { strings } from "./strings";

// 形状对应 GET /v1/runs?state=&category=&before=&limit=（设计 §6.11，附录 B.2）：按入队顺序升序，本页倒过来当日志读。
// 只读展示，不做任何判定；失败处理走 needs_input，不提供重试。
interface Run {
  agent_name?: string; configured_model?: string; permission_cap?: string; selection_source?: string;
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

const PAGE = 100;
const KINDS = { all: "", source: "source", agent: "task,background" } as const;
type Kind = keyof typeof KINDS;
const FAILED = "failed,timeout";

const at = (s?: string) => (s ? new Date(s).toLocaleString(locale()) : "—");
const took = (a?: string, b?: string) => {
  if (!a || !b) return "";
  const s = Math.max(0, Math.round((new Date(b).getTime() - new Date(a).getTime()) / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60 ? ` ${s % 60}s` : ""}`;
};
const tone = (state: string) => (state === "succeeded" ? " ok" : state === "failed" ? " bad" : state === "timeout" ? " warn" : "");

const isItemRun = (r: Run) => r.category === "background" || r.category === "task";

/** 同一来源连续成功的拉取折成一组；失败、超时与其他运行永远单独一行。 */
function fold(runs: Run[]): Run[][] {
  const out: Run[][] = [];
  for (const r of runs) {
    const last = out[out.length - 1];
    const quiet = r.category === "source" && r.state === "succeeded";
    if (quiet && last && last[0].category === "source" && last[0].state === "succeeded" && last[0].target === r.target) last.push(r);
    else out.push([r]);
  }
  return out;
}

function What({ r, n }: { r: Run; n?: number }) {
  const t = useT(strings);
  const label = useLabel();
  const source = useNames("source");
  const title = useItemTitle(isItemRun(r) ? r.target : null);
  if (r.category === "source") return <>{t.pull(source(r.target))}{n ? ` · ${t.pullOk(n)}` : ""}</>;
  if (r.category === "background") return <>{t.background(title)}</>;
  if (r.category === "task") return <>{t.task(title)}</>;
  return <>{label("runCategory", r.category)}</>;
}

function Detail({ r }: { r: Run }) {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  return (
    <div className="log-detail">
      <ul aria-label={t.log} className="sub-list">
        {r.agent_name && <li>{r.agent_name} · {r.runner === "main_session" ? t.mainRole : t.taskRole} · {r.launch === "wake" ? t.wake : t.headless} · {t.cap(label("perm", r.permission_cap ?? "read"))} · {t.sources[r.selection_source as keyof typeof t.sources]}</li>}
        {r.configured_model && <li>{t.configuredModel(r.configured_model)}</li>}
        {r.basis?.rule_id && <li>{t.rule(r.basis.rule_id)}</li>}
        {r.session_id && <li>{t.session(r.session_id)}</li>}
        {r.access && <li>{t.access(label("perm", r.access))}</li>}
        {r.started_at && <li>{t.started(at(r.started_at))}</li>}
        {r.finished_at && <li>{t.finished(at(r.finished_at))}</li>}
        {r.chain_depth > 0 && <li>{t.depth(r.chain_depth)}</li>}
        {r.exit_code !== undefined && <li>{t.exitCode(r.exit_code)}</li>}
        {r.timeout ? <li>{t.timeoutSec(r.timeout)}</li> : null}
        {isItemRun(r) && <li><Link to={"/items/" + r.target}>{t.openItem}</Link></li>}
      </ul>
      {raw && <code className="raw-id">{[r.id, r.target, r.harness, r.runner, r.launch, r.basis ? `policy v${r.basis.policy_version}` : ""].filter(Boolean).join(" · ")}</code>}
    </div>
  );
}

/** 一行日志：时间 · 做了什么 · 结果 · 用时；整行可点，就地展开细节。group 多于一条时是折叠的连续成功拉取。 */
function Line({ runs }: { runs: Run[] }) {
  const [open, setOpen] = useState(false);
  const r = runs[0];
  const many = runs.length > 1;
  const d = took(r.started_at, r.finished_at);
  return (
    <li>
      <button type="button" className="quiet log-line" aria-expanded={open} onClick={() => setOpen(!open)}>
        <time className="when" dateTime={r.started_at ?? r.finished_at}>{at(r.started_at ?? r.finished_at)}</time>
        <span className="what">{r.agent_name && r.agent_name + " · "}<What r={r} n={many ? runs.length : undefined} /></span>
        <strong className={"badge result" + tone(r.state)}><Val k="runState" id={r.state} /></strong>
        <span className="took">{many ? "" : d}</span>
      </button>
      {open && (many ? (
        <ul className="log-children">{runs.map((x) => <Line key={x.id} runs={[x]} />)}</ul>
      ) : (
        <Detail r={r} />
      ))}
    </li>
  );
}

export function RunsPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [more, setMore] = useState(false);
  const [kind, setKind] = useState<Kind>("all");
  const [failedOnly, setFailedOnly] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const seq = useRef(0); // 换筛选后丢弃迟到的旧响应

  const load = useCallback(async (before?: string) => {
    const me = ++seq.current;
    const p = new URLSearchParams({ limit: String(PAGE) });
    if (KINDS[kind]) p.set("category", KINDS[kind]);
    if (failedOnly) p.set("state", FAILED);
    if (before) p.set("before", before);
    try {
      const page = (await api.get<{ runs: Run[] }>("/runs?" + p)).runs;
      if (me !== seq.current) return;
      setError(null);
      setMore(page.length === PAGE);
      setRuns((prev) => (before ? [...(prev ?? []), ...page.reverse()] : page.reverse()));
    } catch (e) {
      if (me !== seq.current) return;
      setRuns(null); // 出错时不保留旧列表，避免把过期结果当成当前筛选的结果
      setMore(false);
      setError(e);
    }
  }, [api, kind, failedOnly]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <div className="filters">
        <label>
          {t.kind}
          <select value={kind} onChange={(e) => setKind(e.target.value as Kind)}>
            {(Object.keys(KINDS) as Kind[]).map((k) => <option key={k} value={k}>{t.kinds[k]}</option>)}
          </select>
        </label>
        <label>
          <input type="checkbox" checked={failedOnly} onChange={(e) => setFailedOnly(e.target.checked)} /> {t.failedOnly}
        </label>
      </div>
      {error != null && (
        <div>
          <ErrorNote error={error} /> <button onClick={() => void load()}>{t.reload}</button>
        </div>
      )}
      {runs === null ? (
        error == null && <p role="status">{t.loading}</p>
      ) : runs.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        <ul aria-label={t.log} className="rows log">
          {fold(runs).map((g) => <Line key={g[0].id} runs={g} />)}
        </ul>
      )}
      {more && runs && <button onClick={() => void load(runs[runs.length - 1].id)}>{t.more}</button>}
    </main>
  );
}
