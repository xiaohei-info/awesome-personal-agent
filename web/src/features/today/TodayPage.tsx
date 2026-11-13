import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import type { Item } from "../items/ItemsPage";
import { ProgressForm } from "./ProgressForm";

// 形状对应 GET /v1/today（附录 B.2）：四组取材全在服务端，本页只展示并把操作交给既有事项接口。
interface Today { planned: Item[]; due: Item[]; pending: Item[]; stale: Item[] }
type Group = keyof Today;

const TITLES: Record<Group, string> = { planned: "计划今天或更早", due: "今天到期", pending: "等你决定", stale: "久未推进" };
const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");
const DATE = /^\d{4}-\d{2}-\d{2}$/;
const PENDING_SHOWN = 5; // 待决只列最紧的几条，其余去决策待办

export function TodayPage() {
  const { api } = useSession();
  const [data, setData] = useState<Today | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    () => api.get<Today>("/today").then((r) => setData(r), (e) => setError(msg(e))),
    [api],
  );
  useEffect(() => { load(); }, [load]);

  // 写入后整页重读：分组永远以服务端为准，失败时不留下本地假状态。
  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try { await fn(); } catch (e) { setError(msg(e)); }
    await load();
  };

  return (
    <main>
      <PageHead lede="今天要做的、到期的、等你决定的，和久未推进的。">今天</PageHead>
      {error && <p role="alert">{error}</p>}
      {!data ? (
        !error && <p role="status">加载中…</p>
      ) : (
        <>
          <Section group="planned" items={data.planned} render={(it) => <PlannedRow key={it.id} item={it} act={act} />} />
          <Section group="due" items={data.due} render={(it) => <li key={it.id}><ItemLink item={it} /></li>} />
          <section aria-label={TITLES.pending}>
            <h2>{TITLES.pending}（{data.pending.length}）</h2>
            {data.pending.length === 0 ? <p className="sub">这一组没有事项。</p> : (
              <ul className="rows">
                {data.pending.slice(0, PENDING_SHOWN).map((it) => <li key={it.id}><ItemLink item={it} /></li>)}
              </ul>
            )}
            {data.pending.length > 0 && <p><Link to="/decisions">去决策待办处理</Link></p>}
          </section>
          <Stale items={data.stale} act={act} />
        </>
      )}
    </main>
  );
}

function Section({ group, items, render }: { group: Group; items: Item[]; render: (it: Item) => JSX.Element }) {
  return (
    <section aria-label={TITLES[group]}>
      <h2>{TITLES[group]}（{items.length}）</h2>
      {items.length === 0 ? <p className="sub">这一组没有事项。</p> : <ul className="rows">{items.map(render)}</ul>}
    </section>
  );
}

function ItemLink({ item }: { item: Item }) {
  return (
    <span>
      <Link to={"/items/" + encodeURIComponent(item.id)}>{item.title}</Link>{" "}
      <span className="sub">{[item.id, item.planned_for && `计划 ${item.planned_for}`, item.deadline && `截止 ${new Date(item.deadline).toLocaleDateString()}`].filter(Boolean).join(" · ")}</span>
    </span>
  );
}

const itemPath = (it: Item) => "/items/" + encodeURIComponent(it.id);
const ifMatch = (it: Item) => ({ "If-Match": String(it.version) });

function PlannedRow({ item, act }: { item: Item; act: (fn: () => Promise<unknown>) => Promise<void> }) {
  const { api } = useSession();
  const [date, setDate] = useState("");
  return (
    <li className="panel stack" data-item={item.id}>
      <ItemLink item={item} />
      <form className="actions" onSubmit={(e) => { e.preventDefault(); if (DATE.test(date)) act(() => api.patch(itemPath(item), { planned_for: date }, ifMatch(item))); }}>
        <label>计划日期 <input type="date" aria-label={`改计划日期 ${item.title}`} value={date} onChange={(e) => setDate(e.target.value)} /></label>
        <button type="submit" disabled={!DATE.test(date)}>改计划日期</button>
      </form>
      <ProgressForm onSubmit={(b) => act(() => api.post(itemPath(item) + "/progress", b))} />
      <button type="button" onClick={() => act(() => api.post(itemPath(item) + "/transition", { to: "done", reason: "在今天页完成" }, ifMatch(item)))}>完成</button>
    </li>
  );
}

type StaleChoice = "continue" | "reschedule" | "drop";

// 批量处理没有专用接口：逐条调既有接口（继续=记一条进展让时钟重新计时；改期=设计划日期；放弃=转 dropped），
// 每条独立成败，失败的留在列表里并报原因。
function Stale({ items, act }: { items: Item[]; act: (fn: () => Promise<unknown>) => Promise<void> }) {
  const { api } = useSession();
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
          if (choice === "continue") await api.post(itemPath(it) + "/progress", { text: "继续" });
          else if (choice === "reschedule") await api.patch(itemPath(it), { planned_for: date }, ifMatch(it));
          else await api.post(itemPath(it) + "/transition", { to: "dropped", reason: "久未推进，本人选择放弃" }, ifMatch(it));
        } catch (e) { failed.push(`${it.title}：${msg(e)}`); }
      }
      setPicked(new Set());
      if (failed.length) throw new Error(failed.join("；"));
    });

  return (
    <section aria-label={TITLES.stale}>
      <h2>{TITLES.stale}（{items.length}）</h2>
      {items.length === 0 ? <p className="sub">这一组没有事项。</p> : (
        <>
          <ul className="rows">
            {items.map((it) => (
              <li key={it.id}>
                <label><input type="checkbox" checked={picked.has(it.id)} onChange={() => toggle(it.id)} aria-label={`选择 ${it.title}`} /> </label>
                <ItemLink item={it} />
              </li>
            ))}
          </ul>
          <div className="panel actions">
            <label>处理方式
              <select value={choice} onChange={(e) => setChoice(e.target.value as StaleChoice)}>
                <option value="continue">继续</option>
                <option value="reschedule">改期</option>
                <option value="drop">放弃</option>
              </select>
            </label>
            {choice === "reschedule" && <label>新计划日期 <input type="date" aria-label="批量改期日期" value={date} onChange={(e) => setDate(e.target.value)} /></label>}
            <button type="button" disabled={!ok} onClick={apply}>应用到所选（{picked.size}）</button>
          </div>
        </>
      )}
    </section>
  );
}
