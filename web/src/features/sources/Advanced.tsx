import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { errorText, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useRaw } from "../../app/raw";
import { Pick } from "../../app/values";
import { strings } from "./strings";
import { when, type Source } from "./shared";

// 不常用、排障或一次性的操作（设计 §5.13）：都收在「高级」里。授权与校验在服务端，这里只调用并如实显示回答。

interface Ingest {
  id: string;
  received_at: string;
  state: string;
  signed: boolean;
  events: number;
  reason?: string;
  body?: string;
  body_base64?: string;
}
interface Reject {
  run_id: string;
  line: number;
  reason: string;
  raw: string;
  at: string;
}
interface Secret {
  secret: string;
  url: string;
  rotated: boolean;
}
interface Restorable {
  matched: number;
  sources: { source: string; count: number }[];
}

function Copy({ text }: { text: string }) {
  const t = useT(strings);
  const [done, setDone] = useState(false);
  return (
    <button className="quiet" onClick={() => navigator.clipboard?.writeText(text).then(() => setDone(true), () => undefined)}>
      {done ? t.copied : t.copy}
    </button>
  );
}

export function WebhookKey({ source }: { source: Source }) {
  const t = useT(strings);
  const { api } = useSession();
  const [secret, setSecret] = useState<Secret | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    setSecret(null);
    setError(null);
  }, [source.id]);

  const issue = async () => {
    setError(null);
    try {
      setSecret(await api.post<Secret>(`/sources/${encodeURIComponent(source.id)}/webhook`));
    } catch (e) {
      setError(e);
    }
  };
  return (
    <section role="region" aria-label={t.webhookKey}>
      <h4>{t.webhookKey}</h4>
      {error !== null && <ErrorNote error={error} />}
      <button onClick={issue}>{t.rotate}</button>
      {secret && (
        <div className="panel">
          <p role="status">{secret.rotated ? t.rotated : t.created} · {t.once}</p>
          <div className="copy-row">{t.address} <code>{secret.url}</code> <Copy text={secret.url} /></div>
          <div className="copy-row">{t.key} <code data-testid="webhook-secret">{secret.secret}</code> <Copy text={secret.secret} /></div>
        </div>
      )}
    </section>
  );
}

export function Ingests({ source }: { source: Source }) {
  const t = useT(strings);
  const raw = useRaw();
  const { api } = useSession();
  const [items, setItems] = useState<Ingest[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [open, setOpen] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const path = `/sources/${encodeURIComponent(source.id)}/ingests`;
  const load = useCallback(
    () =>
      api.get<{ ingests: Ingest[] }>(path).then(
        (r) => setItems(r.ingests),
        (e) => setError(e),
      ),
    [api, path],
  );
  useEffect(() => {
    setItems(null);
    setError(null);
    setNotice(null);
    load();
  }, [load]);

  const replay = async (id: string) => {
    setError(null);
    setNotice(null);
    try {
      await api.post(`/ingests/${encodeURIComponent(id)}/replay`);
      setNotice(t.replayed);
    } catch (e) {
      setError(e);
    }
    await load();
  };

  return (
    <section role="region" aria-label={t.ingests}>
      <h4>{t.ingests}</h4>
      {error !== null && <ErrorNote error={error} />}
      {notice && <p role="status">{notice}</p>}
      {items === null ? (
        error === null && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noIngests}</p>
      ) : (
        <ul className="rows">
          {items.map((g) => (
            <li key={g.id}>
              {when(g.received_at)} · {t.ingestStates[g.state] ?? <code>{g.state}</code>} · {g.signed ? t.signed : t.unsigned} · {t.produced(g.events)}
              {g.reason && ` · ${g.reason}`} {raw && <code className="raw-id">{g.id}</code>}{" "}
              <button onClick={() => setOpen(open === g.id ? null : g.id)}>{open === g.id ? t.hide : t.view}</button>{" "}
              <button onClick={() => replay(g.id)}>{t.replay}</button>
              {open === g.id && <pre data-testid={`ingest-body-${g.id}`}>{g.body ?? (g.body_base64 ? `base64: ${g.body_base64}` : t.noBody)}</pre>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export function Rejects({ source }: { source: Source }) {
  const t = useT(strings);
  const raw = useRaw();
  const { api } = useSession();
  const [items, setItems] = useState<Reject[] | null>(null);
  const [next, setNext] = useState<number | null>(null);
  const [error, setError] = useState<unknown>(null);
  const base = `/sources/${encodeURIComponent(source.id)}/rejects`;

  const load = useCallback(
    (after?: number) =>
      api.get<{ rejects: Reject[]; next_after?: number }>(base + (after ? `?after=${after}` : "")).then(
        (r) => {
          setItems((cur) => (after && cur ? [...cur, ...r.rejects] : r.rejects));
          setNext(r.next_after ?? null);
        },
        (e) => setError(e),
      ),
    [api, base],
  );
  useEffect(() => {
    setItems(null);
    setNext(null);
    setError(null);
    load();
  }, [load]);

  return (
    <section role="region" aria-label={t.rejects}>
      <h4>{t.rejects}</h4>
      {error !== null && <ErrorNote error={error} />}
      {items === null ? (
        error === null && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noRejects}</p>
      ) : (
        <ul className="rows">
          {items.map((j, i) => (
            <li key={i}>
              {t.reject(when(j.at), j.line, j.reason)} {raw && <code className="raw-id">{j.run_id}</code>}
              <pre>{j.raw}</pre>
            </li>
          ))}
        </ul>
      )}
      {next !== null && <button onClick={() => load(next)}>{t.more}</button>}
    </section>
  );
}

// 找回升级前被旧规则自动忽略的消息（§9.3 放回积压）：先看条数（只读）再执行；改动条件后条数作废，必须重看。
export function RestorePanel({ sources, onRestored }: { sources: Source[]; onRestored: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [source, setSource] = useState("");
  const [preview, setPreview] = useState<Restorable | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [done, setDone] = useState<number | null>(null);
  const key = useRef(crypto.randomUUID());
  const win = () => ({ from: new Date(from).toISOString(), to: new Date(to).toISOString(), ...(source ? { source } : {}) });
  const edit = (set: (v: string) => void) => (e: { target: { value: string } }) => {
    set(e.target.value);
    setPreview(null);
    setDone(null);
    setError(null);
  };

  const look = async () => {
    setError(null);
    setDone(null);
    try {
      const q = new URLSearchParams(win());
      setPreview(await api.get<Restorable>(`/source-events/restorable?${q}`));
      key.current = crypto.randomUUID();
    } catch (e) {
      setPreview(null);
      setError(e);
    }
  };
  const run = async () => {
    setError(null);
    try {
      const r = await api.post<{ restored: number }>("/source-events/restore", win(), { "Idempotency-Key": key.current });
      setDone(r.restored);
      setPreview(null);
      onRestored();
    } catch (e) {
      // 4xx 是明确被拒，换新键；结果未知（网络错误、5xx）保留键以便安全重试。
      if (e instanceof ApiError && e.status >= 400 && e.status < 500) key.current = crypto.randomUUID();
      setError(e);
    }
  };

  return (
    <section role="region" aria-labelledby="h-restore">
      <h3 id="h-restore">{t.restoreTitle}</h3>
      {error !== null && <ErrorNote error={error} />}
      {done !== null && <p role="status">{t.restored(done)}</p>}
      <div className="panel">
        <label>
          {t.from}
          <input type="datetime-local" value={from} onChange={edit(setFrom)} />
        </label>
        <label>
          {t.to}
          <input type="datetime-local" value={to} onChange={edit(setTo)} />
        </label>
        <label>
          {t.source}
          <select value={source} onChange={edit(setSource)}>
            <option value="">{t.anySource}</option>
            {sources.map((s) => (
              <option key={s.id} value={s.id}>
                {s.id}
              </option>
            ))}
          </select>
        </label>
        <div>
          <button disabled={!from || !to} onClick={look}>{t.preview}</button>{" "}
          <button disabled={!preview || preview.matched === 0} onClick={run}>{t.restore}</button>
        </div>
        {preview && (
          <div role="status" aria-label={t.restorePreview}>
            <p>{t.matched(preview.matched)}</p>
            <ul className="rows">
              {preview.sources.map((x) => (
                <li key={x.source}>{t.count(x.source, x.count)}</li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </section>
  );
}

const KNOWN_KINDS = ["mail", "calendar", "reminder", "github"];
const OTHER = "\u0000other";

// 自定义命令来源（只能在 Web 登记，§14.3）：周期用「每 [n][单位]」，外部外发范围必须选（§14.14，没有缺省值）。
export function Registration({ onDone }: { onDone: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const blank = { id: "", kind: "", custom: "", command: "", every: "15", unit: "m", timeout: "", credentials_path: "", local: "full", external: "" };
  const [f, setF] = useState(blank);
  const [error, setError] = useState<unknown>(null);
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const sourceType = f.kind === OTHER ? f.custom.trim() : f.kind;

  const submit = async () => {
    setError(null);
    if (!f.external) {
      setError(new Error(t.needExternal));
      return;
    }
    const body: Record<string, unknown> = { id: f.id.trim(), type: "script", command: f.command.trim(), schedule: `${f.every.trim()}${f.unit}` };
    if (sourceType) body.source_type = sourceType;
    if (f.timeout.trim()) body.timeout = Number(f.timeout);
    if (f.credentials_path.trim()) body.credentials_path = f.credentials_path.trim();
    try {
      await api.post("/sources", body);
    } catch (e) {
      setError(e);
      return;
    }
    setF({ ...blank, local: f.local });
    try {
      await api.put(`/settings/sources/${encodeURIComponent(body.id as string)}/outbound`, { local: f.local, external: f.external });
    } catch (e) {
      setError(new Error(t.scopeNotSaved(errorText(e))));
    }
    onDone();
  };

  return (
    <section role="region" aria-labelledby="h-register">
      <h3 id="h-register">{t.registerTitle}</h3>
      {error !== null && <ErrorNote error={error} />}
      <form
        className="panel"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <label>
          {t.id}
          <input value={f.id} onChange={set("id")} />
        </label>
        <label>
          {t.kind}
          <select value={f.kind} onChange={set("kind")}>
            <option value="">{t.kindNone}</option>
            {KNOWN_KINDS.map((k) => (
              <option key={k} value={k}>{t.kinds[k]}</option>
            ))}
            <option value={OTHER}>{t.kindOther}</option>
          </select>
        </label>
        {f.kind === OTHER && (
          <label>
            {t.kindCustom}
            <input value={f.custom} onChange={set("custom")} />
          </label>
        )}
        <label>
          {t.command}
          <input value={f.command} onChange={set("command")} />
        </label>
        <fieldset className="seg-row">
          <legend>{t.period}</legend>
          <input aria-label={t.periodNumber} value={f.every} onChange={set("every")} inputMode="numeric" size={4} />{" "}
          <select aria-label={t.periodUnit} value={f.unit} onChange={set("unit")}>
            {["m", "h"].map((u) => (
              <option key={u} value={u}>{t.units[u]}</option>
            ))}
          </select>
        </fieldset>
        <label>
          {t.timeout}
          <input value={f.timeout} onChange={set("timeout")} inputMode="numeric" />
        </label>
        <label>
          {t.credential}
          <input value={f.credentials_path} onChange={set("credentials_path")} />
        </label>
        <label>
          {t.toLocal}
          <Pick k="outboundScope" value={f.local} onChange={(v) => setF({ ...f, local: v })} />
        </label>
        <label>
          {t.toExternal}
          <Pick k="outboundScope" value={f.external} onChange={(v) => setF({ ...f, external: v })} empty={t.choose} />
        </label>
        <div>
          <button type="submit" disabled={!f.id.trim() || !f.command.trim() || !/^\d+$/.test(f.every.trim()) || Number(f.every) < 1}>{t.register}</button>
        </div>
      </form>
    </section>
  );
}
