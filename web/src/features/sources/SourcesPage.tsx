import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/sources（domain.Source，stats 为 runner.Stats）、/sources/{id}/ingests、/sources/{id}/events、/sources/{id}/rejects。
// 授权（自定义命令只 Web）、命令校验、转建议的规则都在服务端；本页只调用并如实展示服务端的回答。
interface Stats {
  runs?: number;
  consecutive_failures?: number;
  timeouts?: number;
  last_success_at?: string;
  last_failure_at?: string;
  last_error?: string;
}
export interface Source {
  id: string;
  type: "pull" | "script" | "webhook";
  source_type?: string;
  command?: string;
  schedule?: string;
  timeout?: number;
  cursor?: string;
  enabled: boolean;
  stats?: Stats;
  outbound?: Outbound;
}
// 外发范围（§14.14）：external 为 null 表示未选择，没有缺省值。
interface Outbound {
  local: string;
  external: string | null;
}
const SCOPES = ["full", "excerpt", "meta", "none"];
interface Restorable {
  matched: number;
  sources: { source: string; count: number }[];
}
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
interface SourceEvent {
  id: string;
  type: string;
  subject?: string;
  summary?: string;
  occurred_at: string;
  disposition: string;
  item?: string;
  unjudged_reason?: string;
  rejudges?: number;
  score?: number; // 影子打分的初筛分数（§9.3）；没打过分时没有
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

const at = (s?: string) => (s && !s.startsWith("0001") ? s : "—");

// 积压事件的状态（§9.3）：只显示说明文字，不露出内部值。
const backlogState = (t: { expired: string; undecided: string; waitModel: string; waitPaused: string }, v: SourceEvent): { label: string; cls: string } =>
  v.disposition === "expired"
    ? { label: t.expired, cls: "badge plain" }
    : v.unjudged_reason === "undecided"
      ? { label: t.undecided, cls: "badge" }
      : v.unjudged_reason === "models_paused"
        ? { label: t.waitPaused, cls: "badge plain" }
        : { label: t.waitModel, cls: "badge warn" };

// 两步确认的批量忽略：第一次点击只展开确认，避免误触一次忽略整批。
function IgnoreAll({ label, body, onDone }: { label: string; body: Record<string, unknown>; onDone: (err: string | null, n: number) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [confirming, setConfirming] = useState(false);
  const go = async () => {
    setConfirming(false);
    try {
      const r = await api.post<{ ignored: number }>("/source-events/ignore", body);
      onDone(null, r.ignored);
    } catch (e) {
      onDone(msg(e), 0);
    }
  };
  return confirming ? (
    <span role="group" aria-label={label}>
      <button className="danger" onClick={go}>{t.confirm(label)}</button> <button className="quiet" onClick={() => setConfirming(false)}>{t.cancel}</button>
    </span>
  ) : (
    <button onClick={() => setConfirming(true)}>{label}</button>
  );
}

function ScopeSelect({ label, value, onChange }: { label: string; value: string | null; onChange: (v: string) => void }) {
  const t = useT(strings);
  return (
    <label>
      {label}
      <select value={value ?? ""} onChange={(e) => onChange(e.target.value)}>
        {!value && <option value="">{t.unset}</option>}
        {SCOPES.map((v) => (
          <option key={v} value={v}>
            {t.scopes[v]}
          </option>
        ))}
      </select>
    </label>
  );
}

// 来源行里的两个外发范围：显示服务端当前值，改动即 PUT，以服务端回读为准（只有 Web 登录能写）。
function OutboundScopes({ source, onSaved, onError }: { source: Source; onSaved: () => Promise<void>; onError: (m: string) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const o = source.outbound ?? { local: "full", external: null };
  const save = async (next: Outbound) => {
    try {
      await api.put(`/settings/sources/${encodeURIComponent(source.id)}/outbound`, next);
    } catch (e) {
      onError(msg(e));
    }
    await onSaved();
  };
  return (
    <div className="sub full" role="group" aria-label={t.outbound(source.id)}>
      <ScopeSelect label={t.localScope} value={o.local} onChange={(v) => save({ ...o, local: v })} />{" "}
      <ScopeSelect label={t.externalScope} value={o.external} onChange={(v) => save({ ...o, external: v })} />
    </div>
  );
}

// 把旧级联忽略的事件放回积压（§9.3）：先预览（只读）再执行；改动条件后预览作废，必须重新预览。
function RestorePanel({ sources, onRestored }: { sources: Source[]; onRestored: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [source, setSource] = useState("");
  const [preview, setPreview] = useState<Restorable | null>(null);
  const [error, setError] = useState<string | null>(null);
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
      setError(msg(e));
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
      setError(msg(e));
    }
  };

  return (
    <section role="region" aria-labelledby="h-restore">
      <h2 id="h-restore">{t.restoreTitle}</h2>
      <p className="sub">{t.restoreHint}</p>
      {error && <div role="alert">{error}</div>}
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
          <div role="status" aria-label={t.previewLabel}>
            <p>{t.matched(preview.matched)}</p>
            <ul className="rows">
              {preview.sources.map((x) => (
                <li key={x.source}>
                  {t.count(x.source, x.count)}
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </section>
  );
}

function Registration({ onDone }: { onDone: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [f, setF] = useState({ id: "", source_type: "", command: "", schedule: "15m", timeout: "", credentials_path: "", local: "full", external: "" });
  const [error, setError] = useState<string | null>(null);
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });

  const submit = async () => {
    setError(null);
    if (!f.external) {
      setError(t.needExternal);
      return;
    }
    const body: Record<string, unknown> = { id: f.id.trim(), type: "script", command: f.command.trim(), schedule: f.schedule.trim() };
    if (f.source_type.trim()) body.source_type = f.source_type.trim();
    if (f.timeout.trim()) body.timeout = Number(f.timeout);
    if (f.credentials_path.trim()) body.credentials_path = f.credentials_path.trim();
    try {
      await api.post("/sources", body);
    } catch (e) {
      setError(msg(e));
      return;
    }
    setF({ ...f, id: "", source_type: "", command: "", timeout: "", credentials_path: "", external: "" });
    try {
      await api.put(`/settings/sources/${encodeURIComponent(body.id as string)}/outbound`, { local: f.local, external: f.external });
    } catch (e) {
      setError(t.scopeNotSaved(msg(e)));
    }
    onDone();
  };

  return (
    <section role="region" aria-labelledby="h-register">
      <h2 id="h-register">{t.registerTitle}</h2>
      <p className="sub">{t.registerHint}</p>
      {error && <div role="alert">{error}</div>}
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
          {t.sourceType}
          <input value={f.source_type} onChange={set("source_type")} />
        </label>
        <label>
          {t.command}
          <input value={f.command} onChange={set("command")} />
        </label>
        <label>
          {t.schedule}
          <input value={f.schedule} onChange={set("schedule")} />
        </label>
        <label>
          {t.timeout}
          <input value={f.timeout} onChange={set("timeout")} inputMode="numeric" />
        </label>
        <label>
          {t.credential}
          <input value={f.credentials_path} onChange={set("credentials_path")} />
        </label>
        <ScopeSelect label={t.localEndpointScope} value={f.local} onChange={(v) => setF({ ...f, local: v })} />
        <ScopeSelect label={t.externalEndpointScope} value={f.external} onChange={(v) => setF({ ...f, external: v })} />
        <div>
          <button type="submit" disabled={!f.id.trim() || !f.command.trim() || !f.schedule.trim()}>{t.register}</button>
        </div>
      </form>
    </section>
  );
}

function WebhookKey({ source }: { source: Source }) {
  const t = useT(strings);
  const { api } = useSession();
  const [secret, setSecret] = useState<Secret | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setSecret(null);
    setError(null);
  }, [source.id]);

  const issue = async () => {
    setError(null);
    try {
      setSecret(await api.post<Secret>(`/sources/${encodeURIComponent(source.id)}/webhook`));
    } catch (e) {
      setError(msg(e));
    }
  };
  return (
    <section role="region" aria-label={t.webhookKey}>
      <h4>{t.webhookKey}</h4>
      {error && <div role="alert">{error}</div>}
      <button onClick={issue}>{t.rotate}</button>
      {secret && (
        <div>
          <p>
            {secret.rotated ? t.rotated : t.created}
            {t.secretOnce(secret.url)}
          </p>
          <code data-testid="webhook-secret">{secret.secret}</code>
        </div>
      )}
    </section>
  );
}

function Ingests({ source }: { source: Source }) {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<Ingest[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const path = `/sources/${encodeURIComponent(source.id)}/ingests`;
  const load = useCallback(
    () =>
      api.get<{ ingests: Ingest[] }>(path).then(
        (r) => setItems(r.ingests),
        (e) => setError(msg(e)),
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
      setNotice(t.replayed(id));
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };

  return (
    <section role="region" aria-label={t.ingests}>
      <h4>{t.ingests}</h4>
      {error && <div role="alert">{error}</div>}
      {notice && <p role="status">{notice}</p>}
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noIngests}</p>
      ) : (
        <ul className="rows">
          {items.map((g) => (
            <li key={g.id}>
              <span>{g.id}</span> · {g.received_at} · {g.state} · {g.signed ? t.signed : t.unsigned} · {t.events(g.events)}
              {g.reason && ` · ${g.reason}`} <button onClick={() => setOpen(open === g.id ? null : g.id)}>{open === g.id ? t.hide : t.view}</button>{" "}
              <button onClick={() => replay(g.id)}>{t.replay}</button>
              {open === g.id && <pre data-testid={`ingest-body-${g.id}`}>{g.body ?? (g.body_base64 ? `base64: ${g.body_base64}` : t.noBody)}</pre>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function Events({ source }: { source: Source }) {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<SourceEvent[] | null>(null);
  const [next, setNext] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [byScore, setByScore] = useState(false);
  const base = `/sources/${encodeURIComponent(source.id)}/events?disposition=unjudged,expired${byScore ? "&sort=score" : ""}`;

  const load = useCallback(
    (after?: number) =>
      api.get<{ events: SourceEvent[]; next_after?: number }>(base + (after ? `&after=${after}` : "")).then(
        (r) => {
          setItems((cur) => (after && cur ? [...cur, ...r.events] : r.events));
          setNext(r.next_after ?? null);
        },
        (e) => setError(msg(e)),
      ),
    [api, base],
  );
  useEffect(() => {
    setItems(null);
    setNext(null);
    setError(null);
    load();
  }, [load]);

  const act = async (id: string, what: "propose" | "ignore") => {
    setError(null);
    setNotice(null);
    try {
      await api.post(`/source-events/${encodeURIComponent(id)}/${what}`);
    } catch (e) {
      setError(msg(e));
    }
    await load(); // 以服务端状态为准：处理成功的不再留在积压里
  };

  return (
    <section role="region" aria-label={t.unjudged}>
      <h4>{t.unjudged}</h4>
      <p className="sub">{t.unjudgedHint}</p>
      <label>
        <input type="checkbox" checked={byScore} onChange={(e) => setByScore(e.target.checked)} /> {t.byScore}
      </label>
      {error && <div role="alert">{error}</div>}
      {notice && <p role="status">{notice}</p>}
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noUnjudged}</p>
      ) : (
        <>
          <ul className="rows">
            {items.map((v) => {
              const st = backlogState(t, v);
              return (
                <li key={v.id}>
                  <span>{v.subject || v.id}</span> · {v.type} · {v.occurred_at} <span className={st.cls}>{st.label}</span>
                  {v.score !== undefined && <span className="badge plain">{t.score(v.score.toFixed(2))}</span>}
                  {v.summary && <p>{v.summary}</p>}
                  <div role="group" aria-label={t.handle(v.id)}>
                    <button onClick={() => act(v.id, "propose")}>{t.propose}</button> <button onClick={() => act(v.id, "ignore")}>{t.ignore}</button>
                  </div>
                </li>
              );
            })}
          </ul>
          <IgnoreAll
            label={t.ignoreSource}
            body={{ source: source.id }}
            onDone={async (err, n) => {
              setError(err);
              if (!err) setNotice(t.ignored(n));
              await load();
            }}
          />
        </>
      )}
      {next !== null && <button onClick={() => load(next)}>{t.more}</button>}
    </section>
  );
}

function Rejects({ source }: { source: Source }) {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<Reject[] | null>(null);
  const [next, setNext] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const base = `/sources/${encodeURIComponent(source.id)}/rejects`;

  const load = useCallback(
    (after?: number) =>
      api.get<{ rejects: Reject[]; next_after?: number }>(base + (after ? `?after=${after}` : "")).then(
        (r) => {
          setItems((cur) => (after && cur ? [...cur, ...r.rejects] : r.rejects));
          setNext(r.next_after ?? null);
        },
        (e) => setError(msg(e)),
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
      {error && <div role="alert">{error}</div>}
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noRejects}</p>
      ) : (
        <ul className="rows">
          {items.map((j, i) => (
            <li key={i}>
              {t.reject(j.at, j.run_id, j.line, j.reason)}
              <pre>{j.raw}</pre>
            </li>
          ))}
        </ul>
      )}
      {next !== null && <button onClick={() => load(next)}>{t.more}</button>}
    </section>
  );
}

function Detail({ source }: { source: Source }) {
  const t = useT(strings);
  return (
    <section role="region" aria-label={t.detail(source.id)} className="panel">
      <h3>{source.id}</h3>
      {source.type === "webhook" && <WebhookKey source={source} />}
      {source.type === "webhook" && <Ingests source={source} />}
      <Events source={source} />
      <Rejects source={source} />
    </section>
  );
}

export function SourcesPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [sources, setSources] = useState<Source[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [rev, setRev] = useState(0); // 放回积压后让打开的详情重读积压

  const load = useCallback(
    () =>
      api.get<{ sources: Source[] }>("/sources").then(
        (r) => setSources(r.sources),
        (e) => setError(msg(e)),
      ),
    [api],
  );
  useEffect(() => {
    load();
  }, [load]);

  const toggle = async (s: Source) => {
    setError(null);
    try {
      await api.patch(`/sources/${encodeURIComponent(s.id)}`, { enabled: !s.enabled });
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };

  const current = sources?.find((s) => s.id === selected);
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error && <div role="alert">{error}</div>}
      {notice && <p role="status">{notice}</p>}
      <section role="region" aria-labelledby="h-list">
        <h2 id="h-list">{t.registry}</h2>
        <p className="sub">
          {t.registryHint}{" "}
          <IgnoreAll
            label={t.ignoreAll}
            body={{ all: true }}
            onDone={(err, n) => {
              setError(err);
              setNotice(err ? null : t.ignoredAll(n));
              setSelected(null); // 打开的详情列表已过时
            }}
          />
        </p>
        {sources === null ? (
          !error && <p role="status">{t.loading}</p>
        ) : sources.length === 0 ? (
          <p className="empty">{t.none}</p>
        ) : (
          <ul className="rows">
            {sources.map((s) => (
              <li key={s.id} aria-label={s.id}>
                <strong>{s.id}</strong> <span className="sub">{s.type}</span>
                {s.command && <code>{s.command}</code>}
                {s.schedule && <span className="sub">{t.every(s.schedule)}</span>}
                <span className={"badge" + (s.enabled ? " ok" : "")}>{s.enabled ? t.enabled : t.disabled}</span>
                <div className="sub full">
                  {t.stats(s.cursor || "—", at(s.stats?.last_success_at), s.stats?.consecutive_failures ?? 0, s.stats?.timeouts ?? 0)}
                  {s.stats?.last_error && t.lastError(s.stats.last_error)}
                </div>
                <OutboundScopes source={s} onSaved={load} onError={setError} />
                <div role="group" aria-label={t.ops(s.id)}>
                  <button onClick={() => toggle(s)}>{s.enabled ? t.disable : t.enable}</button> <button onClick={() => setSelected(s.id)}>{t.details}</button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
      {current && <Detail key={`${current.id}:${rev}`} source={current} />}
      <RestorePanel sources={sources ?? []} onRestored={() => setRev((n) => n + 1)} />
      <Registration onDone={load} />
    </main>
  );
}
