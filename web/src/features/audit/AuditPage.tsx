import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { ActorBadge, type Step } from "../items/ItemsPage";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/audit/events（附录 B.2，§15.3）。筛选、裁剪、分页都在服务端，本页只搬运条件并显示结果；只读，不提供撤销。
const PAGE = 100;
const REJECTED = "command.rejected"; // §6.7：被拒绝的高风险命令记这一类 Event
interface AuditEvent extends Step { object: string; chain_depth: number; grant?: string; redacted?: boolean; tombstone?: boolean }
interface Filter { actor: string; object: string; type: string }
const EMPTY: Filter = { actor: "", object: "", type: "" };


function query(f: Filter, after: number) {
  const p = new URLSearchParams({ limit: String(PAGE) });
  if (after) p.set("after", String(after));
  for (const [k, v] of Object.entries(f)) if (v.trim()) p.set(k, v.trim());
  return "/audit/events?" + p;
}

export function AuditPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [draft, setDraft] = useState<Filter>(EMPTY);
  const [applied, setApplied] = useState<Filter>(EMPTY);
  const [events, setEvents] = useState<AuditEvent[] | null>(null);
  const [more, setMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async (f: Filter, after: number) => {
    try {
      const r = await api.get<{ events: AuditEvent[] }>(query(f, after));
      setError(null);
      setMore(r.events.length === PAGE);
      setEvents((prev) => (after ? [...(prev ?? []), ...r.events] : r.events));
    } catch (e) {
      setEvents(null); // 出错时不保留旧列表，避免把过期结果当成当前筛选的结果
      setMore(false);
      setError(msg(e));
    }
  }, [api]);

  useEffect(() => { void load(applied, 0); }, [load, applied]);

  const apply = (f: Filter) => {
    setDraft(f);
    // 条件不变时对象引用也要变，才会重新查询
    setApplied({ ...f });
  };
  const submit = (e: FormEvent) => { e.preventDefault(); apply(draft); };
  const field = (k: keyof Filter, label: string) => (
    <label>
      {label}
      <input value={draft[k]} onChange={(e) => setDraft({ ...draft, [k]: e.target.value })} />
    </label>
  );

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <form className="panel inline" onSubmit={submit}>
        {field("actor", t.actor)}
        {field("object", t.object)}
        {field("type", t.type)}
        <div className="actions">
          <button type="submit">{t.filter}</button>
          <button type="button" onClick={() => apply({ ...EMPTY, type: REJECTED })}>{t.rejectedOnly}</button>
          <button type="button" onClick={() => apply(EMPTY)}>{t.clear}</button>
        </div>
      </form>
      {error && <p role="alert">{error}</p>}
      {!events && !error && <p role="status">{t.loading}</p>}
      {events && !events.length && <p className="empty">{t.empty}</p>}
      {events && events.length > 0 && (
        <ul aria-label={t.events} className="rows">
          {events.map((e) => (
            <li key={e.seq}>
              <span>
                #{e.seq} · {e.type} · {e.object} · <ActorBadge actor={e.actor} /> · {new Date(e.at).toLocaleString(locale())}
                {e.grant ? t.grant(e.grant) : ""}
                {e.reason ? ` · ${e.reason}` : ""}
                {e.redacted || e.tombstone ? t.hidden : ""}
              </span>
            </li>
          ))}
        </ul>
      )}
      {more && events && <button onClick={() => void load(applied, events[events.length - 1].seq)}>{t.more}</button>}
    </main>
  );
}
