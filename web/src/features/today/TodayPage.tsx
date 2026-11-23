import { useFaultSay, type Fault } from "../../app/notice";
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { Go, PageHead } from "../../app/page";
import { refreshCounts, useAttention, type Reminder } from "../../app/attention";
import type { Item } from "../items/ItemsPage";
import { ProgressForm } from "./ProgressForm";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useItemTitle, useNamed } from "../../app/names";
import { loadDone } from "../done/DonePage";
import { monday } from "../calendar/layout";
import { hostClock } from "../items/schedule";
import { RowActions } from "../items/RowActions";
import { Val } from "../../app/values";
import { strings } from "./strings";

// 今天（界面设计 §3.2、§5.2）：自上而下五块，空的不显示——要你处理、Agent 交回的、今天的事、提醒、久未推进（默认折叠）。
// 「要你处理」与导航角标、状态灯取同一份注意力汇总；今天的事取自 GET /v1/today（附录 B.2），操作交给既有事项接口。
// 今天的事每行的快捷操作对齐工作台「今日焦点」：排时间（日期与时段）、交给 Agent、完成，做完原位反映。
interface Today { planned: Item[]; due: Item[]; pending: Item[]; stale: Item[] }
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
  const [doneN, setDoneN] = useState<{ today: number; week: number } | null>(null);

  const load = useCallback(
    () => api.get<Today>("/today").then((r) => setData(r), (e) => setError(e)),
    [api],
  );
  useEffect(() => { load(); }, [load]);
  // 底部一行完成计数：做完的事从上面消失后仍看得到进度；明细只在「已完成」页，这里不重复列。取不到就不显示。
  const countDone = useCallback(() => hostClock(api).then((c) => loadDone(api, c.utc_offset).then((ds) =>
    setDoneN({ today: ds.filter((d) => d.day === c.today).length, week: ds.filter((d) => d.day >= monday(c.today)).length }))).catch(() => setDoneN(null)), [api]);
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

  // 行上操作之后：只把这一件换成服务端的最新值，列表、顺序与滚动位置不动；角标与完成计数另取。
  const replace = (it: Item) => {
    setData((d) => d && { ...d, planned: d.planned.map((x) => (x.id === it.id ? it : x)), due: d.due.map((x) => (x.id === it.id ? it : x)) });
    refreshCounts();
    countDone();
  };

  const a = attention;
  const sys = a?.status?.act.length ?? 0;
  const top = a ? [
    a.decide > 0 && { key: "decide", text: t.top.decide(a.decide), button: t.top.toDecide, to: "/decisions", red: true },
    sys > 0 && { key: "system", text: t.top.system(sys), button: t.top.toSystem, to: "/health", red: false },
    a.proposals > 0 && { key: "proposals", text: t.top.proposals(a.proposals), button: t.top.toProposals, to: "/proposals", red: false },
  ].filter((x): x is { key: string; text: string; button: string; to: string; red: boolean } => !!x) : [];
  // 计划今天或更早与今天到期合并成「今天的事」，同一件只列一次。
  const todays = data ? [...data.planned, ...data.due.filter((d) => !data.planned.some((p) => p.id === d.id))] : [];
  const reminders = a?.reminders ?? [];
  const empty = !!data && !top.length && !back.length && !todays.length && !reminders.length && !data.stale.length;

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} />}
      {top.length > 0 && (
        <section aria-label={t.blocks.top} className="panel">
          <h2>{t.blocks.top}</h2>
          <ul className="rows">
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
          <ul className="rows">{back.map((r) => <BackRow key={r.id} run={r} />)}</ul>
        </section>
      )}
      {!data ? (
        error == null && <p role="status">{t.loading}</p>
      ) : (
        <>
          {todays.length > 0 && (
            <section aria-label={t.blocks.today}>
              <h2>{t.count(t.blocks.today, todays.length)}</h2>
              <ul className="rows">{todays.map((it) => <TodayRow key={it.id} item={it} onChange={replace} />)}</ul>
            </section>
          )}
          {reminders.length > 0 && <Reminders list={reminders} />}
          {data.stale.length > 0 && <Stale items={data.stale} act={act} />}
          {empty && <p className="empty">{t.allClear}</p>}
          {doneN && (
            <p className="sub">
              {doneN.week ? t.doneLine(doneN.today, doneN.week) : t.doneNone} · <Link to="/done">{t.seeDone}</Link>
            </p>
          )}
        </>
      )}
    </main>
  );
}

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
        <span className="sub">{[ok ? t.backDone : fault ? t.backFault(fault.say) : t.backNotDone, when].filter(Boolean).join(" · ")}</span>
        {!ok && <span className="sub">{t.backNext}</span>}
      </div>
      <Go className="btn" to={"/items/" + encodeURIComponent(run.target) + "#agent"}>{ok ? t.seeResult : fault ? t.seeWhy : t.answer}</Go>
    </li>
  );
}

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

/**
 * 今天的一件事：一行标题，行上直接是「排时间、交给 Agent、完成」（RowActions，与事项列表共用）；写进展收在折叠里。
 * 做完、改期之后这一行留在原处显示新状态（已完成、新的计划），不重读整页；下次打开今天页按服务端重新分组。
 */
function TodayRow({ item, onChange }: { item: Item; onChange: (it: Item) => void }) {
  const { api } = useSession();
  const t = useT(strings);
  const [err, setErr] = useState<unknown>(null);
  const progress = async (b: { text: string }) => {
    setErr(null);
    try { await api.post(itemPath(item) + "/progress", b); } catch (e) { setErr(e); }
    await api.get<{ item: Item }>(itemPath(item)).then((r) => onChange(r.item), () => undefined);
  };
  const ended = item.phase === "done" || item.phase === "dropped";
  return (
    <li className="stack" data-item={item.id}>
      <div className="act-row">
        <div><ItemLink item={item} />{ended && <> <span className="badge plain"><Val k="phase" id={item.phase} /></span></>}</div>
        <RowActions item={item} where={t.where} onChange={onChange} />
      </div>
      {err != null && <ErrorNote error={err} />}
      {!ended && (
        <details>
          <summary>{t.moreFor}</summary>
          <ProgressForm onSubmit={progress} />
        </details>
      )}
    </li>
  );
}

/** 提醒：每条一行（标题、类别、被压下的原因），可打开、可「知道了」。只有本人点击才发 read；失败后以服务端为准。 */
function Reminders({ list }: { list: Reminder[] }) {
  const { api } = useSession();
  const t = useT(strings);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const read = async (id: string) => {
    setBusy(id);
    setError(null);
    try {
      await api.post("/deliveries/" + encodeURIComponent(id) + "/read");
    } catch (e) {
      setError(e);
    }
    setBusy(null);
    refreshCounts();
  };
  return (
    <section aria-label={t.blocks.reminders} id="reminders">
      <h2>{t.count(t.blocks.reminders, list.length)}</h2>
      {error != null && <ErrorNote error={error} />}
      <ul className="rows">
        {list.map((d) => (
          <li key={d.id} className="act-row" aria-label={d.content.title}>
            <div>
              <span>{d.content.title}</span>
              {d.content.summary && <span className="sub">{d.content.summary}</span>}
              <span className="sub">
                <Val k="deliveryClass" id={d.class} />
                {d.suppressed && <> · <Val k="suppressed" id={d.suppressed} /></>}
                {d.injections && d.injections.length > 0 && <> · {t.injected(d.injections.length)}</>}
              </span>
            </div>
            <span className="actions">
              {d.content.web_url ? <a className="btn" href={d.content.web_url}>{t.open}</a> : d.item && <Link className="btn" to={"/items/" + encodeURIComponent(d.item)}>{t.open}</Link>}
              <button type="button" disabled={busy === d.id} onClick={() => void read(d.id)}>{t.gotIt}</button>
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
    <details aria-label={t.groups.stale}>
      <summary>{t.count(t.groups.stale, items.length)}</summary>
      <ul className="rows">
        {items.map((it) => (
          <li key={it.id}>
            <label><input type="checkbox" checked={picked.has(it.id)} onChange={() => toggle(it.id)} aria-label={t.select(it.title)} /> </label>
            <ItemLink item={it} />
          </li>
        ))}
      </ul>
      <div className="panel actions">
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
    </details>
  );
}
