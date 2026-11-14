import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import type { Item } from "../items/ItemsPage";
import { ProgressForm } from "./ProgressForm";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { strings } from "./strings";

// 形状对应 GET /v1/today（附录 B.2）：四组取材全在服务端，本页只展示并把操作交给既有事项接口。
interface Today { planned: Item[]; due: Item[]; pending: Item[]; stale: Item[] }
type Group = keyof Today;

const DATE = /^\d{4}-\d{2}-\d{2}$/;
const PENDING_SHOWN = 5; // 待决只列最紧的几条，其余去决策待办

export function TodayPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [data, setData] = useState<Today | null>(null);
  const [error, setError] = useState<unknown>(null);

  const load = useCallback(
    () => api.get<Today>("/today").then((r) => setData(r), (e) => setError(e)),
    [api],
  );
  useEffect(() => { load(); }, [load]);

  // 写入后整页重读：分组永远以服务端为准，失败时不留下本地假状态。
  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try { await fn(); } catch (e) { setError(e); }
    await load();
  };

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} />}
      {!data ? (
        error == null && <p role="status">{t.loading}</p>
      ) : (
        <>
          <Section group="planned" items={data.planned} render={(it) => <PlannedRow key={it.id} item={it} act={act} />} />
          <Section group="due" items={data.due} render={(it) => <li key={it.id}><ItemLink item={it} /></li>} />
          <section aria-label={t.groups.pending}>
            <h2>{t.count(t.groups.pending, data.pending.length)}</h2>
            {data.pending.length === 0 ? <p className="sub">{t.empty}</p> : (
              <ul className="rows">
                {data.pending.slice(0, PENDING_SHOWN).map((it) => <li key={it.id}><ItemLink item={it} /></li>)}
              </ul>
            )}
            {data.pending.length > 0 && <p><Link to="/decisions">{t.toDecisions}</Link></p>}
          </section>
          <Stale items={data.stale} act={act} />
        </>
      )}
    </main>
  );
}

function Section({ group, items, render }: { group: Group; items: Item[]; render: (it: Item) => JSX.Element }) {
  const t = useT(strings);
  return (
    <section aria-label={t.groups[group]}>
      <h2>{t.count(t.groups[group], items.length)}</h2>
      {items.length === 0 ? <p className="sub">{t.empty}</p> : <ul className="rows">{items.map(render)}</ul>}
    </section>
  );
}

function ItemLink({ item }: { item: Item }) {
  const t = useT(strings);
  return (
    <span>
      <Link to={"/items/" + encodeURIComponent(item.id)}>{item.title}</Link>{" "}
      <span className="sub">{[item.planned_for && t.planned(item.planned_for), item.deadline && t.deadline(new Date(item.deadline).toLocaleDateString(locale()))].filter(Boolean).join(" · ")}</span>
    </span>
  );
}

const itemPath = (it: Item) => "/items/" + encodeURIComponent(it.id);
const ifMatch = (it: Item) => ({ "If-Match": String(it.version) });

function PlannedRow({ item, act }: { item: Item; act: (fn: () => Promise<unknown>) => Promise<void> }) {
  const { api } = useSession();
  const t = useT(strings);
  const [date, setDate] = useState("");
  return (
    <li className="panel stack" data-item={item.id}>
      <ItemLink item={item} />
      <form className="actions" onSubmit={(e) => { e.preventDefault(); if (DATE.test(date)) act(() => api.patch(itemPath(item), { planned_for: date }, ifMatch(item))); }}>
        <label>{t.plannedDate} <input type="date" aria-label={t.changePlanFor(item.title)} value={date} onChange={(e) => setDate(e.target.value)} /></label>
        <button type="submit" disabled={!DATE.test(date)}>{t.changePlan}</button>
      </form>
      <ProgressForm onSubmit={(b) => act(() => api.post(itemPath(item) + "/progress", b))} />
      <button type="button" onClick={() => act(() => api.post(itemPath(item) + "/transition", { to: "done", reason: t.doneReason }, ifMatch(item)))}>{t.done}</button>
    </li>
  );
}

type StaleChoice = "continue" | "reschedule" | "drop";

// 批量处理没有专用接口：逐条调既有接口（继续=记一条进展让时钟重新计时；改期=设计划日期；放弃=转 dropped），
// 每条独立成败，失败的留在列表里并报原因。
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
    <section aria-label={t.groups.stale}>
      <h2>{t.count(t.groups.stale, items.length)}</h2>
      {items.length === 0 ? <p className="sub">{t.empty}</p> : (
        <>
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
        </>
      )}
    </section>
  );
}
