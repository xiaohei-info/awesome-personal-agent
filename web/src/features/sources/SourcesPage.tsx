import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";

// 形状对应 GET /v1/sources（domain.Source，stats 为 runner.Stats）、/sources/{id}/ingests、/sources/{id}/events。
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
}
interface Secret {
  secret: string;
  url: string;
  rotated: boolean;
}

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");
const at = (s?: string) => (s && !s.startsWith("0001") ? s : "—");

function Registration({ onDone }: { onDone: () => void }) {
  const { api } = useSession();
  const [f, setF] = useState({ id: "", command: "", schedule: "15m", timeout: "", credentials_path: "" });
  const [error, setError] = useState<string | null>(null);
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });

  const submit = async () => {
    setError(null);
    const body: Record<string, unknown> = { id: f.id.trim(), type: "script", command: f.command.trim(), schedule: f.schedule.trim() };
    if (f.timeout.trim()) body.timeout = Number(f.timeout);
    if (f.credentials_path.trim()) body.credentials_path = f.credentials_path.trim();
    try {
      await api.post("/sources", body);
      setF({ ...f, id: "", command: "", timeout: "", credentials_path: "" });
      onDone();
    } catch (e) {
      setError(msg(e));
    }
  };

  return (
    <section role="region" aria-labelledby="h-register">
      <h3 id="h-register">登记自定义命令来源</h3>
      <p>登记的命令会在本机按周期执行，只能在 Web 登记。</p>
      {error && <div role="alert">{error}</div>}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <label>
          来源 id
          <input value={f.id} onChange={set("id")} />
        </label>
        <label>
          命令
          <input value={f.command} onChange={set("command")} />
        </label>
        <label>
          周期
          <input value={f.schedule} onChange={set("schedule")} />
        </label>
        <label>
          超时（秒）
          <input value={f.timeout} onChange={set("timeout")} inputMode="numeric" />
        </label>
        <label>
          凭据路径
          <input value={f.credentials_path} onChange={set("credentials_path")} />
        </label>
        <button type="submit" disabled={!f.id.trim() || !f.command.trim() || !f.schedule.trim()}>登记</button>
      </form>
    </section>
  );
}

function WebhookKey({ source }: { source: Source }) {
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
    <section role="region" aria-label="webhook 密钥">
      <h4>webhook 密钥</h4>
      {error && <div role="alert">{error}</div>}
      <button onClick={issue}>创建或轮换密钥</button>
      {secret && (
        <div>
          <p>{secret.rotated ? "已轮换，旧密钥短期内仍有效。" : "已创建。"}密钥只显示这一次，请立即保存；接收地址 {secret.url}</p>
          <code data-testid="webhook-secret">{secret.secret}</code>
        </div>
      )}
    </section>
  );
}

function Ingests({ source }: { source: Source }) {
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
      setNotice(`已重放 ${id}`);
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };

  return (
    <section role="region" aria-label="入站记录">
      <h4>入站记录</h4>
      {error && <div role="alert">{error}</div>}
      {notice && <p role="status">{notice}</p>}
      {items === null ? (
        !error && <p>加载中…</p>
      ) : items.length === 0 ? (
        <p>还没有入站记录</p>
      ) : (
        <ul>
          {items.map((g) => (
            <li key={g.id}>
              <span>{g.id}</span> · {g.received_at} · {g.state} · {g.signed ? "已验签" : "未验签"} · {g.events} 个事件
              {g.reason && ` · ${g.reason}`} <button onClick={() => setOpen(open === g.id ? null : g.id)}>{open === g.id ? "收起" : "查看"}</button>{" "}
              <button onClick={() => replay(g.id)}>重放</button>
              {open === g.id && <pre data-testid={`ingest-body-${g.id}`}>{g.body ?? (g.body_base64 ? `base64: ${g.body_base64}` : "（无内容）")}</pre>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function Events({ source }: { source: Source }) {
  const { api } = useSession();
  const [items, setItems] = useState<SourceEvent[] | null>(null);
  const [next, setNext] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const base = `/sources/${encodeURIComponent(source.id)}/events?disposition=unjudged`;

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

  const propose = async (id: string) => {
    setError(null);
    try {
      await api.post(`/source-events/${encodeURIComponent(id)}/propose`);
    } catch (e) {
      setError(msg(e));
    }
    await load(); // 以服务端状态为准：转成功的不再是 unjudged
  };

  return (
    <section role="region" aria-label="未判定事件">
      <h4>未判定事件</h4>
      {error && <div role="alert">{error}</div>}
      {items === null ? (
        !error && <p>加载中…</p>
      ) : items.length === 0 ? (
        <p>没有未判定事件</p>
      ) : (
        <ul>
          {items.map((v) => (
            <li key={v.id}>
              <span>{v.subject || v.id}</span> · {v.type} · {v.occurred_at}
              {v.summary && <p>{v.summary}</p>} <button onClick={() => propose(v.id)}>转为建议</button>
            </li>
          ))}
        </ul>
      )}
      {next !== null && <button onClick={() => load(next)}>加载更多</button>}
    </section>
  );
}

function Detail({ source }: { source: Source }) {
  return (
    <section role="region" aria-label={`来源 ${source.id}`}>
      <h3>{source.id}</h3>
      {source.type === "webhook" && <WebhookKey source={source} />}
      {source.type === "webhook" && <Ingests source={source} />}
      <Events source={source} />
    </section>
  );
}

export function SourcesPage() {
  const { api } = useSession();
  const [sources, setSources] = useState<Source[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);

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
      <h2>来源</h2>
      {error && <div role="alert">{error}</div>}
      <section role="region" aria-labelledby="h-list">
        <h3 id="h-list">来源登记</h3>
        {sources === null ? (
          !error && <p>加载中…</p>
        ) : sources.length === 0 ? (
          <p>还没有来源</p>
        ) : (
          <ul>
            {sources.map((s) => (
              <li key={s.id} aria-label={s.id}>
                <span>{s.id}</span> · {s.type}
                {s.command && ` · ${s.command}`}
                {s.schedule && ` · 每 ${s.schedule}`} · {s.enabled ? "已启用" : "已停用"}
                <br />
                游标 {s.cursor || "—"} · 最近成功 {at(s.stats?.last_success_at)} · 连续失败 {s.stats?.consecutive_failures ?? 0} · 超时 {s.stats?.timeouts ?? 0}
                {s.stats?.last_error && ` · 最近错误 ${s.stats.last_error}`}
                <br />
                <button onClick={() => toggle(s)}>{s.enabled ? "停用" : "启用"}</button> <button onClick={() => setSelected(s.id)}>详情</button>
              </li>
            ))}
          </ul>
        )}
      </section>
      {current && <Detail source={current} />}
      <Registration onDone={load} />
    </main>
  );
}
