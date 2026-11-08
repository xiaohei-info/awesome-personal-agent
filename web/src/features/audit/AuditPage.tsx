import { useCallback, useEffect, useState, type FormEvent } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { ActorBadge, type Step } from "../items/ItemsPage";

// 形状对应 GET /v1/audit/events（附录 B.2，§15.3）。筛选、裁剪、分页都在服务端，本页只搬运条件并显示结果；只读，不提供撤销。
const PAGE = 100;
const REJECTED = "command.rejected"; // §6.7：被拒绝的高风险命令记这一类 Event
interface AuditEvent extends Step { object: string; chain_depth: number; grant?: string; redacted?: boolean; tombstone?: boolean }
interface Filter { actor: string; object: string; type: string }
const EMPTY: Filter = { actor: "", object: "", type: "" };

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

function query(f: Filter, after: number) {
  const p = new URLSearchParams({ limit: String(PAGE) });
  if (after) p.set("after", String(after));
  for (const [k, v] of Object.entries(f)) if (v.trim()) p.set(k, v.trim());
  return "/audit/events?" + p;
}

export function AuditPage() {
  const { api } = useSession();
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
      <h2>审计</h2>
      <form onSubmit={submit}>
        {field("actor", "操作者筛选")}
        {field("object", "对象筛选")}
        {field("type", "事件类型筛选")}
        <button type="submit">筛选</button>
        <button type="button" onClick={() => apply({ ...EMPTY, type: REJECTED })}>只看被拒绝的命令</button>
        <button type="button" onClick={() => apply(EMPTY)}>清除</button>
      </form>
      {error && <p role="alert">{error}</p>}
      {!events && !error && <p>加载中…</p>}
      {events && !events.length && <p>没有符合条件的事件</p>}
      {events && events.length > 0 && (
        <ul aria-label="审计事件">
          {events.map((e) => (
            <li key={e.seq}>
              #{e.seq} · {e.type} · {e.object} · <ActorBadge actor={e.actor} /> · {new Date(e.at).toLocaleString()}
              {e.grant ? ` · 授权单 ${e.grant}` : ""}
              {e.reason ? ` · ${e.reason}` : ""}
              {e.redacted || e.tombstone ? " · （内容已隐藏）" : ""}
            </li>
          ))}
        </ul>
      )}
      {more && events && <button onClick={() => void load(applied, events[events.length - 1].seq)}>加载更多</button>}
    </main>
  );
}
