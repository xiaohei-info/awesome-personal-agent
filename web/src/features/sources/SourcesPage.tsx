import { useCallback, useEffect, useRef, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { Val } from "../../app/values";
import { useRaw } from "../../app/raw";
import { forgetNames } from "../../app/names";
import { rejudgeDays } from "../../app/status";
import { strings } from "./strings";
import { ago, period, when, type Source } from "./shared";
import { BacklogFlow, IgnoreAll, inProgress, SourceBacklog, type Models } from "./Backlog";
import { Ingests, Registration, Rejects, RestorePanel, WebhookKey } from "./Advanced";
import { DefaultProject, Digest } from "./Digest";
import { TrustedSenders } from "./TrustedSenders";
import { Link, Route, Routes, useNavigate, useSearchParams } from "react-router-dom";
import { RecentMessages } from "./Recent";
import { MessageTracePage } from "../trace/Trace";

// 消息来源页（界面设计 §9.7）：来源列表 → 积压四步；排障与一次性操作在「高级」里。
// 来源不再逐个配外发：送模型端点的范围在「设置 › 模型」（MODEL_SOURCES），钥匙读多少在签发钥匙时选（§14.14）。
// 授权（自定义命令只 Web）、命令校验、转建议的规则都在服务端；本页只调用并如实展示服务端的回答。

/** 「设置 › 模型」里「模型端点能收哪些来源」一节。 */
export const MODEL_SOURCES = "/settings/models#endpoint-sources";

interface Backlog {
  count: number;
  outbound_unset: number;
  rejudgeDays?: number;
}
interface HealthSignal {
  id: string;
  count?: number;
  detail?: Record<string, unknown>;
}
interface Endpoints {
  judge_endpoint?: { url?: string } | null;
  generate_endpoint?: { url?: string } | null;
  models_paused?: boolean;
}

const ICON: Record<string, string> = { mail: "\u2709\ufe0f", calendar: "\ud83d\udcc5", reminder: "\u23f0", github: "\ud83e\udde9", webhook: "\ud83d\udce5" };

function Schedule({ s }: { s?: string }) {
  const t = useT(strings);
  if (!s) return null;
  const p = period(s);
  return <span className="sub">{p ? t.every(p.n, t.units[p.unit]) : t.cron}</span>;
}

/** 来源的状态一句话：读取失败 / 已停用 / 正常，加上最近一次读到的时间。 */
function Status({ s }: { s: Source }) {
  const t = useT(strings);
  const fails = s.stats?.consecutive_failures ?? 0;
  const read = ago(s.stats?.last_success_at);
  const badge = !s.enabled ? <span className="badge">{t.disabled}</span> : fails > 0 ? <span className="badge warn">{t.readFailed}</span> : <span className="badge ok">{t.ok}</span>;
  return (
    <>
      <span className="sub">{fails > 0 ? t.failing(fails) : read ? t.readAgo(read) : s.type === "webhook" ? null : t.neverRead}</span> {badge}
    </>
  );
}

/** 名字与行内改名：重名等被拒时原地显示原因，名字不变。 */
function Rename({ s, onSaved }: { s: Source; onSaved: () => void }) {
  const t = useT(strings);
  const raw = useRaw();
  const { api } = useSession();
  const [draft, setDraft] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  if (draft === null)
    return (
      <>
        <strong>{s.name}</strong>
        {raw && <code className="raw-id">{s.id}</code>}{" "}
        <button className="link" aria-label={t.renameLabel(s.name)} onClick={() => { setError(null); setDraft(s.name); }}>{t.rename}</button>
      </>
    );
  const save = async () => {
    try {
      await api.patch(`/sources/${encodeURIComponent(s.id)}`, { name: draft });
    } catch (e) {
      setError(e);
      return;
    }
    setDraft(null);
    onSaved();
  };
  return (
    <form className="rename" aria-label={t.renameLabel(s.name)} onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <input aria-label={t.name} value={draft} maxLength={40} autoFocus onChange={(e) => setDraft(e.target.value)} />{" "}
      <button type="submit" disabled={!draft.trim()}>{t.save}</button> <button type="button" onClick={() => setDraft(null)}>{t.cancel}</button>
      {error !== null && <ErrorNote error={error} />}
    </form>
  );
}

function Detail({ source, rejudgeDays, add, onChooseOutbound, onSaved }: { source: Source; rejudgeDays?: number; add?: string; onChooseOutbound: () => void; onSaved: () => void }) {
  const t = useT(strings);
  const raw = useRaw();
  const st = source.stats;
  return (
    <section role="region" aria-label={t.detail(source.name)} className="panel">
      <h3>{source.name}{raw && <code className="raw-id">{source.id}</code>}</h3>
      {source.type === "webhook" && <WebhookKey source={source} />}
      <DefaultProject source={source} onSaved={onSaved} />
      <TrustedSenders source={source} add={add} />
      <RecentMessages source={source.id} />
      <SourceBacklog source={source.id} rejudgeDays={rejudgeDays} onChooseOutbound={onChooseOutbound} />
      <Digest source={source.id} />
      <details>
        <summary>{t.advanced}</summary>
        <p className="sub">
          {t.stats(st?.runs ?? 0, st?.consecutive_failures ?? 0, st?.timeouts ?? 0)} · {t.lastOk(when(st?.last_success_at))}
          {st?.last_error && <> · {t.lastError(st.last_error)}</>}
        </p>
        {(source.cursor || source.command || raw) && (
          <p className="sub">
            {source.cursor && t.cursor(source.cursor)} {source.command && <code>{source.command}</code>} {source.schedule && <code>{source.schedule}</code>}
          </p>
        )}
        {source.type === "webhook" && <Ingests source={source} />}
        <Rejects source={source} />
      </details>
    </section>
  );
}

/** 消息来源：来源列表；/sources/events/:id 是一条消息的处理经过。 */
export function SourcesPage() {
  return (
    <Routes>
      <Route index element={<SourcesHome />} />
      <Route path="events/:id" element={<MessageTracePage />} />
    </Routes>
  );
}

function SourcesHome() {
  const t = useT(strings);
  const { api } = useSession();
  const [sources, setSources] = useState<Source[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [params] = useSearchParams(); // 挂靠消息的「加入可读名单」带 ?open=<来源>&add=<发件人>
  const [selected, setSelected] = useState<string | null>(params.get("open"));
  const navigate = useNavigate();
  const chooseOutbound = () => navigate(MODEL_SOURCES);
  const [notice, setNotice] = useState<string | null>(null);
  const [rev, setRev] = useState(0); // 积压变化后让打开的详情重读
  const [backlog, setBacklog] = useState<Backlog | null>(null);
  const [models, setModels] = useState<Models | null>(null);
  const advanced = useRef<HTMLDetailsElement>(null);

  const load = useCallback(
    () =>
      api.get<{ sources: Source[] }>("/sources").then(
        (r) => setSources(r.sources),
        (e) => setError(e),
      ),
    [api],
  );
  // 积压的数字与模型现状只用来带出四步与提示，取不到时不显示，不挡来源列表。
  const loadBacklog = useCallback(
    () =>
      api.get<{ signals: HealthSignal[] }>("/health").then(
        (r) => {
          const s = r.signals.find((x) => x.id === "unjudged_backlog");
          setBacklog({ count: s?.count ?? 0, outbound_unset: Number(s?.detail?.outbound_unset ?? 0), rejudgeDays: rejudgeDays(r) });
        },
        () => setBacklog(null),
      ),
    [api],
  );
  useEffect(() => {
    load();
    loadBacklog();
    api.get<Endpoints>("/settings/model-endpoints").then(
      (r) => {
        const judge = !!r.judge_endpoint?.url;
        setModels({ judge, any: judge || !!r.generate_endpoint?.url, paused: !!r.models_paused });
      },
      () => setModels(null),
    );
  }, [api, load, loadBacklog]);

  const toggle = async (s: Source) => {
    setError(null);
    try {
      await api.patch(`/sources/${encodeURIComponent(s.id)}`, { enabled: !s.enabled });
    } catch (e) {
      setError(e);
    }
    await load();
  };
  const changed = () => {
    setRev((n) => n + 1);
    loadBacklog();
  };

  const current = sources?.find((s) => s.id === selected);
  const waiting = backlog?.outbound_unset ?? 0;
  const total = backlog?.count ?? 0;
  const ignoreAll = total > 0 && (
    <IgnoreAll
      label={t.ignoreAll(total)}
      body={{ all: true }}
      onDone={(err, n) => {
        setError(err);
        setNotice(err ? null : t.ignoredAll(n));
        setSelected(null); // 打开的详情列表已过时
        loadBacklog();
      }}
    />
  );
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error !== null && <ErrorNote error={error} />}
      {notice && <p role="status">{notice}</p>}
      {waiting > 0 && <p className="badge warn"><Link to={MODEL_SOURCES}>{t.unsetNotice(waiting)}</Link></p>}
      <section role="region" aria-label={t.list}>
        {sources === null ? (
          error === null && <p role="status">{t.loading}</p>
        ) : sources.length === 0 ? (
          <div className="empty">
            <p>{t.none}</p>
            <button
              className="primary"
              onClick={() => {
                if (advanced.current) advanced.current.open = true;
                document.getElementById("h-register")?.scrollIntoView?.();
              }}
            >
              {t.addFirst}
            </button>
          </div>
        ) : (
          <ul className="rows">
            {sources.map((s) => (
              <li key={s.id} aria-label={s.name}>
                <span aria-hidden="true">{ICON[s.source_type ?? s.type] ?? "\ud83d\udcf0"}</span>{" "}
                <Rename s={s} onSaved={() => { forgetNames(api, "source"); void load(); }} />{" "}
                <span className="sub">{(s.source_type && t.kinds[s.source_type]) || <Val k="sourceType" id={s.type} />}</span> <Schedule s={s.schedule} /> <Status s={s} />
                <div role="group" aria-label={t.ops(s.name)}>
                  <button onClick={() => toggle(s)}>{s.enabled ? t.disable : t.enable}</button> <button onClick={() => setSelected(s.id)}>{t.details}</button>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
      {(total > 0 || inProgress()) && models && <BacklogFlow total={total} models={models} onChanged={changed} ignoreAll={ignoreAll} />}
      {current && <Detail key={`${current.id}:${rev}`} source={current} add={current.id === params.get("open") ? params.get("add") ?? undefined : undefined} rejudgeDays={backlog?.rejudgeDays} onChooseOutbound={chooseOutbound} onSaved={() => void load()} />}
      <details ref={advanced}>
        <summary>{t.advanced}</summary>
        <RestorePanel sources={sources ?? []} onRestored={changed} />
        <Registration sources={sources ?? []} onDone={() => { forgetNames(api, "source"); void load(); }} />
      </details>
    </main>
  );
}
