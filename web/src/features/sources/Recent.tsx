import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { Val } from "../../app/values";
import { TraceView } from "../trace/Trace";
import { strings } from "./strings";
import { ago, when, type SourceEvent } from "./shared";

// 最近的消息（设计 §15.1）：这个来源进来的全部消息，从新到旧；点一行就地展开它的处理经过。
const FILTERS: Record<string, string> = { all: "", proposed: "proposed", ignored: "ignored", attached: "attached", backlog: "unjudged,expired" };
const PAGE = 20;

function Row({ v }: { v: SourceEvent }) {
  const t = useT(strings);
  const [open, setOpen] = useState(false);
  const subject = v.subject || v.type;
  return (
    <li>
      <button type="button" className="quiet event-row" aria-expanded={open} onClick={() => setOpen(!open)}>
        <span>
          <strong>{subject}</strong>
          {v.sender && <span className="sub"> · {v.sender}{v.sender_verified ? ` · ${t.verified}` : ""}</span>}
        </span>
        <span className="badge"><Val k="disposition" id={v.disposition} /></span>
        <time className="sub" dateTime={v.occurred_at} title={when(v.occurred_at)}>{ago(v.occurred_at)}</time>
      </button>
      {open && (
        <div role="region" aria-label={t.traceOf(subject)}>
          <TraceView path={`/source-events/${encodeURIComponent(v.id)}/trace`} />
          <p className="sub"><Link to={"/sources/events/" + encodeURIComponent(v.id)}>{t.openAlone}</Link></p>
        </div>
      )}
    </li>
  );
}

export function RecentMessages({ source }: { source: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [filter, setFilter] = useState("all");
  const [items, setItems] = useState<SourceEvent[] | null>(null);
  const [next, setNext] = useState<number | null>(null);
  const [error, setError] = useState<unknown>(null);
  const seq = useRef(0); // 换筛选后丢弃迟到的旧响应
  const load = useCallback(
    (after?: number) => {
      const me = ++seq.current;
      const p = new URLSearchParams({ sort: "recent", limit: String(PAGE) });
      if (FILTERS[filter]) p.set("disposition", FILTERS[filter]);
      if (after) p.set("after", String(after));
      api.get<{ events: SourceEvent[]; next_after?: number }>(`/sources/${encodeURIComponent(source)}/events?${p}`).then(
        (r) => {
          if (me !== seq.current) return;
          setError(null);
          setItems((cur) => (after && cur ? [...cur, ...r.events] : r.events));
          setNext(r.next_after ?? null);
        },
        (e) => me === seq.current && setError(e),
      );
    },
    [api, source, filter],
  );
  useEffect(() => {
    setItems(null);
    load();
  }, [load]);
  return (
    <section role="region" aria-label={t.recent}>
      <h4>{t.recent}</h4>
      <div role="group" aria-label={t.recent} className="seg">
        {Object.keys(FILTERS).map((f) => (
          <button key={f} type="button" className="quiet" aria-pressed={filter === f} onClick={() => setFilter(f)}>{t.filters[f]}</button>
        ))}
      </div>
      {error !== null && <ErrorNote error={error} />}
      {items === null ? (
        error === null && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="sub">{t.noRecent}</p>
      ) : (
        <ul className="rows" aria-label={t.recent}>
          {items.map((v) => <Row key={v.id} v={v} />)}
        </ul>
      )}
      {next !== null && <p><button type="button" onClick={() => load(next)}>{t.loadMore}</button></p>}
    </section>
  );
}
