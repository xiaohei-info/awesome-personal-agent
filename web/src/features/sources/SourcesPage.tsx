import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

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

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");
const at = (s?: string) => (s && !s.startsWith("0001") ? s : "—");

// 积压事件的状态（§9.3）：只显示中文，不露出内部值。
const backlogState = (v: SourceEvent): { label: string; cls: string } =>
  v.disposition === "expired"
    ? { label: "已过期，不再自动重判", cls: "badge plain" }
    : v.unjudged_reason === "undecided"
      ? { label: "判断未定，等你处理", cls: "badge" }
      : { label: "等模型恢复后自动重判", cls: "badge warn" };

// 两步确认的批量忽略：第一次点击只展开确认，避免误触一次忽略整批。
function IgnoreAll({ label, body, onDone }: { label: string; body: Record<string, unknown>; onDone: (err: string | null, n: number) => void }) {
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
      <button className="danger" onClick={go}>确认{label}</button> <button className="quiet" onClick={() => setConfirming(false)}>取消</button>
    </span>
  ) : (
    <button onClick={() => setConfirming(true)}>{label}</button>
  );
}

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
      <h2 id="h-register">登记自定义命令来源</h2>
      <p className="sub">登记的命令会在本机按周期执行，只能在 Web 登记。</p>
      {error && <div role="alert">{error}</div>}
      <form
        className="panel"
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
        <div>
          <button type="submit" disabled={!f.id.trim() || !f.command.trim() || !f.schedule.trim()}>登记</button>
        </div>
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
        !error && <p role="status">加载中…</p>
      ) : items.length === 0 ? (
        <p className="sub">还没有入站记录</p>
      ) : (
        <ul className="rows">
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
  const [notice, setNotice] = useState<string | null>(null);
  const base = `/sources/${encodeURIComponent(source.id)}/events?disposition=unjudged,expired`;

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
    <section role="region" aria-label="未判定事件">
      <h4>未判定事件</h4>
      <p className="sub">因模型不可用而未判定的，模型恢复后会自动重判；发生在 7 天之前的标为已过期，不再自动重判。都可以手动转为建议或忽略。</p>
      {error && <div role="alert">{error}</div>}
      {notice && <p role="status">{notice}</p>}
      {items === null ? (
        !error && <p role="status">加载中…</p>
      ) : items.length === 0 ? (
        <p className="sub">没有未判定事件</p>
      ) : (
        <>
          <ul className="rows">
            {items.map((v) => {
              const st = backlogState(v);
              return (
                <li key={v.id}>
                  <span>{v.subject || v.id}</span> · {v.type} · {v.occurred_at} <span className={st.cls}>{st.label}</span>
                  {v.summary && <p>{v.summary}</p>}
                  <div role="group" aria-label={`处理 ${v.id}`}>
                    <button onClick={() => act(v.id, "propose")}>转为建议</button> <button onClick={() => act(v.id, "ignore")}>忽略</button>
                  </div>
                </li>
              );
            })}
          </ul>
          <IgnoreAll
            label="忽略本来源的全部未判定事件"
            body={{ source: source.id }}
            onDone={async (err, n) => {
              setError(err);
              if (!err) setNotice(`已忽略 ${n} 条`);
              await load();
            }}
          />
        </>
      )}
      {next !== null && <button onClick={() => load(next)}>加载更多</button>}
    </section>
  );
}

function Rejects({ source }: { source: Source }) {
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
    <section role="region" aria-label="被拒收的事件">
      <h4>被拒收的事件</h4>
      {error && <div role="alert">{error}</div>}
      {items === null ? (
        !error && <p role="status">加载中…</p>
      ) : items.length === 0 ? (
        <p className="sub">没有被拒收的事件</p>
      ) : (
        <ul className="rows">
          {items.map((j, i) => (
            <li key={i}>
              {j.at} · 运行 {j.run_id} · 第 {j.line} 行 · {j.reason}
              <pre>{j.raw}</pre>
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
    <section role="region" aria-label={`来源 ${source.id}`} className="panel">
      <h3>{source.id}</h3>
      {source.type === "webhook" && <WebhookKey source={source} />}
      {source.type === "webhook" && <Ingests source={source} />}
      <Events source={source} />
      <Rejects source={source} />
    </section>
  );
}

export function SourcesPage() {
  const { api } = useSession();
  const [sources, setSources] = useState<Source[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

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
      <PageHead lede="APA 从这些地方读你的事。读到的内容先筛查，再决定要不要打扰你。">来源</PageHead>
      {error && <div role="alert">{error}</div>}
      {notice && <p role="status">{notice}</p>}
      <section role="region" aria-labelledby="h-list">
        <h2 id="h-list">来源登记</h2>
        <p className="sub">
          未判定事件在各来源的详情里处理；也可以一次忽略所有来源的未判定事件。{" "}
          <IgnoreAll
            label="忽略所有来源的未判定事件"
            body={{ all: true }}
            onDone={(err, n) => {
              setError(err);
              setNotice(err ? null : `已忽略 ${n} 条未判定事件`);
              setSelected(null); // 打开的详情列表已过时
            }}
          />
        </p>
        {sources === null ? (
          !error && <p role="status">加载中…</p>
        ) : sources.length === 0 ? (
          <p className="empty">还没有来源</p>
        ) : (
          <ul className="rows">
            {sources.map((s) => (
              <li key={s.id} aria-label={s.id}>
                <strong>{s.id}</strong> <span className="sub">{s.type}</span>
                {s.command && <code>{s.command}</code>}
                {s.schedule && <span className="sub">每 {s.schedule}</span>}
                <span className={"badge" + (s.enabled ? " ok" : "")}>{s.enabled ? "已启用" : "已停用"}</span>
                <div className="sub full">
                  游标 {s.cursor || "—"} · 最近成功 {at(s.stats?.last_success_at)} · 连续失败 {s.stats?.consecutive_failures ?? 0} · 超时 {s.stats?.timeouts ?? 0}
                  {s.stats?.last_error && ` · 最近错误 ${s.stats.last_error}`}
                </div>
                <div role="group" aria-label={`操作 ${s.id}`}>
                  <button onClick={() => toggle(s)}>{s.enabled ? "停用" : "启用"}</button> <button onClick={() => setSelected(s.id)}>详情</button>
                </div>
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
