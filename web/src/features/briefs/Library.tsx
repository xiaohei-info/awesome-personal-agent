import { useEffect, useState } from "react";
import { Link, NavLink, useSearchParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useLabel } from "../../app/values";
import { BriefBody, when, type Brief } from "./BriefBody";
import type { Instance } from "./Routine";
import { When } from "./ScheduleCard";
import { strings } from "./strings";

export const PAGE = 20;

/** 简报页的导航：看已生成的 / 管安排。 */
export function Tabs() {
  const t = useT(strings);
  return (
    <nav aria-label={t.title} className="subnav">
      <NavLink to="/briefs" end>{t.tabRead}</NavLink>
      <NavLink to="/briefs/schedule">{t.tabSchedule}</NavLink>
    </nav>
  );
}

// 读已生成的简报：最新一份整份展开，其余按时间倒排成历史列表（?page= 翻页，刷新与返回都还在）。
// GET /v1/items?kind=brief 已含全部历史简报（服务端按 id 返回，这里按 created_at 倒排）；旧简报没有 created_at 时排在最后。
export function Library() {
  const { api } = useSession();
  const t = useT(strings);
  const label = useLabel();
  const [briefs, setBriefs] = useState<Brief[] | null>(null);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [q, setQ] = useSearchParams();
  useEffect(() => {
    Promise.all([api.get<{ items: Brief[] }>("/items?kind=brief"), api.get<{ instances: Instance[] }>("/instances")]).then(([b, i]) => {
      setBriefs([...b.items].sort((x, y) => (y.created_at ?? "").localeCompare(x.created_at ?? "") || y.id.localeCompare(x.id)));
      setInstances(i.instances);
    }, setError);
  }, [api]);

  if (!briefs) return <main>{error != null ? <ErrorNote error={error} /> : <p role="status">{t.loading}</p>}</main>;
  // 早、晚等不同安排各自的简报：来源实例的包名 + 定时。
  const origin = (b: Brief) => {
    const i = instances.find((x) => b.origin?.source === `instance:${x.id}`);
    // 点来源进安排页并展开这张卡片：改内容要求的入口就在读简报的地方。实例 id 走路由 state，不进地址栏。
    return i ? <Link to="schedule" state={{ open: i.id }}>{label("pack", i.pack)} · <When schedule={i.params.schedule} /></Link> : null;
  };
  const [latest, ...older] = briefs;
  const pages = Math.max(1, Math.ceil(older.length / PAGE));
  const page = Math.min(pages, Math.max(1, Number(q.get("page")) || 1));
  return (
    <main>
      <PageHead>{t.title}</PageHead>
      <Tabs />
      {error != null && <ErrorNote error={error} />}
      {!latest ? (
        <p className="sub">{t.none} <Link to="schedule" state={{ suggest: true }}>{t.suggest}</Link></p>
      ) : (
        <>
          <section aria-label={t.latest}>
            <h2>{t.latest}</h2>
            <article aria-label={t.brief(latest.title)}>
              <h3 className="brief-title"><Link to={encodeURIComponent(latest.id)}>{latest.title}</Link></h3>
              <p className="meta">{[when(latest.created_at), latest.spec?.period].filter(Boolean).join(" · ")}{origin(latest) && <> · {origin(latest)}</>}</p>
              <BriefBody spec={latest.spec} />
            </article>
          </section>
          <section aria-label={t.history}>
            <h2>{t.history}</h2>
            {!older.length && <p className="sub">{t.noHistory}</p>}
            <ul aria-label={t.historyList} className="rows">
              {older.slice((page - 1) * PAGE, page * PAGE).map((b) => (
                <li key={b.id}>
                  <Link to={encodeURIComponent(b.id)}>{b.title}</Link>
                  <span className="sub">{when(b.created_at) || t.noTime}{origin(b) && <> · {origin(b)}</>}</span>
                </li>
              ))}
            </ul>
            {pages > 1 && (
              <p className="pager">
                <button type="button" disabled={page <= 1} onClick={() => setQ({ page: String(page - 1) })}>{t.prev}</button>
                <span>{t.pageOf(page, pages)}</span>
                <button type="button" disabled={page >= pages} onClick={() => setQ({ page: String(page + 1) })}>{t.next}</button>
              </p>
            )}
          </section>
        </>
      )}
    </main>
  );
}
