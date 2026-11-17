import { Link } from "react-router-dom";
import { useCallback, useEffect, useState } from "react";
import { PageHead } from "../../app/page";
import { errorText, useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { strings } from "./strings";

// 设置 › 导入：POST /import（application/x-ndjson，每次至多 500 行）、GET /imports、GET /audit/events?imported=、POST /settings/grants（附录 B.2）。
// 页面只切片与汇总；校验、幂等、批次与授权都在服务端，被拒绝时原因原样显示。
const CHUNK = 500;
const PAGE = 500;
const SOURCE = /^[a-z0-9-]{1,32}$/;
const NDJSON = { "Content-Type": "application/x-ndjson" };

interface Row { line: number; external_id?: string; outcome: "created" | "existing" | "invalid"; object?: string; code?: string; why?: string }
interface Result { batch?: string; results: Row[]; created: number; existing: number; invalid: number }
interface Batch { id: string; source: string; created: number; existing: number; invalid: number; started_at: string; actor: string; grant?: string }

const readText = (f: File) => new Promise<string>((ok, no) => {
  const r = new FileReader();
  r.onload = () => ok(String(r.result));
  r.onerror = () => no(r.error);
  r.readAsText(f);
});
const chunks = (lines: string[]) => Array.from({ length: Math.ceil(lines.length / CHUNK) }, (_, i) => lines.slice(i * CHUNK, (i + 1) * CHUNK));

export function SettingsImportPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [batches, setBatches] = useState<Batch[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    try {
      setBatches((await api.get<{ batches: Batch[] }>("/imports")).batches);
      setError(null);
    } catch (e) {
      setError(errorText(e));
    }
  }, [api]);
  useEffect(() => void load(), [load]);
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <Upload onDone={load} />
      <GrantForm />
      <section aria-label={t.batches} className="panel stack">
        <h2>{t.batches}</h2>
        {error && <p role="alert">{error}</p>}
        {batches && batches.length === 0 && <p className="sub">{t.noBatches}</p>}
        {batches && batches.map((b) => <BatchRow key={b.id} b={b} />)}
      </section>
    </main>
  );
}

function Upload({ onDone }: { onDone: () => Promise<void> }) {
  const t = useT(strings);
  const { api } = useSession();
  const [source, setSource] = useState("");
  const [lines, setLines] = useState<string[]>([]);
  const [check, setCheck] = useState<Result | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [run, setRun] = useState<{ batch?: string; done: number; created: number } | null>(null);
  const parts = chunks(lines);
  const q = (extra: Record<string, string>) => "/import?" + new URLSearchParams({ source, ...extra });

  async function pick(f: File | undefined) {
    setCheck(null); setRun(null); setErr(null);
    setLines(f ? (await readText(f)).split("\n").map((l) => l.trim()).filter(Boolean) : []);
  }
  async function precheck() {
    setErr(null); setCheck(null); setRun(null);
    if (!SOURCE.test(source)) return setErr(t.badSource);
    if (!lines.length) return setErr(t.emptyFile);
    setBusy(true);
    try {
      const sum: Result = { results: [], created: 0, existing: 0, invalid: 0 };
      for (const [i, p] of parts.entries()) {
        const r = await api.postText<Result>(q({ dry_run: "1" }), p.join("\n") + "\n", NDJSON);
        sum.created += r.created; sum.existing += r.existing; sum.invalid += r.invalid;
        sum.results.push(...r.results.map((x) => ({ ...x, line: x.line + i * CHUNK })));
      }
      setCheck(sum);
    } catch (e) {
      setErr(errorText(e));
    } finally {
      setBusy(false);
    }
  }
  // 逐批写入；中途失败时已写入的批留着，"从断点继续"沿用同一批次号接着写。
  async function write() {
    setErr(null); setBusy(true);
    let cur = run ?? { done: 0, created: 0 };
    setRun(cur);
    try {
      while (cur.done < parts.length) {
        const r = await api.postText<Result>(q(cur.batch ? { batch: cur.batch } : {}), parts[cur.done].join("\n") + "\n", NDJSON);
        cur = { batch: r.batch ?? cur.batch, done: cur.done + 1, created: cur.created + r.created };
        setRun(cur);
      }
      await onDone();
    } catch (e) {
      setErr(errorText(e));
    } finally {
      setBusy(false);
    }
  }
  const finished = run && run.done === parts.length && parts.length > 0;
  return (
    <section aria-label={t.upload} className="panel stack">
      <h2>{t.upload}</h2>
      <label>{t.file} <input type="file" accept=".jsonl,.ndjson,.json,text/plain" onChange={(e) => void pick(e.target.files?.[0])} /></label>
      <label>{t.source} <input value={source} placeholder={t.sourceHint} onChange={(e) => { setSource(e.target.value); setCheck(null); setRun(null); }} /></label>
      <div><button disabled={busy} onClick={() => void precheck()}>{busy && !run ? t.checking : t.check}</button></div>
      {err && <p role="alert">{err}</p>}
      {check && (
        <section aria-label={t.precheck} className="stack">
          <h3>{t.precheck}</h3>
          <p>{t.willCreate(check.created)}；{t.existing(check.existing)}；{t.invalid(check.invalid)}</p>
          <ul className="rows">
            {check.results.filter((r) => r.outcome === "invalid").map((r) => (
              <li key={r.line}>{t.line(r.line)}{r.external_id ? `（${r.external_id}）` : ""}：{r.why ?? r.code}</li>
            ))}
          </ul>
          {!finished && <div><button disabled={busy || check.created === 0} onClick={() => void write()}>{run && run.done > 0 ? t.resume : t.confirm}</button></div>}
        </section>
      )}
      {run && (
        <p role="status">{finished ? t.finished(run.batch ?? "", run.created) : t.progress(run.done, parts.length)}</p>
      )}
    </section>
  );
}

function BatchRow({ b }: { b: Batch }) {
  const t = useT(strings);
  const { api } = useSession();
  const [open, setOpen] = useState(false);
  const [objs, setObjs] = useState<string[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    if (!open || objs) return;
    // 一个批次可含多个 500 条的请求：按 seq 游标翻页（附录 B.2），取到不足一页为止。
    (async () => {
      const out: string[] = [];
      for (let after = 0; ; ) {
        const { events } = await api.get<{ events: { seq: number; object: string }[] }>("/audit/events?" + new URLSearchParams({ imported: b.id, type: "item.created", limit: String(PAGE), after: String(after) }));
        out.push(...events.map((e) => e.object));
        if (events.length < PAGE) return out;
        after = events[events.length - 1].seq;
      }
    })().then(setObjs, (e) => setErr(errorText(e)));
  }, [open, objs, api, b.id]);
  return (
    <div role="group" aria-label={b.id} className="stack">
      <p>
        <strong>{b.source}</strong> · {t.cCreated}: {b.created} · {t.cExisting}: {b.existing} · {t.cInvalid}: {b.invalid} · {t.cTime}: {new Date(b.started_at).toLocaleString()} · {t.cActor}: {b.actor}
        {b.grant ? ` · ${t.cGrant}: ${b.grant}` : ""}
      </p>
      <button aria-expanded={open} aria-label={t.open(b.id)} onClick={() => setOpen(!open)}>{open ? "−" : "+"}</button>
      {open && err && <p role="alert">{err}</p>}
      {open && objs && (
        <ul aria-label={t.written(b.id)} className="rows">
          {objs.length === 0 && <li className="sub">{t.noWritten}</li>}
          {objs.map((id) => <li key={id}><Link to={"/items/" + encodeURIComponent(id)}>{id}</Link></li>)}
        </ul>
      )}
    </div>
  );
}

// 导入授权单：actions 只能是 import.apply，带 import 段与不超过 7 天的有效期（附录 B.2，§6.13）。
function GrantForm() {
  const t = useT(strings);
  const { api } = useSession();
  const [agent, setAgent] = useState("");
  const [source, setSource] = useState("");
  const [max, setMax] = useState("");
  const [hours, setHours] = useState("24");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  async function issue() {
    const m = Number(max), h = Number(hours);
    if (!agent.trim() || !SOURCE.test(source) || !Number.isInteger(m) || m < 1 || !Number.isInteger(h) || h < 1 || h > 168)
      return setMsg({ ok: false, text: !SOURCE.test(source) ? t.badSource : t.grantBad });
    try {
      const g = await api.post<{ id: string }>("/settings/grants", {
        grantee: { agent: agent.trim() }, scope: { all: true }, actions: ["import.apply"],
        import: { source, max_records: m }, expires_at: new Date(Date.now() + h * 3600_000).toISOString(),
      });
      setMsg({ ok: true, text: t.grantDone(g.id) });
    } catch (e) {
      setMsg({ ok: false, text: errorText(e) });
    }
  }
  return (
    <form aria-label={t.grantTitle} className="panel stack" onSubmit={(e) => { e.preventDefault(); void issue(); }}>
      <h2>{t.grantTitle}</h2>
      <label>{t.grantAgent} <input value={agent} onChange={(e) => setAgent(e.target.value)} /></label>
      <label>{t.cSource} <input value={source} placeholder={t.sourceHint} onChange={(e) => setSource(e.target.value)} /></label>
      <label>{t.grantMax} <input type="number" value={max} onChange={(e) => setMax(e.target.value)} /></label>
      <label>{t.grantHours} <input type="number" value={hours} onChange={(e) => setHours(e.target.value)} /></label>
      <div><button type="submit">{t.grantIssue}</button></div>
      {msg && <p role={msg.ok ? "status" : "alert"}>{msg.text}</p>}
    </form>
  );
}
