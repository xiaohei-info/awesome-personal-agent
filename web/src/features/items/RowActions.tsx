import { useEffect, useRef, useState } from "react";
import { ErrorNote } from "../../app/errors";
import { useT } from "../../app/i18n";
import { HANDOFF_TAG } from "../../app/ids";
import { useSession } from "../../app/session";
import { AgentBlock } from "./HandOff";
import { DeleteItem } from "./DeleteItem";
import type { Item } from "./ItemsPage";
import { SlotFields, slotForm, slotValue } from "./ScheduleBlock";
import { useHostClock } from "./schedule";
import { ProgressForm } from "../today/ProgressForm";

/** 行上做了什么：done、plan、progress、edit（交给 Agent 等其余改动）。 */
export type RowAct = "done" | "plan" | "progress" | "edit" | "drop";
import { strings } from "./strings";

// 列表行上的快捷操作，放在 .act-row 里：按钮与标题同一行，展开的表单与 Agent 处理区（.row-panel）换到下一行占满整行（界面设计 §5.2、§9.3）：对应工作台任务行的「排时间、交给助手、完成」，另加「记进展」，今天页与事项列表共用。
// 写入走事项详情同样的接口（PATCH 带 If-Match、transition）；写完只重读这一件（GET /items/{id}）交给 onChange 原位替换，
// 不重读整页、不丢滚动位置。失败时显示服务端原因，并同样重读这一件，不留本地假状态。
// 「交给 Agent」就是事项详情的 Agent 处理区（AgentBlock）：加上交给 Agent 的标记后在行下展开同一块，选 Agent、看前提、现在开始都在那里。

const DATE = /^\d{4}-\d{2}-\d{2}$/;

export function RowActions({ item, where, onChange }: { item: Item; where: string; onChange: (it: Item, what: RowAct) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [open, setOpen] = useState<"plan" | "agent" | "progress" | "more" | null>(null);
  const [err, setErr] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const path = "/items/" + encodeURIComponent(item.id);
  const ifMatch = { "If-Match": String(item.version) };

  /** 写一次，再重读这一件；返回是否成功。what 告诉页面这次做了什么（今天页据此刷新计数、说明为什么离开了列表）。 */
  const write = async (fn: () => Promise<unknown>, what: RowAct = "edit"): Promise<boolean> => {
    setErr(null);
    setBusy(true);
    let ok = true;
    try {
      await fn();
    } catch (e) {
      ok = false;
      setErr(e);
    }
    await api.get<{ item: Item }>(path).then((r) => onChange(r.item, what), () => undefined);
    setBusy(false);
    return ok;
  };

  // 建议要先接受、已结束的事不再有行内操作（同工作台：完成的行不显示按钮）。
  if (["proposed", "done", "dropped"].includes(item.phase)) return null;
  const handed = !!item.tags?.includes(HANDOFF_TAG);
  const tagWrite = (tags: string[]) => write(() => api.patch(path, { tags, reason: t.handoffReason(tags.includes(HANDOFF_TAG)) }, ifMatch));
  const handoff = async () => {
    if (handed) return setOpen(open === "agent" ? null : "agent");
    if (await tagWrite([...(item.tags ?? []), HANDOFF_TAG])) setOpen("agent");
  };

  return (
    <>
      <span className="actions" role="group" aria-label={t.rowActions(item.title)}>
        <button type="button" aria-expanded={open === "plan"} onClick={() => setOpen(open === "plan" ? null : "plan")}>{t.rowPlan}</button>
        {/* 交给 Agent 只对任务（交给 Agent 的规则只认任务，§13.6）。 */}
        {item.kind === "task" && <button type="button" aria-expanded={open === "agent"} disabled={busy} onClick={() => void handoff()}>{handed ? t.rowAgent : t.rowHandoff}</button>}
        {/* 委派完成要带证据（§6.14），去详情填；其余一键完成。 */}
        {item.kind !== "delegation" && <button type="button" disabled={busy} onClick={() => void write(() => api.post(path + "/transition", { to: "done", reason: t.rowDoneReason(where) }, ifMatch), "done")}>{t.done}</button>}
        <button type="button" aria-expanded={open === "progress"} onClick={() => setOpen(open === "progress" ? null : "progress")}>{t.rowProgress}</button>
        <button type="button" aria-expanded={open === "more"} onClick={() => setOpen(open === "more" ? null : "more")}>{t.rowMore}</button>
      </span>
      {err != null && <div className="row-panel"><ErrorNote error={err} /></div>}
      {open === "more" && (
        <div className="row-panel">
          <DeleteItem title={item.title} busy={busy} onConfirm={() => void write(() => api.post(path + "/transition", { to: "dropped", reason: t.delReason(where) }, ifMatch), "drop")} />
        </div>
      )}
      {open === "plan" && <PlanForm item={item} onCancel={() => setOpen(null)} onSubmit={async (b) => {
        if (await write(() => api.patch(path, { ...b, reason: t.rowPlanReason(where) }, ifMatch), "plan")) setOpen(null);
      }} />}
      {open === "progress" && (
        <div className="row-panel" role="group" aria-label={t.rowProgressFor(item.title)}>
          <ProgressForm autoFocus label={t.rowSave} onSubmit={async (b) => {
            const ok = await write(() => api.post(path + "/progress", b), "progress");
            if (ok) setOpen(null);
            return ok;
          }} />
        </div>
      )}
      {open === "agent" && item.kind === "task" && (
        <div className="row-panel" aria-label={t.rowAgentFor(item.title)} role="group">
          <AgentBlock key={"h" + item.version} item={item} onSave={(tags) => void tagWrite(tags)}
            onAgent={(aid) => void write(() => api.patch(path, { preferred_agent_id: aid, reason: t.handoff }, ifMatch))} />
        </div>
      )}
    </>
  );
}

/** 排时间：日期必填（缺省今天，主机时区），时段可选；写计划日期与时段两项，截止仍在事项详情的排期里改。 */
function PlanForm({ item, onSubmit, onCancel }: { item: Item; onSubmit: (b: { planned_for: string; planned_slot?: string | null }) => Promise<void>; onCancel: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { today } = useHostClock(api);
  const [date, setDate] = useState(item.planned_for ?? "");
  const touched = useRef(false);
  // 没排过的预填今天（主机时区，时钟到手后再对一次）；本人动过就不再改。
  useEffect(() => { if (!touched.current && !item.planned_for) setDate(today); }, [today, item.planned_for]);
  const [slot, setSlot] = useState(() => slotForm(item.planned_slot));
  const day = date;
  const v = slotValue(day, slot);
  const submit = () => {
    if (!DATE.test(day) || v.bad) return;
    // 只发变了的时段：原来没有、现在也不定时就不带这一项。
    const slotChange = v.slot !== (item.planned_slot || null) ? { planned_slot: v.slot } : {};
    void onSubmit({ planned_for: day, ...slotChange });
  };
  return (
    <form className="panel row-panel" aria-label={t.rowPlanFor(item.title)} onSubmit={(e) => { e.preventDefault(); submit(); }}>
      <label>{t.plannedDate} <input type="date" value={day} onChange={(e) => { touched.current = true; setDate(e.target.value); }} required /></label>
      <SlotFields date={day} {...slot} bad={v.bad} onChange={(p) => setSlot({ ...slot, ...p })} />
      <div className="actions">
        <button type="submit" className="primary" disabled={!DATE.test(day) || v.bad}>{t.rowOk}</button>
        <button type="button" onClick={onCancel}>{t.cancel}</button>
      </div>
    </form>
  );
}
