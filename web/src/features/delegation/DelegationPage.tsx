import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { common } from "../../app/strings";
import { codeText, lang, locale, useT } from "../../app/i18n";
import { strings } from "./strings";
import { Val, useLabel } from "../../app/values";

// 形状对应 GET /v1/items?kind=delegation、GET /v1/audit/events?type=delegation.reported、GET /v1/executors?project=、
// PATCH /v1/items/{id}（改派，If-Match）、POST /v1/delegations（重新委派）。
// 项目允许的执行者（§14.7）由服务端校验；本页只显示"是否允许"提示与服务端拒绝的原因，不在前端做判定。
interface Child {
  id: string;
  phase: string;
  version: number;
  title: string;
  status_label?: string;
  project?: string;
  assignee?: string;
}
interface Evidence { commit?: string; pr?: string; branch?: string; link?: string }
interface Report { outcome: string; summary: string; evidence: Evidence }
interface Step { seq: number; type: string; object: string; at: string; payload?: { report?: Report } }
interface Executor { id: string; kind: string; host?: string; allowed?: boolean }

const PAGE = 500;
const TERMINAL = new Set(["done", "dropped"]);

const msg = (e: unknown) => {
  if (!(e instanceof ApiError)) return e instanceof Error ? e.message : "request failed";
  const t = strings[lang()];
  const allowed = e.body.allowed ? t.allowedTail(e.body.allowed.length ? e.body.allowed.join(common[lang()].listSep) : t.ownerOnly) : "";
  return codeText(e.body) + allowed;
};

function Row({ c, report, onDone }: { c: Child; report?: Step; onDone: () => Promise<void> }) {
  const { api } = useSession();
  const t = useT(strings);
  const label = useLabel();
  const [mode, setMode] = useState<"reassign" | "redelegate" | null>(null);
  const [execs, setExecs] = useState<Executor[]>([]);
  const [pick, setPick] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const r = report?.payload?.report;
  const ev = r?.evidence ?? {};
  const terminal = TERMINAL.has(c.phase);

  const open = async (m: "reassign" | "redelegate") => {
    setError(null);
    setMode(m);
    setPick("");
    try {
      const q = c.project ? `?project=${encodeURIComponent(c.project)}` : "";
      setExecs((await api.get<{ executors: Executor[] }>("/executors" + q)).executors);
    } catch (e) {
      setError(msg(e));
    }
  };

  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      if (mode === "reassign") {
        await api.patch(`/items/${encodeURIComponent(c.id)}`, { assignee: pick, reason: "reassigned from web" }, { "If-Match": String(c.version) });
      } else {
        const { relations } = await api.get<{ relations: { type: string; from: string; to: string }[] }>(`/items/${encodeURIComponent(c.id)}`);
        const parent = relations.find((x) => x.type === "parent" && x.from === c.id)?.to;
        if (!parent) throw new Error(t.noParent);
        await api.post("/delegations", { parent, assignee: pick, title: c.title, reason: "re-delegated from web" });
      }
      setMode(null);
      await onDone();
    } catch (e) {
      setError(msg(e)); // 含 21（名单外）、409（版本冲突）：以服务端判定为准
    } finally {
      setBusy(false);
    }
  };

  return (
    <article aria-label={c.title}>
      <h3>{c.title}</h3>
      <p className="meta">
        {t.executor}
        <strong>{c.assignee ?? t.unassigned}</strong>
        {t.state}
        <Val k="phase" id={c.phase} />
        {c.project && t.project(c.project)}
      </p>
      <p>
        {t.progress}
        {c.status_label || t.noReport}
        {r && t.report(label("outcome", r.outcome), new Date(report!.at).toLocaleString(locale()))}
      </p>
      {(ev.commit || ev.pr || ev.branch || ev.link) && (
        <ul aria-label={t.evidence}>
          {ev.commit && <li>commit{t.colon}{ev.commit}</li>}
          {ev.pr && <li>PR{t.colon}{ev.pr}</li>}
          {ev.branch && <li>{t.branch}{t.colon}{ev.branch}</li>}
          {ev.link && <li>{t.link}{t.colon}{ev.link}</li>}
        </ul>
      )}
      {!mode && (
        <div className="actions">
          <button onClick={() => open(terminal ? "redelegate" : "reassign")}>{terminal ? t.redelegate : t.reassign}</button>
        </div>
      )}
      {mode && (
        <div className="actions">
          <select aria-label={t.target} value={pick} onChange={(e) => setPick(e.target.value)}>
            <option value="">{t.pickExecutor}</option>
            {execs.map((x) => (
              <option key={x.id} value={x.id}>
                {x.id}
                {x.allowed === false ? t.notAllowed : ""}
              </option>
            ))}
          </select>
          <button disabled={!pick || busy} onClick={submit}>
            {t.confirm(mode === "reassign" ? t.reassign : t.redelegate)}
          </button>
          <button disabled={busy} onClick={() => setMode(null)}>
            {t.cancel}
          </button>
        </div>
      )}
      {error && <div role="alert">{error}</div>}
    </article>
  );
}

export function DelegationPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [items, setItems] = useState<Child[] | null>(null);
  const [reports, setReports] = useState<Map<string, Step>>(new Map());
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const { items } = await api.get<{ items: Child[] }>("/items?kind=delegation");
      const latest = new Map<string, Step>();
      for (let after = 0; ; ) {
        const { events } = await api.get<{ events: Step[] }>(`/audit/events?type=delegation.reported&after=${after}&limit=${PAGE}`);
        for (const s of events) latest.set(s.object, s); // 序号升序，后者覆盖前者
        if (events.length < PAGE) break;
        after = events[events.length - 1].seq;
      }
      setReports(latest);
      setItems(items);
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);

  useEffect(() => {
    load();
  }, [load]);

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error && (
        <div role="alert">
          {error} <button onClick={load}>{t.retry}</button>
        </div>
      )}
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        items.map((c) => <Row key={c.id} c={c} report={reports.get(c.id)} onDone={load} />)
      )}
    </main>
  );
}
