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

// 已完成（界面设计 §9.13）：这一周推进了什么、做成了什么（对齐工作台「本周回顾」）。完成按完成那天、进展按进展日（主机时区）算，周一开始；
// 只读，点一件进事项详情；不主动提醒（同工作台）。
// 完成时刻取自 GET /items?phase=done 的 done_at（附录 B.2）。不含简报（APA 自己写的），委派完成的算、标"委派"。
// 按项目分组，组与组内都是最近完成的在前；没归项目的排最后。地址带 week，可收藏、从详情返回还在原周。

export interface Done { item: Item; at: string; day: string }

/** 全部已完成的事（不含简报），最近完成的在前；day 是完成那天（主机时区）。今天页的完成计数用同一份。 */
export async function loadDone(api: ApiClient, offset: string): Promise<Done[]> {
  return (await loadDoneAll(api, offset)).done;
}

/** 同 loadDone，另给完成日期不详的事（工作台迁移来、没记完成日期；不按任何一天算，不计入完成数）。 */
export async function loadDoneAll(api: ApiClient, offset: string): Promise<{ done: Done[]; unknown: Item[] }> {
  const r = await api.get<{ items: Item[]; done_at?: Record<string, string>; done_unknown?: string[] }>("/items?phase=done");
  const at = r.done_at ?? {};
  const undated = new Set(r.done_unknown ?? []);
  return {
    done: r.items
      .filter((i) => i.kind !== "brief" && at[i.id])
      .map((i) => ({ item: i, at: at[i.id], day: hostParts(at[i.id], offset).date }))
      .sort((a, b) => Date.parse(b.at) - Date.parse(a.at)),
    unknown: r.items.filter((i) => i.kind !== "brief" && undated.has(i.id)),
  };
}

const DATE = /^\d{4}-\d{2}-\d{2}$/;

interface Progress { id: string; text: string; on?: string; at: string }
/** 一条进展记录：同一件事同一天的几条算一条（迁移来的一篇笔记会拆成同日几条），与工作台本周回顾、周报取材同一口径。 */
export interface DayRecord { day: string; lines: string[] }
interface Row { item: Item; done?: Done; records: DayRecord[]; last: string }

/** 这一周有进展的事（不含简报），GET /items?progress_from&progress_to（附录 B.2）；进展日是 on，没写 on 的取记录那天（主机时区）。 */
export async function loadProgress(api: ApiClient, from: string, to: string, offset: string): Promise<{ item: Item; records: DayRecord[]; last: string }[]> {
  const r = await api.get<{ items: Item[]; progress?: { [id: string]: Progress[] } }>(`/items?progress_from=${from}&progress_to=${to}`);
  return r.items.filter((i) => i.kind !== "brief").map((i) => {
    const days = new Map<string, string[]>();
    for (const p of r.progress?.[i.id] ?? []) {
      const day = p.on || hostParts(p.at, offset).date;
      days.set(day, [...(days.get(day) ?? []), p.text]);
    }
    const ps = r.progress?.[i.id] ?? [];
    return { item: i, records: [...days].sort(([a], [b]) => a.localeCompare(b)).map(([day, lines]) => ({ day, lines })), last: ps.length ? ps[ps.length - 1].at : "" };
  });
}

const firstLine = (s: string) => s.split("\n")[0];

export function DonePage() {
  const { api } = useSession();
  const t = useT(strings);
  const projects = useNamed("project");
  const [params, setParams] = useSearchParams();
  const [clock, setClock] = useState<HostClock | null>(null);
  const [all, setAll] = useState<{ done: Done[]; unknown: Item[] } | null>(null);
  const [moved, setMoved] = useState<{ week: string; list: Awaited<ReturnType<typeof loadProgress>> } | null>(null);
  const [error, setError] = useState<unknown>(null);

  const thisWeek = clock ? monday(clock.today) : "";
  const w = params.get("week");
  const week = w && DATE.test(w) ? monday(w) : thisWeek;
  const end = week && addDays(week, 6);

  useEffect(() => void hostClock(api).then(setClock), [api]);
  useEffect(() => void (clock && loadDoneAll(api, clock.utc_offset).then(setAll, setError)), [api, clock]);
  useEffect(() => void (clock && week && loadProgress(api, week, end, clock.utc_offset).then((list) => setMoved({ week, list }), setError)), [api, clock, week, end]);

  const go = (d: string) => setParams(d === thisWeek ? {} : { week: d });
  const done = all?.done;
  const unknown = all?.unknown ?? [];
  const list = done?.filter((d) => d.day >= week && d.day <= end) ?? [];
  const progressed = moved?.week === week ? moved.list : [];
  // 每件事一行：这周完成的带完成那天，这周有进展的带每天的进展；按项目分组，组与组内都是最近动过的在前，没归项目的排最后。
  const rows = new Map<string, Row>();
  for (const d of list) rows.set(d.item.id, { item: d.item, done: d, records: [], last: d.at });
  for (const p of progressed) {
    const r = rows.get(p.item.id);
    if (r) { r.records = p.records; if (p.last > r.last) r.last = p.last; }
    else rows.set(p.item.id, { item: p.item, records: p.records, last: p.last });
  }
  const groups = new Map<string, Row[]>();
  for (const r of [...rows.values()].sort((a, b) => b.last.localeCompare(a.last))) groups.set(r.item.project ?? "", [...(groups.get(r.item.project ?? "") ?? []), r]);
  const ordered = [...groups].sort(([a], [b]) => Number(!a) - Number(!b));
  const name = (id: string) => projects.find((p) => p.id === id)?.name ?? "";
  // 统计同工作台本周回顾：进展按条（同事同日算一条）、完成按件、涉及项目不算没归项目的。
  const stats = (rs: Row[]) => ({ progress: rs.reduce((n, r) => n + r.records.length, 0), done: rs.filter((r) => r.done).length });
  const total = stats([...rows.values()]);
  const unknownIds = new Set(unknown.map((i) => i.id));
  const day = (d: string) => t.on(t.weekdays[(new Date(d + "T00:00:00Z").getUTCDay() + 6) % 7], d);

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
      {!done || moved?.week !== week ? (
        error == null && <p role="status">{t.loading}</p>
      ) : !rows.size ? (
        <>
          <p className="empty">{week === thisWeek ? t.emptyThis : t.emptyPast}</p>
          {week === thisWeek && <p><button type="button" className="link" onClick={() => go(addDays(week, -7))}>{t.seeLast}</button></p>}
        </>
      ) : (
        <>
          <p className="sub">{t.stats(total.progress, total.done, ordered.filter(([pid]) => pid).length)}</p>
          {ordered.map(([pid, rs]) => {
            const s = stats(rs);
            return (
              <section key={pid} aria-label={pid ? name(pid) : t.noProject}>
                <h2>{pid ? <Link to={"/projects/" + encodeURIComponent(pid)}>{name(pid)}</Link> : t.noProject}</h2>
                <p className="sub">{t.groupStats(s.progress, s.done)}</p>
                <ul className="rows">
                  {rs.map((r) => (
                    <li key={r.item.id} className="act-row">
                      <div>
                        <Go to={"/items/" + encodeURIComponent(r.item.id)}>{r.item.title}</Go>
                        <span className="sub">
                          {r.done ? t.doneOn(day(r.done.day)) : unknownIds.has(r.item.id) ? t.doneUnknown : <Val k="phase" id={r.item.phase} />}
                          {r.item.kind === "delegation" && <> · <Val k="itemKind" id="delegation" /></>}
                        </span>
                        {r.records.length > 0 && (
                          <ul className="sub" aria-label={t.progressOf(r.item.title)}>
                            {r.records.map((x) => <li key={x.day} title={x.lines.join("\n")}>{day(x.day)} {x.lines.map(firstLine).join("；")}</li>)}
                          </ul>
                        )}
                      </div>
                    </li>
                  ))}
                </ul>
              </section>
            );
          })}
        </>
      )}
      {done && unknown.length > 0 && (
        <section aria-label={t.unknownTitle}>
          <h2>{t.unknownTitle}</h2>
          <p className="sub">{t.unknownNote(unknown.length)}</p>
          <ul className="rows">
            {unknown.map((i) => <li key={i.id} className="act-row"><div><Go to={"/items/" + encodeURIComponent(i.id)}>{i.title}</Go></div></li>)}
          </ul>
        </section>
      )}
    </main>
  );
}
