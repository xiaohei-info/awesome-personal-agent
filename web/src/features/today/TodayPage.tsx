import { useFaultSay, type Fault } from "../../app/notice";
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useSession } from "../../app/session";
import { Go, PageHead } from "../../app/page";
import { refreshCounts, useAttention, type Reminder } from "../../app/attention";
import type { Item } from "../items/ItemsPage";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useItemTitle, useNamed } from "../../app/names";
import { loadDone, loadProgress } from "../done/DonePage";
import { monday } from "../calendar/layout";
import { hostClock, hostParts, useHostClock } from "../items/schedule";
import { DoneCircle, RowActions, type RowAct } from "../items/RowActions";
import { Val, useLabel } from "../../app/values";
import { ItemDetail } from "../items/ItemsPage";
import { Disclosure, EmptyState, ErrorBoundary, Icon, LIST_KEYS, PeekPanel, Skeleton, collapse, rowEl, toast, toastError, undoToast, useListKeys, usePageKeys, usePeek } from "../../app/ui";
import { strings } from "./strings";

// 今天（界面设计 §3.2、§5.2）：自上而下五块，空的不显示——要你处理、Agent 交回的、今天的事、提醒、久未推进（默认折叠）。
// 「要你处理」与导航角标、状态灯取同一份注意力汇总；今天的事取自 GET /v1/today（附录 B.2），操作交给既有事项接口。
// 今天的事对齐工作台「今日焦点」：计划今天或更早、今天到期、已逾期、高优先级四类合在一起，按项目分组，每行标出它为什么在这；
// 行上四个快捷操作：排时间（日期与时段）、交给 Agent、完成、记进展。
// 交互（交互样式规格 §3.1）：完成、放弃、改排期之后弹可撤销的提示，撤销就是用现有接口改回去（拍板项 3：立即写入、撤销时改回）；
// 离开今天的那一行先收起再移除。J/K 移动当前行，Enter 打开，空格或 Shift+点击标题在右侧速览，E 完成，Z 撤销。
interface Today { planned: Item[]; due: Item[]; pending: Item[]; stale: Item[]; overdue?: Item[]; high_priority?: Item[] }
type Why = "planned" | "due" | "overdue" | "high";
export interface Entry { item: Item; why: Why[] }

/** 「今天的事」：四组合并，同一件只列一次、记下它在哪几组（服务端的组，前端不重算口径）。 */
export function todayEntries(d: Today): Entry[] {
  const out = new Map<string, Entry>();
  const put = (list: Item[] | undefined, why: Why) => {
    for (const it of list ?? []) {
      const e = out.get(it.id);
      if (e) e.why.push(why);
      else out.set(it.id, { item: it, why: [why] });
    }
  };
  put(d.planned, "planned");
  put(d.due, "due");
  put(d.overdue, "overdue");
  put(d.high_priority, "high");
  return [...out.values()];
}

/** 按项目分组：组按项目列表的顺序（与事项列表一致），没归项目的排最后；组内保持服务端的先后。 */
export function byProject(entries: Entry[], order: string[]): [string, Entry[]][] {
  const groups = new Map<string, Entry[]>();
  for (const e of entries) groups.set(e.item.project ?? "", [...(groups.get(e.item.project ?? "") ?? []), e]);
  const rank = (p: string) => (!p ? order.length + 1 : order.includes(p) ? order.indexOf(p) : order.length);
  return [...groups].sort(([a], [b]) => rank(a) - rank(b));
}
interface Run { id: string; target: string; category: string; state: string; finished_at?: string; fault?: Fault }

const DATE = /^\d{4}-\d{2}-\d{2}$/;
const SEEN = "apa.today.seen"; // 上次打开今天页的时刻：只存这台浏览器，与改动页「自上次访问以来」同一口径

export function TodayPage() {
  const { api } = useSession();
  const t = useT(strings);
  const attention = useAttention();
  const [data, setData] = useState<Today | null>(null);
  const [back, setBack] = useState<Run[]>([]);
  const [error, setError] = useState<unknown>(null);
  const [since] = useState(() => localStorage.getItem(SEEN) ?? "");
  const [doneN, setDoneN] = useState<{ today: number; week: number; progress: number | null } | null>(null);
  const [entering, setEntering] = useState<string | null>(null);
  const projects = useNamed("project");
  const label = useLabel();
  const navigate = useNavigate();
  const peek = usePeek();
  usePageKeys(LIST_KEYS);

  const load = useCallback(
    () => api.get<Today>("/today").then((r) => setData(r), (e) => setError(e)),
    [api],
  );
  useEffect(() => { load(); }, [load]);
  // 底部一行完成与进展计数：做完的事从上面消失后仍看得到进度；进展条数与「已完成」页同一口径（同事同日算一条）。
  // 明细只在「已完成」页，这里不重复列。取不到就不显示。
  const countDone = useCallback(() => hostClock(api).then(async (c) => {
    const [ds, ps] = await Promise.all([loadDone(api, c.utc_offset), loadProgress(api, monday(c.today), c.today, c.utc_offset).catch(() => null)]);
    setDoneN({ today: ds.filter((d) => d.day === c.today).length, week: ds.filter((d) => d.day >= monday(c.today)).length, progress: ps && ps.reduce((n, p) => n + p.records.length, 0) });
  }).catch(() => setDoneN(null)), [api]);
  useEffect(() => { countDone(); }, [countDone]);
  useEffect(() => {
    localStorage.setItem(SEEN, new Date().toISOString());
    // Agent 交回的：上次打开今天页之后结束的 Agent 运行，按事项各取最近一次。取不到就不显示这一块。
    // 只留本人要看或要动手的：模板型（简报、例行任务）成功了结果就在简报页，不占一行；失败了、事项已关掉的也不再列（本人已处理）。
    api.get<{ runs: Run[] }>("/runs?category=background,task&state=succeeded,failed,timeout&limit=100").then(async (r) => {
      const latest = new Map<string, Run>();
      for (const x of r.runs) if (x.finished_at && (!since || x.finished_at > since)) latest.set(x.target, x);
      const runs = [...latest.values()].reverse();
      const keep = await Promise.all(runs.map((x) => api.get<{ item: Item }>("/items/" + encodeURIComponent(x.target)).then(
        (r) => worthShowing(x, r.item), () => x.state !== "succeeded")));
      setBack(runs.filter((_, i) => keep[i]));
    }, () => setBack([]));
  }, [api, since]);

  // 久未推进的批量处理写完整页重读（它会让事项离开这一组）。
  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try { await fn(); } catch (e) { setError(e); }
    await load();
    refreshCounts();
    countDone();
  };

  // 行上操作之后：先把这一件换成服务端的最新值，再静默重取一次 GET /today，按新的分组与先后就地重排（不显示加载、不卸载页面，滚动位置不动）。
  // 归不归今天由服务端定，前端不复刻口径；离开了今天的那一行先收起再移除，焦点落到下一行。
  // 完成、放弃、改排期弹可撤销的提示（判定看服务端返回的这一件，写入失败时不弹）。
  const replace = async (it: Item, what: RowAct, before?: Item) => {
    const swap = (l?: Item[]) => l?.map((x) => (x.id === it.id ? it : x));
    setData((d) => d && { ...d, planned: swap(d.planned)!, due: swap(d.due)!, overdue: swap(d.overdue), high_priority: swap(d.high_priority) });
    if (before) offerUndo(before, it, what);
    const r = await api.get<Today>("/today").catch(() => null);
    refreshCounts();
    countDone();
    if (!r) return;
    if (!todayEntries(r).some((e) => e.item.id === it.id)) {
      const ids = order.current;
      const next = ids[ids.indexOf(it.id) + 1] ?? ids[ids.indexOf(it.id) - 1];
      await collapse(rowEl(it.id));
      if (peek.id === it.id) peek.close();
      const to = next && rowEl(next)?.querySelector<HTMLElement>("a");
      if (to && document.activeElement === document.body) to.focus({ preventScroll: true });
    }
    setData(r);
  };

  const undoWith = async (fn: () => Promise<unknown>, done: string, id: string) => {
    try {
      await fn();
      setEntering(id);
      toast(done);
    } catch (e) {
      toastError(t.undoFailed(msg(e)));
    }
    const r = await api.get<Today>("/today").catch(() => null);
    if (r) setData(r);
    refreshCounts();
    countDone();
  };

  /** 撤销提示：完成与放弃改回原来的阶段（等待中的带回原来在等的对象），改排期写回原来的计划日期与时段。 */
  const offerUndo = (before: Item, after: Item, what: RowAct) => {
    const path = itemPath(after);
    if ((what === "done" && after.phase === "done") || (what === "drop" && after.phase === "dropped")) {
      const back = { to: before.phase, reason: t.undoReason, ...(before.phase === "waiting" && before.waiting_on && { waiting_on: before.waiting_on }) };
      const text = what === "done" ? t.toastDone(after.title) : after.kind === "delegation" ? t.toastDropDelegation(after.title) : t.toastDrop(after.title);
      undoToast(text, () => undoWith(() => api.post(path + "/transition", back, ifMatch(after)), t.undone(label("phase", before.phase)), after.id));
    } else if (what === "plan" && (after.planned_for !== before.planned_for || after.planned_slot !== before.planned_slot)) {
      const when = [after.planned_for && md(after.planned_for), after.planned_slot].filter(Boolean).join(" ");
      undoToast(t.toastPlan(when), () => undoWith(() => api.patch(path, { planned_for: before.planned_for ?? null, planned_slot: before.planned_slot ?? null, reason: t.undoReason }, ifMatch(after)), t.undonePlan, after.id));
    }
  };

  // 键盘「完成」：与行上的「完成」同一个接口，写完重读这一件再交给 replace。
  const doneById = async (id: string) => {
    const it = todayEntries(data ?? { planned: [], due: [], pending: [], stale: [] }).find((e) => e.item.id === id)?.item;
    if (!it || it.kind === "delegation" || ["proposed", "done", "dropped"].includes(it.phase)) return;
    try {
      await api.post(itemPath(it) + "/transition", { to: "done", reason: t.doneReason }, ifMatch(it));
    } catch (e) {
      toastError(e);
    }
    const r = await api.get<{ item: Item }>(itemPath(it)).catch(() => null);
    if (r) void replace(r.item, "done", it);
  };

  const a = attention;
  const sys = a?.status?.act.length ?? 0;
  const top = a ? [
    a.decide > 0 && { key: "decide", text: t.top.decide(a.decide), button: t.top.toDecide, to: "/decisions", red: true },
    sys > 0 && { key: "system", text: t.top.system(sys), button: t.top.toSystem, to: "/health", red: false },
    a.proposals > 0 && { key: "proposals", text: t.top.proposals(a.proposals), button: t.top.toProposals, to: "/proposals", red: false },
  ].filter((x): x is { key: string; text: string; button: string; to: string; red: boolean } => !!x) : [];
  const todays = data ? todayEntries(data) : [];
  const groups = byProject(todays, projects.map((p) => p.id));
  const order = useRef<string[]>([]);
  order.current = groups.flatMap(([, es]) => es.map((e) => e.item.id));
  const cursor = useListKeys({
    ids: order.current,
    onOpen: (id) => navigate(itemPath({ id } as Item)),
    onPeek: (id) => (peek.enabled ? (peek.id === id ? peek.close() : peek.open(id)) : navigate(itemPath({ id } as Item))),
    onDone: (id) => void doneById(id),
    onMove: (id) => peek.id && peek.open(id),
    onEscape: () => !!peek.id && (peek.close(), true),
  });
  const peeked = peek.id ? todays.find((e) => e.item.id === peek.id)?.item : undefined;
  const reminders = a?.reminders ?? [];
  const empty = !!data && !top.length && !back.length && !todays.length && !reminders.length && !data.stale.length;

  return (
    <main className="wide today">
      <PageHead compact>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} reveal />}
      {top.length > 0 && (
        <section aria-label={t.blocks.top}>
          <h2>{t.blocks.top}</h2>
          <ul className="rows flat">
            {top.map((x) => (
              <li key={x.key} className="act-row">
                <div><span>{x.red ? <strong>{x.text}</strong> : x.text}</span></div>
                <Link className="btn" to={x.to}>{x.button}</Link>
              </li>
            ))}
          </ul>
        </section>
      )}
      {back.length > 0 && (
        <section aria-label={t.blocks.back}>
          <h2>{t.blocks.back}</h2>
          <ErrorBoundary><ul className="rows flat">{back.map((r) => <BackRow key={r.id} run={r} />)}</ul></ErrorBoundary>
        </section>
      )}
      {!data ? (
        error == null && <Skeleton label={t.loading} />
      ) : (
        <>
          {todays.length > 0 && (
            <section aria-label={t.blocks.today}>
              <h2>{t.count(t.blocks.today, todays.length)}</h2>
              {groups.map(([pid, es]) => {
                const name = pid ? projects.find((p) => p.id === pid)?.name ?? "" : t.noProject;
                // 每个项目组各自一道错误边界：一组渲染出错只换掉这一组。
                return (
                  <ErrorBoundary key={pid}>
                  <section aria-label={name} className="today-group">
                    <Disclosure head defaultOpen persistKey={"today.group." + (pid || "-")}
                      summary={<h3>{pid ? <Link to={"/projects/" + encodeURIComponent(pid)}>{name}</Link> : name}{" "}<span className="count-n">{es.length}</span></h3>}>
                      <ul className="rows flat">
                        {es.map((e) => (
                          <TodayRow key={e.item.id} entry={e} cursor={cursor === e.item.id} peeked={peek.id === e.item.id} entering={entering === e.item.id}
                            onPeek={peek.enabled ? () => peek.open(e.item.id) : undefined} onChange={(it, what) => void replace(it, what, e.item)} onDone={() => void doneById(e.item.id)} />
                        ))}
                      </ul>
                    </Disclosure>
                  </section>
                  </ErrorBoundary>
                );
              })}
            </section>
          )}
          {reminders.length > 0 && <ErrorBoundary><Reminders list={reminders} /></ErrorBoundary>}
          {data.stale.length > 0 && <ErrorBoundary><Stale items={data.stale} act={act} /></ErrorBoundary>}
          {empty && <EmptyState title={t.allClear} note={t.allClearNote} action={<Link className="btn" to="/items">{t.seeItems}</Link>} />}
          {doneN && (
            <p className="sub">
              {doneN.week ? t.doneLine(doneN.today, doneN.week) : t.doneNone} · {doneN.progress != null && <>{t.progressLine(doneN.progress)} · </>}<Link to="/done">{t.seeDone}</Link>
            </p>
          )}
        </>
      )}
      {peek.id && (
        <PeekPanel key="peek" id={peek.id} ids={order.current} href={itemPath({ id: peek.id } as Item)} title={peeked?.title ?? ""} onOpen={peek.open} onClose={peek.close}>
          <ItemDetail key={peek.id} embedded={peek.id} />
        </PeekPanel>
      )}
    </main>
  );
}

/** 提醒正文里的 Markdown 链接只留文字：这一行有自己的"打开"。 */
const plainLinks = (s: string) => s.replace(/\[([^[\]]*)\]\((?:https?:\/\/|\/)[^()\s]*\)/g, "$1");

/** 模板型运行的事项：成功就是结果本身（在简报页），只有没出来时才要本人知道。 */
const TEMPLATE_KINDS = new Set(["brief", "routine"]);

/** 「Agent 交回的」只列本人需要知道或要动手的运行。 */
export function worthShowing(run: Run, item: Pick<Item, "kind" | "phase">): boolean {
  if (run.state === "succeeded") return !TEMPLATE_KINDS.has(item.kind);
  return item.phase !== "done" && item.phase !== "dropped";
}

/** Agent 交回的一件事：谁做的不重复写（事项页里有），只说做完没有、多久前，下一步是看结果或去回答。 */
function BackRow({ run }: { run: Run }) {
  const t = useT(strings);
  const title = useItemTitle(run.target);
  const ok = run.state === "succeeded";
  const fault = useFaultSay()(run.fault); // 运行故障：说清没跑起来的原因与结果没落账，不叫人"去回答"——那里没有问题
  const when = run.finished_at ? new Date(run.finished_at).toLocaleString(locale(), { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "";
  return (
    <li className="act-row">
      <div>
        <span>{title}</span>
        <span className="sub">{[ok ? t.backDone : fault ? (run.fault?.code === "timeout" ? t.backTimeout(fault.say) : t.backFault(fault.say)) : t.backNotDone, when].filter(Boolean).join(" · ")}</span>
        {!ok && <span className="sub">{t.backNext}</span>}
      </div>
      <Go className="btn" to={"/items/" + encodeURIComponent(run.target) + "#agent"}>{ok ? t.seeResult : fault ? t.seeWhy : t.answer}</Go>
    </li>
  );
}

/** 久未推进的一行：标题、项目与计划、截止。 */
function ItemLink({ item }: { item: Item }) {
  const t = useT(strings);
  const pname = useNamed("project").find((p) => p.id === item.project)?.name;
  return (
    <span>
      {item.planned_slot && <span className="badge plain">{item.planned_slot}</span>} <Link to={"/items/" + encodeURIComponent(item.id)}>{item.title}</Link>{" "}
      <span className="sub">
        {pname && <><Link to={"/projects/" + encodeURIComponent(item.project!)}>{pname}</Link> · </>}
        {[item.planned_for && t.planned(item.planned_for), item.deadline && t.deadline(new Date(item.deadline).toLocaleDateString(locale()))].filter(Boolean).join(" · ")}
      </span>
    </span>
  );
}

const itemPath = (it: Item) => "/items/" + encodeURIComponent(it.id);
const ifMatch = (it: Item) => ({ "If-Match": String(it.version) });
const md = (d: string) => d.slice(5);

/**
 * 今天的一件事，一行（Multica 式扁平行）：行首状态圆圈（点一下完成，带撤销提示）、优先级小图标、标题；
 * 为什么在这（逾期、今天截止、计划哪天）与时段靠右成一列次要字；行上其余操作悬停才出现（RowActions compact）。项目名在组标题上，这里不重复。
 */
function TodayRow({ entry, onChange, onDone, cursor, peeked, entering, onPeek }: {
  entry: Entry;
  onChange: (it: Item, what: RowAct) => void;
  onDone: () => void;
  cursor?: boolean;
  peeked?: boolean;
  entering?: boolean;
  /** 宽屏才有：Shift+点击标题在右侧速览，普通点击照旧整页打开（拍板项 1）。 */
  onPeek?: () => void;
}) {
  const t = useT(strings);
  const { api } = useSession();
  const { utc_offset: offset, today } = useHostClock(api);
  const { item, why } = entry;
  const due = item.deadline ? hostParts(item.deadline, offset) : null;
  const marks: { cls: string; text: string }[] = [];
  if (why.includes("overdue") && due) marks.push({ cls: "badge bad", text: t.why.overdue(md(due.date)) });
  if (why.includes("due") && due) marks.push({ cls: "badge warn", text: due.time === "23:59:59" ? t.why.dueToday : t.why.dueAt(due.time.slice(0, 5)) });
  if (why.includes("planned") && item.planned_for) marks.push({ cls: "badge plain", text: item.planned_for < today ? t.why.plannedPast(md(item.planned_for)) : t.why.plannedToday });
  const prio = item.priority === "urgent" || item.priority === "high" ? item.priority : null;
  return (
    <li data-item={item.id} data-cursor={cursor || undefined} data-peeked={peeked || undefined} className={["trow", entering && "row-enter"].filter(Boolean).join(" ")}>
      <div className="act-row trow-line">
        <DoneCircle item={item} onDone={onDone} />
        {prio && (
          <span className="prio" data-prio={prio}>
            {prio === "urgent" ? <Icon.PrioUrgent /> : <Icon.PrioHigh />}
            <span className="sr-only">{prio === "urgent" ? t.why.urgent : t.why.high}</span>
          </span>
        )}
        <Link className="trow-title" to={itemPath(item)} onClick={(e) => { if (e.shiftKey && onPeek) { e.preventDefault(); onPeek(); } }}>{item.title}</Link>
        <span className="sub reasons">
          {item.planned_slot && <span className="slot">{item.planned_slot}</span>}
          {marks.map((m) => <span key={m.text} className={m.cls}>{m.text}</span>)}
        </span>
        <RowActions compact item={item} where={t.where} onChange={onChange} />
      </div>
    </li>
  );
}

/** 提醒：每条一行（标题、类别、被压下的原因），可打开、可「知道了」。只有本人点击才发 read；失败后以服务端为准。 */
function Reminders({ list }: { list: Reminder[] }) {
  const { api } = useSession();
  const t = useT(strings);
  const [gone, setGone] = useState<Set<string>>(new Set());
  const [error, setError] = useState<unknown>(null);
  // 「知道了」当场收起这一行；失败就回来并就地报错（以服务端为准）。
  const read = async (id: string) => {
    setError(null);
    await collapse(document.querySelector<HTMLElement>(`#reminders li[data-delivery="${CSS.escape(id)}"]`));
    setGone((g) => new Set(g).add(id));
    try {
      await api.post("/deliveries/" + encodeURIComponent(id) + "/read");
    } catch (e) {
      setError(e);
      setGone((g) => { const n = new Set(g); n.delete(id); return n; });
    }
    refreshCounts();
  };
  const shown = list.filter((d) => !gone.has(d.id));
  if (!shown.length && error == null) return null;
  return (
    <section aria-label={t.blocks.reminders} id="reminders">
      <h2>{t.count(t.blocks.reminders, shown.length)}</h2>
      {error != null && <ErrorNote error={error} reveal />}
      <ul className="rows flat">
        {shown.map((d) => (
          <li key={d.id} className="act-row" aria-label={d.content.title} data-delivery={d.id}>
            <div>
              <span>{d.content.title}</span>
              {d.content.summary && <span className="sub">{plainLinks(d.content.summary)}</span>}
              <span className="sub">
                <Val k="deliveryClass" id={d.class} />
                {d.suppressed && <> · <Val k="suppressed" id={d.suppressed} /></>}
                {d.injections && d.injections.length > 0 && <> · {t.injected(d.injections.length)}</>}
              </span>
            </div>
            <span className="actions">
              {d.content.web_url ? <a className="btn" href={d.content.web_url}>{t.open}</a> : d.item && <Link className="btn" to={"/items/" + encodeURIComponent(d.item)}>{t.open}</Link>}
              <button type="button" onClick={() => void read(d.id)}>{t.gotIt}</button>
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}

type StaleChoice = "continue" | "reschedule" | "drop";

// 批量处理没有专用接口：逐条调既有接口（继续=记一条进展让时钟重新计时；改期=设计划日期；放弃=转 dropped），
// 每条独立成败，失败的留在列表里并报原因。默认折叠：它不卡住任何事。
function Stale({ items, act }: { items: Item[]; act: (fn: () => Promise<unknown>) => Promise<void> }) {
  const { api } = useSession();
  const t = useT(strings);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [choice, setChoice] = useState<StaleChoice>("continue");
  const [date, setDate] = useState("");
  const toggle = (id: string) => setPicked((s) => { const n = new Set(s); n.has(id) ? n.delete(id) : n.add(id); return n; });
  const ok = picked.size > 0 && (choice !== "reschedule" || DATE.test(date));

  const apply = () =>
    act(async () => {
      const failed: string[] = [];
      for (const it of items.filter((i) => picked.has(i.id))) {
        try {
          if (choice === "continue") await api.post(itemPath(it) + "/progress", { text: t.continueText });
          else if (choice === "reschedule") await api.patch(itemPath(it), { planned_for: date }, ifMatch(it));
          else await api.post(itemPath(it) + "/transition", { to: "dropped", reason: t.dropReason }, ifMatch(it));
        } catch (e) { failed.push(t.failed(it.title, msg(e))); }
      }
      setPicked(new Set());
      if (failed.length) throw new Error(failed.join(t.sep));
    });

  return (
    <Disclosure aria-label={t.groups.stale} persistKey="today.stale" summary={<h2>{t.count(t.groups.stale, items.length)}</h2>}>
      <ul className="rows flat">
        {items.map((it) => (
          <li key={it.id}>
            <label><input type="checkbox" checked={picked.has(it.id)} onChange={() => toggle(it.id)} aria-label={t.select(it.title)} /> </label>
            <ItemLink item={it} />
          </li>
        ))}
      </ul>
      <div className="actions stale-bar">
        <label>{t.how}
          <select value={choice} onChange={(e) => setChoice(e.target.value as StaleChoice)}>
            <option value="continue">{t.choices.continue}</option>
            <option value="reschedule">{t.choices.reschedule}</option>
            <option value="drop">{t.choices.drop}</option>
          </select>
        </label>
        {choice === "reschedule" && <label>{t.newDate} <input type="date" aria-label={t.batchDate} value={date} onChange={(e) => setDate(e.target.value)} /></label>}
        <button type="button" disabled={!ok} onClick={apply}>{t.apply(picked.size)}</button>
      </div>
    </Disclosure>
  );
}
