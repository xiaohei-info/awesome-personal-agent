import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import type { ApiClient } from "../../app/api";
import { ErrorNote } from "../../app/errors";
import { useT } from "../../app/i18n";
import { useNamed } from "../../app/names";
import { Go, PageHead } from "../../app/page";
import { useSession } from "../../app/session";
import { Val } from "../../app/values";
import { addDays, monday } from "../calendar/layout";
import type { Item } from "../items/ItemsPage";
import { hostClock, hostParts, type HostClock } from "../items/schedule";
import { strings } from "./strings";

// 已完成（界面设计 §9.13）：这一周做成了什么。按完成那天（主机时区）算、周一开始；只读，点一件进事项详情。
// 完成时刻取自 GET /items?phase=done 的 done_at（附录 B.2）。不含简报（APA 自己写的），委派完成的算、标"委派"。
// 按项目分组，组与组内都是最近完成的在前；没归项目的排最后。地址带 week，可收藏、从详情返回还在原周。

export interface Done { item: Item; at: string; day: string }

/** 全部已完成的事（不含简报），最近完成的在前；day 是完成那天（主机时区）。今天页的完成计数用同一份。 */
export async function loadDone(api: ApiClient, offset: string): Promise<Done[]> {
  const r = await api.get<{ items: Item[]; done_at?: Record<string, string> }>("/items?phase=done");
  const at = r.done_at ?? {};
  return r.items
    .filter((i) => i.kind !== "brief" && at[i.id])
    .map((i) => ({ item: i, at: at[i.id], day: hostParts(at[i.id], offset).date }))
    .sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
}

const DATE = /^\d{4}-\d{2}-\d{2}$/;

export function DonePage() {
  const { api } = useSession();
  const t = useT(strings);
  const projects = useNamed("project");
  const [params, setParams] = useSearchParams();
  const [clock, setClock] = useState<HostClock | null>(null);
  const [done, setDone] = useState<Done[] | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => void hostClock(api).then(setClock), [api]);
  useEffect(() => void (clock && loadDone(api, clock.utc_offset).then(setDone, setError)), [api, clock]);

  const thisWeek = clock ? monday(clock.today) : "";
  const w = params.get("week");
  const week = w && DATE.test(w) ? monday(w) : thisWeek;
  const end = week && addDays(week, 6);
  const go = (d: string) => setParams(d === thisWeek ? {} : { week: d });
  const list = done?.filter((d) => d.day >= week && d.day <= end) ?? [];
  const groups = new Map<string, Done[]>();
  for (const d of list) groups.set(d.item.project ?? "", [...(groups.get(d.item.project ?? "") ?? []), d]);
  const ordered = [...groups].sort(([a], [b]) => Number(!a) - Number(!b));
  const name = (id: string) => projects.find((p) => p.id === id)?.name ?? "";

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {week && (
        <div className="actions">
          <div className="seg">
            <button type="button" aria-label={t.prev} onClick={() => go(addDays(week, -7))}>‹</button>
            <button type="button" aria-pressed={week === thisWeek} onClick={() => go(thisWeek)}>{t.thisWeek}</button>
            <button type="button" aria-label={t.next} disabled={week >= thisWeek} onClick={() => go(addDays(week, 7))}>›</button>
          </div>
          <strong>{t.period(week, end)}</strong>
        </div>
      )}
      {error != null && <ErrorNote error={error} />}
      {!done ? (
        error == null && <p role="status">{t.loading}</p>
      ) : !list.length ? (
        <>
          <p className="empty">{week === thisWeek ? t.emptyThis : t.emptyPast}</p>
          {week === thisWeek && <p><button type="button" className="link" onClick={() => go(addDays(week, -7))}>{t.seeLast}</button></p>}
        </>
      ) : (
        <>
          <p className="sub">{ordered.length > 1 ? t.total(list.length, ordered.length) : t.totalOne(list.length)}</p>
          {ordered.map(([pid, ds]) => (
            <section key={pid} aria-label={pid ? name(pid) : t.noProject}>
              <h2>{pid ? <Link to={"/projects/" + encodeURIComponent(pid)}>{name(pid)}</Link> : t.noProject} <span className="sub">{t.count(ds.length)}</span></h2>
              <ul className="rows">
                {ds.map((d) => (
                  <li key={d.item.id} className="act-row">
                    <div>
                      <Go to={"/items/" + encodeURIComponent(d.item.id)}>{d.item.title}</Go>
                      <span className="sub">
                        {t.on(t.weekdays[(new Date(d.day + "T00:00:00Z").getUTCDay() + 6) % 7], d.day)}
                        {d.item.kind === "delegation" && <> · <Val k="itemKind" id="delegation" /></>}
                      </span>
                    </div>
                  </li>
                ))}
              </ul>
            </section>
          ))}
        </>
      )}
    </main>
  );
}
