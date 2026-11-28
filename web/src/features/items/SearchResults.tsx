import { useEffect, useState } from "react";
import { Link, useLocation } from "react-router-dom";
import { useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { ErrorNote } from "../../app/errors";
import { EmptyState, Highlight, Skeleton, terms } from "../../app/ui";
import { Val } from "../../app/values";
import type { Item } from "./ItemsPage";
import { strings } from "./strings";

// 事项页的检索（主文档 §7.1，GET /search?type=item）：搜索框有字时列表换成按相关度排的命中，筛选项目、阶段、类型、标签、
// 最近变更的时间段都交给服务端；一页 20 条，按 next_cursor 往后翻。每条命中逐级展开：先是标题与一段片段（命中的词标出），
// 「展开」再读这件事，列出正文与进展里含命中词的段落，最后「打开」进详情。已放弃的默认不出，写明还有几件。

export interface SearchHit { id: string; title: string; project?: string; project_name?: string; phase?: string; kind?: string; updated?: string; matched_in?: string[]; snippet?: string }
interface Page { hits: SearchHit[]; total?: number; withheld?: { dropped?: number }; next_cursor?: string }
export interface SearchQuery { q: string; project?: string; phase?: string; kind?: string; tag?: string; after?: string; before?: string }

const PAGE = 20;

export function SearchResults({ query }: { query: SearchQuery }) {
  const t = useT(strings);
  const { api } = useSession();
  const [cursors, setCursors] = useState<string[]>([""]); // 每页的起点；最后一个是当前页
  const [page, setPage] = useState<Page | null>(null);
  const [error, setError] = useState<unknown>(null);
  const key = JSON.stringify(query);
  useEffect(() => setCursors([""]), [key]);
  const cursor = cursors[cursors.length - 1];
  // 停手 250ms 后检索；过时的响应丢掉。
  useEffect(() => {
    let live = true;
    setError(null);
    const sp = new URLSearchParams({ q: query.q.trim(), type: "item", limit: String(PAGE) });
    for (const k of ["project", "phase", "kind", "tag", "after", "before"] as const) if (query[k]) sp.set(k, query[k]!);
    if (query.phase === "dropped") sp.set("include_dropped", "true");
    if (cursor) sp.set("cursor", cursor);
    const timer = setTimeout(() => {
      api.get<Page>("/search?" + sp).then((r) => live && setPage(r), (e) => live && setError(e));
    }, 250);
    return () => { live = false; clearTimeout(timer); };
  }, [api, key, cursor]); // eslint-disable-line react-hooks/exhaustive-deps

  if (error != null) return <ErrorNote error={error} />;
  if (!page) return <Skeleton label={t.searching} />;
  const n = cursors.length;
  return (
    <section aria-label={t.searchResults} aria-busy={false}>
      <p className="sub search-summary">
        {t.searchTotal(page.total ?? page.hits.length)}
        {page.withheld?.dropped ? ` · ${t.searchDropped(page.withheld.dropped)}` : ""}
      </p>
      {!page.hits.length ? (
        <EmptyState title={t.searchNone} note={t.searchNoneNote} />
      ) : (
        <ul className="rows search-hits" aria-label={t.searchResults}>
          {page.hits.map((h) => <HitRow key={h.id} h={h} q={query.q} />)}
        </ul>
      )}
      {(n > 1 || page.next_cursor) && (
        <nav className="pager" aria-label={t.pager}>
          <button type="button" disabled={n <= 1} onClick={() => setCursors(cursors.slice(0, -1))}>{t.prev}</button>
          <span className="sub">{t.searchPage(n)}</span>
          <button type="button" disabled={!page.next_cursor} onClick={() => setCursors([...cursors, page.next_cursor!])}>{t.next}</button>
        </nav>
      )}
    </section>
  );
}

/** 一条命中：标题、项目与阶段、片段；「展开」读这件事，列出含命中词的段落。 */
function HitRow({ h, q }: { h: SearchHit; q: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const location = useLocation();
  const [open, setOpen] = useState(false);
  const [passages, setPassages] = useState<{ where: string; text: string }[] | null>(null);
  const to = "/items/" + encodeURIComponent(h.id);
  const expand = async () => {
    setOpen(!open);
    if (passages || open) return;
    const r = await api.get<{ item: Item; progress?: { text: string }[] }>(to).catch(() => null);
    if (!r) return setPassages([]);
    const ts = terms(q).map((x) => x.toLowerCase());
    const hit = (s: string) => ts.some((x) => s.toLowerCase().includes(x));
    const paras = (r.item.body ?? "").split(/\n\s*\n|\n(?=[-*#>])/).map((s) => s.trim()).filter(Boolean);
    setPassages([
      ...paras.filter(hit).map((text) => ({ where: t.inBody, text })),
      ...(r.progress ?? []).map((p) => p.text).filter(hit).map((text) => ({ where: t.inDiscussion, text })),
    ].slice(0, 6));
  };
  return (
    <li className="hit" data-item={h.id}>
      <div className="hit-line">
        <Link className="trow-title" to={to} state={{ list: location.search }}><Highlight text={h.title} q={q} /></Link>
        <span className="sub reasons">
          {h.project_name && <span>{h.project_name}</span>}
          {h.phase && <span className={h.phase === "needs_input" ? "bad" : undefined}><Val k="phase" id={h.phase} /></span>}
          {h.updated && <span>{h.updated}</span>}
        </span>
        <button type="button" className="quiet" aria-expanded={open} onClick={() => void expand()}>{open ? t.collapse : t.expandHit}</button>
      </div>
      {h.snippet && <p className="hit-snippet"><Highlight text={h.snippet} q={q} /></p>}
      {open && (
        <div className="hit-more">
          {h.matched_in?.length ? <p className="sub">{t.matchedIn(h.matched_in.map((m) => t.matchField[m] ?? m).join(t.listSep))}</p> : null}
          {passages === null ? <Skeleton n={2} label={t.loading} /> : passages.length === 0 ? <p className="sub">{t.noPassage}</p> : (
            <ul className="plain">
              {passages.map((p, i) => <li key={i}><span className="badge plain">{p.where}</span> <Highlight text={p.text} q={q} /></li>)}
            </ul>
          )}
          <Link to={to} state={{ list: location.search }}>{t.openItem}</Link>
        </div>
      )}
    </li>
  );
}
