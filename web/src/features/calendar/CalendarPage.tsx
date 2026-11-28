import { useCallback, useEffect, useLayoutEffect, useRef, useState, type CSSProperties, type ReactNode, type RefObject } from "react";
import { EmptyState, Skeleton } from "../../app/ui";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { ErrorNote } from "../../app/errors";
import { useT } from "../../app/i18n";
import { useNamed } from "../../app/names";
import { Go, PageHead } from "../../app/page";
import { useRaw } from "../../app/raw";
import { useSession } from "../../app/session";
import { useNarrow } from "../../app/Shell";
import { useLabel } from "../../app/values";
import type { Item } from "../items/ItemsPage";
import { browserClock, hhmm, hostClock, hostParts, slotOf, type HostClock } from "../items/schedule";
import { entries, lanes, ordered, range, scrollStart, split, step, VIEWS, type Entry, type View } from "./layout";
import { strings } from "./strings";
import "./calendar.css";

// 日历（界面设计 §9.12）：事项的只读读法。数据取自 GET /items?from=&to=，日界按主机时区；页面不写任何东西，
// 改期在事项详情的排期区块，新建去新建页（只预填）。地址带 view、date、project，可收藏、可回程还原。

const VIEW_KEY = "apa.calendar.view"; // 视图选择只记在这台浏览器，与"上次打开今天页"同一类本地记忆
const SCROLL_KEY = "apa.calendar.scroll";
const DATE = /^\d{4}-\d{2}-\d{2}$/;

const isView = (v: string | null): v is View => !!v && (VIEWS as string[]).includes(v);

export function CalendarPage() {
  const { api } = useSession();
  const t = useT(strings);
  const narrow = useNarrow();
  const [params, setParams] = useSearchParams();
  const { pathname, search } = useLocation();
  const [clock, setClock] = useState<HostClock | null>(null);
  const [items, setItems] = useState<Item[] | null>(null);
  const [overdue, setOverdue] = useState(0);
  const [error, setError] = useState<unknown>(null);
  const grid = useRef<HTMLDivElement>(null);

  // 地址里带了参数就以地址为准；没带时视图取这台浏览器记住的，第一次打开桌面是周、窄屏是日；日期每次从今天开始。
  const stored = localStorage.getItem(VIEW_KEY);
  const view: View = isView(params.get("view")) ? (params.get("view") as View) : isView(stored) ? stored : narrow ? "day" : "week";
  const pDate = params.get("date");
  const date = pDate && DATE.test(pDate) ? pDate : clock?.today ?? "";
  const project = params.get("project") ?? "";
  const r = date ? range(view, date) : null;

  useEffect(() => void hostClock(api).then(setClock), [api]);

  const load = useCallback(() => {
    if (!r || !clock) return;
    setError(null);
    const qs = new URLSearchParams({ from: r.from, to: r.to });
    if (project) qs.set("project", project);
    api.get<{ items: Item[]; today?: string; utc_offset?: string }>("/items?" + qs).then((res) => {
      setItems(res.items);
      if (res.today && res.utc_offset && (res.today !== clock.today || res.utc_offset !== clock.utc_offset)) setClock({ today: res.today, utc_offset: res.utc_offset });
    }, setError);
    // 过期未办与今天页第一组同一口径（计划早于今天、仍未完成），不受这一段的范围限制。
    api.get<{ planned: Item[] }>("/today").then(
      (td) => setOverdue(td.planned.filter((i) => (i.planned_for ?? "") < clock.today && (!project || i.project === project)).length),
      () => setOverdue(0),
    );
  }, [api, r?.from, r?.to, project, clock]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(load, [load]);
  // 页面重新获得焦点时重读：日历不缓存状态，别处改过就跟着变。
  useEffect(() => {
    const on = () => document.visibilityState !== "hidden" && load();
    window.addEventListener("focus", on);
    document.addEventListener("visibilitychange", on);
    return () => {
      window.removeEventListener("focus", on);
      document.removeEventListener("visibilitychange", on);
    };
  }, [load]);

  const go = useCallback((next: { view?: View; date?: string; project?: string }) => {
    const v = next.view ?? view;
    if (next.view) localStorage.setItem(VIEW_KEY, next.view);
    const p = next.project ?? project;
    setParams({ view: v, date: next.date ?? date, ...(p ? { project: p } : {}) });
  }, [view, date, project, setParams]);

  // 键盘：左右翻段，T 回到今天，D、W、M 切换视图；焦点在输入框里时不生效。
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      if (e.metaKey || e.ctrlKey || e.altKey || !date || (el && (/^(INPUT|SELECT|TEXTAREA)$/.test(el.tagName) || el.isContentEditable))) return;
      const k = e.key.toLowerCase();
      if (e.key === "ArrowLeft") go({ date: step(view, date, -1) });
      else if (e.key === "ArrowRight") go({ date: step(view, date, 1) });
      else if (k === "t" && clock) go({ date: clock.today });
      else if (k === "d") go({ view: "day" });
      else if (k === "w") go({ view: "week" });
      else if (k === "m") go({ view: "month" });
      else return;
      e.preventDefault();
    };
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, [go, view, date, clock]);

  const byDay = clock && items ? entries(items, clock.today, clock.utc_offset, Date.now()) : null;
  const total = byDay && r ? r.days.reduce((n, d) => n + (byDay.get(d)?.length ?? 0), 0) : 0;

  // 回程还原滚动位置；没有可还原的时，时间格滚到 07:00 或最早一条前一小时。
  useLayoutEffect(() => {
    if (!byDay || !r) return;
    const saved = JSON.parse(sessionStorage.getItem(SCROLL_KEY) ?? "null") as { url: string; grid: number; win: number } | null;
    if (saved && saved.url === pathname + search) {
      sessionStorage.removeItem(SCROLL_KEY);
      if (grid.current) grid.current.scrollTop = saved.grid;
      window.scrollTo?.(0, saved.win);
    } else if (grid.current) {
      grid.current.scrollTop = (scrollStart(r.days.flatMap((d) => split(byDay.get(d)).timed)) / 30) * (narrow ? 44 : 24);
    }
  }, [items, view, r?.from]); // eslint-disable-line react-hooks/exhaustive-deps

  const remember = () => sessionStorage.setItem(SCROLL_KEY, JSON.stringify({ url: pathname + search, grid: grid.current?.scrollTop ?? 0, win: window.scrollY }));
  const browserOff = browserClock().utc_offset;
  const ctx: Ctx = { today: clock?.today ?? "", offset: clock?.utc_offset ?? browserOff, overdue, remember, go, narrow, row: narrow ? 44 : 24 };

  return (
    <main className="cal">
      <PageHead>{t.title}</PageHead>
      <div className="cal-bar">
        <div className="seg" role="group" aria-label={t.viewLabel}>
          {VIEWS.map((v) => (
            <button key={v} type="button" aria-pressed={v === view} className={v === view ? "on" : undefined} onClick={() => go({ view: v })}>{t.views[v]}</button>
          ))}
        </div>
        <div className="seg">
          <button type="button" aria-label={t.prev} onClick={() => date && go({ date: step(view, date, -1) })}>‹</button>
          <button type="button" onClick={() => clock && go({ date: clock.today })}>{t.today}</button>
          <button type="button" aria-label={t.next} onClick={() => date && go({ date: step(view, date, 1) })}>›</button>
        </div>
        {r && <strong className="cal-period">{period(t, view, r.days)}</strong>}
        <ProjectPick value={project} onChange={(p) => go({ project: p })} />
        {date && <Go className="btn" to={"/new?" + new URLSearchParams({ planned_for: date, ...(project ? { project } : {}) })}>{t.newItem}</Go>}
      </div>
      {clock && clock.utc_offset !== browserOff && <p className="sub">{t.hostTime(clock.utc_offset)}</p>}
      {error != null && <ErrorNote error={error} />}
      {!byDay || !r ? (
        error == null && <Skeleton label={t.loading} />
      ) : (
        <>
          {total === 0 && <EmptyState title={t.empty} />}
          {view === "day" && <DayView day={r.days[0]} byDay={byDay} grid={grid} ctx={ctx} />}
          {view === "week" && (narrow ? <WeekList days={r.days} byDay={byDay} ctx={ctx} /> : <WeekView days={r.days} byDay={byDay} grid={grid} ctx={ctx} />)}
          {view === "month" && <MonthView days={r.days} month={date.slice(0, 7)} byDay={byDay} ctx={ctx} />}
        </>
      )}
    </main>
  );
}

interface Ctx {
  today: string;
  offset: string;
  overdue: number;
  remember: () => void;
  go: (n: { view?: View; date?: string }) => void;
  narrow: boolean;
  /** 半小时一行的高度（px）：窄屏按 44px 的触控高度。 */
  row: number;
}
type T = (typeof strings)["zh"];
type ByDay = Map<string, Entry[]>;

function period(t: T, view: View, days: string[]): string {
  if (view === "month") {
    const mid = days[15];
    return t.monthTitle(Number(mid.slice(0, 4)), Number(mid.slice(5, 7)));
  }
  return view === "day" ? t.dayTitle(days[0], t.weekdays[(new Date(days[0] + "T00:00:00Z").getUTCDay() + 6) % 7]) : `${days[0]} – ${days[6]}`;
}

function ProjectPick({ value, onChange }: { value: string; onChange: (p: string) => void }) {
  const t = useT(strings);
  const projects = useNamed("project");
  return (
    <label>
      {t.projectFilter}{" "}
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">{t.allProjects}</option>
        {projects.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
      </select>
    </label>
  );
}

/** 一条事项：点开直接进事项详情；悬停或聚焦出一张只读卡，读屏读同样几项。原始 id 只在"显示原始值"时出现。 */
function EntryLink({ e, ctx, children, className, style }: { e: Entry; ctx: Ctx; children: ReactNode; className?: string; style?: CSSProperties }) {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const projects = useNamed("project");
  const it = e.item;
  const pname = it.project ? projects.find((p) => p.id === it.project)?.name ?? "" : "";
  const card = [
    it.title,
    it.planned_for && [t.planOn(it.planned_for), it.planned_slot].filter(Boolean).join(" "),
    pname,
    label("phase", it.phase),
    e.due && t.dueOn(e.due.time ? `${e.due.date} ${e.due.time}` : e.due.date),
    raw && t.rawId(it.id),
  ].filter(Boolean) as string[];
  const cls = ["cal-entry", "s-" + e.status, e.overdue && "is-overdue", e.pastDue && "is-pastdue", className].filter(Boolean).join(" ");
  return (
    <Go to={"/items/" + encodeURIComponent(it.id)} className={cls} style={style} aria-label={card.join(t.listSep)} onClick={ctx.remember}>
      {children}
      <span className="cal-card" aria-hidden="true">{card.map((c, i) => <span key={i}>{c}</span>)}</span>
    </Go>
  );
}

/** 状态标记：在等、等你决定、已完成、过期未办、已过截止、晚于截止、时间重叠；"要做"不标。 */
function Marks({ e, overlap }: { e: Entry; overlap?: boolean }) {
  const t = useT(strings);
  const m: [string, string][] = [];
  if (e.status !== "todo") m.push([e.status, t.status[e.status]]);
  if (e.overdue) m.push(["overdue", t.overdue]);
  if (e.dueHere && e.type === "plan") m.push([e.pastDue ? "pastdue" : "due", e.pastDue ? t.pastDue : e.due?.time ? t.dueAt(e.due.time) : t.due]);
  else if (e.pastDue) m.push(["pastdue", t.pastDue]);
  if (e.lateVsDue) m.push(["late", t.lateVsDue]);
  if (overlap) m.push(["overlap", t.overlap]);
  return <>{m.map(([k, v]) => <span key={k} className={"cal-mark m-" + k}>{v}</span>)}</>;
}

/** 一行：时间（有时段的写开始或起止）、标题、标记；项目名只在日视图写。 */
function Line({ e, ctx, range: showRange, project }: { e: Entry; ctx: Ctx; range?: boolean; project?: boolean }) {
  const t = useT(strings);
  const projects = useNamed("project");
  const time = e.type === "due" ? (e.due?.time ? t.dueAt(e.due.time) : t.due) : e.slot ? (showRange ? slotOf(e.slot[0], e.slot[1]) : hhmm(e.slot[0])) : "";
  const pname = project && e.item.project ? projects.find((p) => p.id === e.item.project)?.name : "";
  return (
    <EntryLink e={e} ctx={ctx}>
      {e.status === "done" && <span aria-hidden="true">✓ </span>}
      {time && <span className={e.type === "due" ? "cal-time cal-due" : "cal-time"}>{time}</span>}
      <span className="cal-title">{e.item.title}</span>
      {pname && <span className="sub">{pname}</span>}
      <Marks e={e} />
    </EntryLink>
  );
}

function OverdueLine({ ctx }: { ctx: Ctx }) {
  const t = useT(strings);
  if (!ctx.overdue) return null;
  return <p className="cal-overdue">{t.overdueLine(ctx.overdue)} · <Link to="/today">{t.toToday}</Link></p>;
}

/** 截止栏或未定时栏：limit 之外写"还有 n 件"，点开进当天的日视图。 */
function Bar({ list, ctx, day, limit, project }: { list: Entry[]; ctx: Ctx; day: string; limit?: number; project?: boolean }) {
  const t = useT(strings);
  const shown = limit ? list.slice(0, limit) : list;
  return (
    <ul className="cal-list">
      {shown.map((e) => <li key={e.type + e.item.id}><Line e={e} ctx={ctx} project={project} /></li>)}
      {list.length > shown.length && <li><button type="button" className="link" onClick={() => ctx.go({ view: "day", date: day })}>{t.more(list.length - shown.length)}</button></li>}
    </ul>
  );
}

function DayView({ day, byDay, grid, ctx }: { day: string; byDay: ByDay; grid: RefObject<HTMLDivElement>; ctx: Ctx }) {
  const t = useT(strings);
  const s = split(byDay.get(day));
  return (
    <section aria-label={day}>
      {day === ctx.today && <OverdueLine ctx={ctx} />}
      {s.due.length > 0 && <div className="cal-row"><span className="cal-head">{t.dueBar}</span><Bar list={s.due} ctx={ctx} day={day} project /></div>}
      {s.untimed.length > 0 && <div className="cal-row"><span className="cal-head">{t.untimedBar}</span><Bar list={s.untimed} ctx={ctx} day={day} project /></div>}
      <TimeGrid days={[day]} byDay={byDay} grid={grid} ctx={ctx} detail />
    </section>
  );
}

function WeekView({ days, byDay, grid, ctx }: { days: string[]; byDay: ByDay; grid: RefObject<HTMLDivElement>; ctx: Ctx }) {
  const t = useT(strings);
  const s = days.map((d) => split(byDay.get(d)));
  return (
    <section aria-label={`${days[0]} – ${days[6]}`}>
      <div className="cal-week cal-week-head">
        <span />
        {days.map((d, i) => (
          <button key={d} type="button" className={"link cal-dayhead" + (d === ctx.today ? " is-today" : "")} onClick={() => ctx.go({ view: "day", date: d })}>
            {t.weekdays[i]} {Number(d.slice(8))}
          </button>
        ))}
      </div>
      {days.includes(ctx.today) && ctx.overdue > 0 && (
        <div className="cal-week"><span />{days.map((d) => <div key={d}>{d === ctx.today && <OverdueLine ctx={ctx} />}</div>)}</div>
      )}
      {s.some((x) => x.due.length) && (
        <div className="cal-week"><span className="cal-head">{t.dueBar}</span>{days.map((d, i) => <div key={d}><Bar list={s[i].due} ctx={ctx} day={d} limit={3} /></div>)}</div>
      )}
      {s.some((x) => x.untimed.length) && (
        <div className="cal-week"><span className="cal-head">{t.untimedBar}</span>{days.map((d, i) => <div key={d}><Bar list={s[i].untimed} ctx={ctx} day={d} limit={3} /></div>)}</div>
      )}
      <TimeGrid days={days} byDay={byDay} grid={grid} ctx={ctx} />
    </section>
  );
}

/** 0–24 点、每行半小时的时间格；点空白格去新建页，预填那天与"该格开始、往后一小时"（最晚 24:00）。 */
function TimeGrid({ days, byDay, grid, ctx, detail }: { days: string[]; byDay: ByDay; grid: RefObject<HTMLDivElement>; ctx: Ctx; detail?: boolean }) {
  const t = useT(strings);
  const nowMin = (() => {
    const p = hostParts(Date.now(), ctx.offset).time;
    return Number(p.slice(0, 2)) * 60 + Number(p.slice(3, 5));
  })();
  const ROW = ctx.row;
  const rows = Array.from({ length: 48 }, (_, i) => i * 30);
  return (
    <div className="cal-grid" ref={grid} style={{ ["--cols" as string]: days.length }}>
      <div className="cal-hours" aria-hidden="true">{rows.map((m) => <span key={m} style={{ height: ROW }}>{m % 60 === 0 ? hhmm(m) : ""}</span>)}</div>
      {days.map((d) => {
        const { blocks, overflow } = lanes(split(byDay.get(d)).timed);
        return (
          <div key={d} className="cal-col" style={{ height: 48 * ROW }}>
            {rows.map((m) => (
              <Go key={m} className="cal-slot" style={{ top: (m / 30) * ROW, height: ROW }} aria-label={t.newAt(d, hhmm(m))}
                to={"/new?" + new URLSearchParams({ planned_for: d, planned_slot: slotOf(m, Math.min(m + 60, 1440)) })} onClick={ctx.remember} />
            ))}
            {blocks.map((b) => {
              const [s, e] = b.entry.slot!;
              const short = e - s <= 30;
              return (
                <EntryLink key={b.entry.item.id} e={b.entry} ctx={ctx} className={"cal-block" + (short ? " short" : "")}
                  style={{ top: (s / 30) * ROW, height: (Math.max(e - s, 30) / 30) * ROW - 2, left: `${(b.col / b.cols) * 100}%`, width: `calc(${100 / b.cols}% - 2px)` }}>
                  {b.entry.status === "done" && <span aria-hidden="true">✓ </span>}
                  <span className="cal-time">{slotOf(s, e)}</span> <span className="cal-title">{b.entry.item.title}</span>
                  {detail && !short && <ProjectName id={b.entry.item.project} />}
                  <Marks e={b.entry} overlap={b.overlap} />
                </EntryLink>
              );
            })}
            {overflow.map((o) => (
              <details key={o.start} className="cal-overflow" style={{ top: (o.start / 30) * ROW, left: "66.6%", width: "33.3%" }}>
                <summary>{t.plus(o.entries.length)}</summary>
                <ul className="cal-list">{[...o.entries].sort((a, b) => a.slot![0] - b.slot![0]).map((e) => <li key={e.item.id}><Line e={e} ctx={ctx} range /></li>)}</ul>
              </details>
            ))}
            {d === ctx.today && <div className="cal-now" style={{ top: (nowMin / 30) * ROW }} aria-label={t.now} />}
          </div>
        );
      })}
    </div>
  );
}

function ProjectName({ id }: { id?: string }) {
  const projects = useNamed("project");
  const n = id ? projects.find((p) => p.id === id)?.name : "";
  return n ? <span className="sub"> {n}</span> : null;
}

/** 窄屏的周：七天的列表，没有安排的一天写"没有安排"。 */
function WeekList({ days, byDay, ctx }: { days: string[]; byDay: ByDay; ctx: Ctx }) {
  const t = useT(strings);
  return (
    <section aria-label={`${days[0]} – ${days[6]}`}>
      {days.map((d, i) => {
        const list = ordered(byDay.get(d));
        return (
          <div key={d} className={"cal-day" + (d === ctx.today ? " is-today" : "")}>
            <h2><button type="button" className="link" onClick={() => ctx.go({ view: "day", date: d })}>{t.dayTitle(d, t.weekdays[i])}</button></h2>
            {d === ctx.today && <OverdueLine ctx={ctx} />}
            {list.length ? <ul className="cal-list">{list.map((e) => <li key={e.type + e.item.id}><Line e={e} ctx={ctx} /></li>)}</ul> : <p className="sub">{t.nothing}</p>}
          </div>
        );
      })}
    </section>
  );
}

/** 月：6×7 格，周一开始；每格最多 3 条，多的写"还有 n 件"；窄屏只给圆点与条数。 */
function MonthView({ days, month, byDay, ctx }: { days: string[]; month: string; byDay: ByDay; ctx: Ctx }) {
  const t = useT(strings);
  return (
    <section aria-label={month}>
      <div className="cal-month cal-month-head" aria-hidden="true">{t.weekdays.map((w) => <span key={w}>{w}</span>)}</div>
      <div className="cal-month">
        {days.map((d) => {
          const list = ordered(byDay.get(d));
          const cls = ["cal-cell", d.slice(0, 7) !== month && "is-other", d === ctx.today && "is-today"].filter(Boolean).join(" ");
          if (ctx.narrow) {
            return (
              <button key={d} type="button" className={cls} aria-label={`${d} ${t.count(list.length)}`} onClick={() => ctx.go({ view: "day", date: d })}>
                <span className="cal-num">{Number(d.slice(8))}</span>
                {list.length > 0 && <span className="cal-dots">{"•".repeat(Math.min(list.length, 3))} {list.length}</span>}
              </button>
            );
          }
          return (
            <div key={d} className={cls}>
              <div className="cal-cellhead">
                <button type="button" className="link cal-num" onClick={() => ctx.go({ view: "day", date: d })}>{Number(d.slice(8))}</button>
                <Go className="cal-add" to={"/new?planned_for=" + d} aria-label={t.newOn(d)} onClick={ctx.remember}>+</Go>
              </div>
              {d === ctx.today && <OverdueLine ctx={ctx} />}
              <Bar list={list} ctx={ctx} day={d} limit={3} />
            </div>
          );
        })}
      </div>
    </section>
  );
}
