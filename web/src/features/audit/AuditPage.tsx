import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { ActorBadge, type Step } from "../items/ItemsPage";
import { locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useItemTitle, useNamed, useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { useLabel, Val } from "../../app/values";
import { EVENT, EVENT_INTERNAL } from "../../app/ids";
import { strings } from "./strings";

// 诊断 › 操作记录（原「审计」，界面设计 §9.10）：形状对应 GET /v1/audit/events（附录 B.2，§15.3）。
// 筛选（actor、object、type 精确匹配）、裁剪、分页都在服务端，本页只搬运条件并显示结果；只读，不提供撤销。
// 最新的在最前：order=desc，往下翻用 before=<最后一条的序号> 取更早的。
const PAGE = 100;
const REJECTED = "command.rejected"; // §6.7：被拒绝的命令记这一类 Event
const OWNER = "owner"; // Web 上本人的 principal（api/core）
// basis.via 是这条记录凭什么生效（设计 §14.3）；grant 只出现在切换前的旧模型记录里。
interface AuditEvent extends Step { object: string; chain_depth: number; basis?: { via?: string }; grant?: string; redacted?: boolean; tombstone?: boolean; imported?: string; recorded_at?: string }
interface Filter { actor: string; object: string; type: string }
const EMPTY: Filter = { actor: "", object: "", type: "" };

type Group = keyof (typeof strings)["zh"]["typeGroups"];
const groupOf = (type: string): Group => {
  const p = type.split(".")[0];
  if (["item", "proposal", "decision", "command"].includes(p)) return "items";
  if (["delegation", "run", "followmsg", "timer"].includes(p)) return "agents";
  if (["material", "guideline", "project"].includes(p)) return "projects";
  if (["rule", "policy", "allowlist", "grant", "executor", "instance", "authz"].includes(p)) return "settings";
  if (["source", "inbound", "health"].includes(p)) return "sources";
  if (p === "auth") return "login";
  return "other";
};
const GROUPS: Group[] = ["items", "agents", "projects", "settings", "sources", "login", "other"];

function query(f: Filter, before: number) {
  const p = new URLSearchParams({ limit: String(PAGE), order: "desc" });
  if (before) p.set("before", String(before));
  for (const [k, v] of Object.entries(f)) if (v.trim()) p.set(k, v.trim());
  return "/audit/events?" + p;
}

function Row({ e }: { e: AuditEvent }) {
  const t = useT(strings);
  const raw = useRaw();
  const title = useItemTitle(e.object.startsWith("itm_") ? e.object : null);
  const source = useNames("source");
  // 对象按名字称呼：事项链到事项，来源（含对话捕获）用显示名；其余原样。
  const what = e.object.startsWith("itm_") ? <Link to={"/items/" + e.object}>{title}</Link> : /^(src_|capture:)/.test(e.object) ? source(e.object) : e.object;
  return (
    <li>
      <span>
        <ActorBadge actor={e.actor} /> <Val k="event" id={e.type} /> {what} · {new Date(e.at).toLocaleString(locale())}
        {e.basis?.via && t.basis[e.basis.via] ? ` · ${t.basis[e.basis.via]}` : e.grant ? ` · ${t.legacy}` : ""}
        {e.redacted || e.tombstone ? ` · ${t.hidden}` : ""}
      </span>
      {raw && <code className="raw-id">{[`#${e.seq}`, e.type, e.object, e.reason, e.grant].filter(Boolean).join(" · ")}</code>}
    </li>
  );
}

// 导入事件在序号上是连续的一段（设计 §6.7），所以相邻且批次相同的事件折成一组；普通事件各自一组。
type Entry = { batch?: string; list: AuditEvent[] };
const fold = (events: AuditEvent[]): Entry[] => {
  const out: Entry[] = [];
  for (const e of events) {
    const last = out[out.length - 1];
    if (e.imported && last?.batch === e.imported) last.list.push(e);
    else out.push({ batch: e.imported || undefined, list: [e] });
  }
  return out;
};

function ImportFold({ entry }: { entry: Entry }) {
  const t = useT(strings);
  const raw = useRaw();
  const rec = entry.list[0].recorded_at;
  return (
    <li>
      <details className="import-fold">
        <summary>
          {t.imported(entry.list.length)}{rec ? ` · ${new Date(rec).toLocaleString(locale())}` : ""}
          {raw && <code className="raw-id">{entry.batch}</code>}
        </summary>
        <ul className="rows">{entry.list.map((e) => <Row key={e.seq} e={e} />)}</ul>
      </details>
    </li>
  );
}

export function AuditPage() {
  const { api } = useSession();
  const t = useT(strings);
  const label = useLabel();
  const credentials = useNamed("credential");
  const [params] = useSearchParams();
  const initial = { ...EMPTY, object: params.get("object") ?? "" };
  const [draft, setDraft] = useState<Filter>(initial);
  const [applied, setApplied] = useState<Filter>(initial);
  const [events, setEvents] = useState<AuditEvent[] | null>(null);
  const [more, setMore] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const load = useCallback(async (f: Filter, before: number) => {
    try {
      const r = await api.get<{ events: AuditEvent[] }>(query(f, before));
      setError(null);
      setMore(r.events.length === PAGE);
      setEvents((prev) => (before ? [...(prev ?? []), ...r.events] : r.events));
    } catch (e) {
      setEvents(null); // 出错时不保留旧列表，避免把过期结果当成当前筛选的结果
      setMore(false);
      setError(e);
    }
  }, [api]);

  useEffect(() => { void load(applied, 0); }, [load, applied]);

  const apply = (f: Filter) => {
    setDraft(f);
    // 条件不变时对象引用也要变，才会重新查询
    setApplied({ ...f });
  };
  const submit = (e: FormEvent) => { e.preventDefault(); apply(draft); };
  const types = [...EVENT, ...EVENT_INTERNAL];

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <form className="panel inline" onSubmit={submit}>
        <label>
          {t.actor}
          <select value={draft.actor} onChange={(e) => setDraft({ ...draft, actor: e.target.value })}>
            <option value="">{t.anyone}</option>
            <option value={OWNER}>{t.you}</option>
            {credentials.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
        </label>
        <label>
          {t.type}
          <select value={draft.type} onChange={(e) => setDraft({ ...draft, type: e.target.value })}>
            <option value="">{t.anything}</option>
            {GROUPS.map((g) => {
              const list = types.filter((x) => groupOf(x) === g);
              return list.length ? (
                <optgroup key={g} label={t.typeGroups[g]}>
                  {list.map((x) => <option key={x} value={x}>{label("event", x)}</option>)}
                </optgroup>
              ) : null;
            })}
          </select>
        </label>
        <details open={!!draft.object}>
          <summary>{t.advanced}</summary>
          <label>
            {t.object}
            <input value={draft.object} onChange={(e) => setDraft({ ...draft, object: e.target.value })} />
          </label>
        </details>
        <div className="actions">
          <button type="submit">{t.filter}</button>
          <button type="button" onClick={() => apply({ ...EMPTY, type: REJECTED })}>{t.rejectedOnly}</button>
          <button type="button" onClick={() => apply(EMPTY)}>{t.clear}</button>
        </div>
      </form>
      {error != null && <ErrorNote error={error} />}
      {!events && error == null && <p role="status">{t.loading}</p>}
      {events && !events.length && <p className="empty">{t.empty}</p>}
      {events && events.length > 0 && (
        <ul aria-label={t.events} className="rows">
          {fold(events).map((g) => g.batch ? <ImportFold key={g.list[0].seq} entry={g} /> : <Row key={g.list[0].seq} e={g.list[0]} />)}
        </ul>
      )}
      {more && events && <button onClick={() => void load(applied, events[events.length - 1].seq)}>{t.more}</button>}
    </main>
  );
}
