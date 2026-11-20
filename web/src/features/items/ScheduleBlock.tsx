import { useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { ErrorNote } from "../../app/errors";
import { useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import type { Item } from "./ItemsPage";
import { deadlineText, deadlineValue, hhmm, hostParts, parseSlot, slotOf, useHostClock } from "./schedule";
import { strings } from "./strings";

// 事项详情 › 排期（界面设计 §4、§9.4）：计划日期、时段、截止唯一可编辑的位置，整块保存。
// 保存带版本号；对不上时载入最新值，本人填的留在表单里并标出不同的项，确认后再存（不静默覆盖）。

interface Form {
  date: string;
  timed: boolean;
  start: string;
  end: string; // time 控件写不出 24:00，用 00:00 表示当天结束
  dueDate: string;
  dueTimed: boolean;
  dueTime: string;
}

function formOf(it: Item, offset: string): Form {
  const s = parseSlot(it.planned_slot);
  const due = it.deadline ? hostParts(it.deadline, offset) : null;
  const dueTimed = !!due && due.time !== "23:59:59";
  return {
    date: it.planned_for ?? "",
    timed: !!s,
    start: s ? hhmm(s[0]) : "09:00",
    end: s ? hhmm(s[1] % 1440) : "10:00",
    dueDate: due?.date ?? "",
    dueTimed,
    dueTime: dueTimed ? due!.time.slice(0, 5) : "18:00",
  };
}

/** 表单 → 三个字段的写入值（null 表示清除）。时段只在有日期且选了定时时给。 */
function valuesOf(f: Form, offset: string): { planned_for: string | null; planned_slot: string | null; deadline: string | null; badSlot: boolean } {
  const s = parseSlot(`${f.start} - ${f.end === "00:00" ? "24:00" : f.end}`);
  const slot = f.date && f.timed && s ? slotOf(s[0], s[1]) : null;
  return {
    planned_for: f.date || null,
    planned_slot: slot,
    deadline: f.dueDate ? deadlineValue(f.dueDate, f.dueTimed ? f.dueTime : null, offset) : null,
    badSlot: !!f.date && f.timed && !s,
  };
}

const sameInstant = (a?: string | null, b?: string | null) => (a && b ? Date.parse(a) === Date.parse(b) : !a && !b);

export function ScheduleBlock({ item, path, onSaved }: { item: Item; path: string; onSaved: () => Promise<void> }) {
  const t = useT(strings);
  const { api } = useSession();
  const { utc_offset: offset } = useHostClock(api);
  const [f, setF] = useState(() => formOf(item, offset));
  const [dirty, setDirty] = useState(false);
  const [conflict, setConflict] = useState(false);
  const [err, setErr] = useState<unknown>(null);
  const [ok, setOk] = useState(false);

  // 没动过表单时跟着服务端的最新值走；动过之后保留本人填的（冲突时靠下面的差异标记对照）。
  useEffect(() => {
    if (!dirty) setF(formOf(item, offset));
  }, [item, offset, dirty]);

  const set = (p: Partial<Form>) => {
    setF({ ...f, ...p });
    setDirty(true);
    setOk(false);
  };
  const v = valuesOf(f, offset);
  const body: Record<string, unknown> = {};
  if (v.planned_for !== (item.planned_for || null)) body.planned_for = v.planned_for;
  if (v.planned_slot !== (item.planned_slot || null)) body.planned_slot = v.planned_slot;
  if (!sameInstant(v.deadline, item.deadline)) body.deadline = v.deadline;
  const changed = Object.keys(body).length > 0;
  const latest = formOf(item, offset);
  const differs = conflict ? {
    date: f.date !== latest.date,
    slot: v.planned_slot !== (item.planned_slot || null),
    due: !sameInstant(v.deadline, item.deadline),
  } : { date: false, slot: false, due: false };

  const save = async () => {
    setErr(null);
    try {
      // 清日期时内核会一起清时段，这里不必再发 planned_slot。
      if (body.planned_for === null) delete body.planned_slot;
      await api.patch(path, { ...body, reason: t.scheduleReason }, { "If-Match": String(item.version) });
      setDirty(false);
      setConflict(false);
      setOk(true);
      await onSaved();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setConflict(true);
        await onSaved();
      } else setErr(e);
    }
  };

  const latestSlot = item.planned_slot || t.untimed;
  const latestDue = item.deadline ? deadlineText(item.deadline, offset) : t.notSet;
  return (
    <section aria-label={t.schedule} id="schedule">
      <h2>{t.schedule}</h2>
      <form className="panel" onSubmit={(e) => { e.preventDefault(); if (changed && !v.badSlot) save(); }}>
        {conflict && <div role="alert"><p>{t.conflict}</p><p className="sub">{t.conflictKeep}</p></div>}
        <label className={differs.date ? "differs" : undefined}>
          {t.plannedDate} <input type="date" value={f.date} onChange={(e) => set({ date: e.target.value })} />
          {differs.date && <span className="sub">{t.latest(item.planned_for || t.notSet)}</span>}
        </label>
        <fieldset className={differs.slot ? "differs" : undefined} disabled={!f.date}>
          <legend>{t.slot}</legend>
          <label><input type="checkbox" checked={!f.timed} onChange={(e) => set({ timed: !e.target.checked })} /> {t.untimed}</label>
          {f.timed && (
            <div className="actions">
              <label>{t.slotStart} <input type="time" step={60} value={f.start} onChange={(e) => set({ start: e.target.value })} /></label>
              <label>{t.slotEnd} <input type="time" step={60} value={f.end} onChange={(e) => set({ end: e.target.value })} /></label>
            </div>
          )}
          {v.badSlot && <p role="alert">{t.badSlot}</p>}
          {differs.slot && <span className="sub">{t.latest(latestSlot)}</span>}
        </fieldset>
        <fieldset className={differs.due ? "differs" : undefined}>
          <legend>{t.deadlineField}</legend>
          <label>{t.deadlineField} <input type="date" value={f.dueDate} onChange={(e) => set({ dueDate: e.target.value })} /></label>
          {f.dueDate && <label><input type="checkbox" checked={f.dueTimed} onChange={(e) => set({ dueTimed: e.target.checked })} /> {t.dueTimed}</label>}
          {f.dueDate && f.dueTimed && <label>{t.dueTime} <input type="time" step={60} value={f.dueTime} onChange={(e) => set({ dueTime: e.target.value })} /></label>}
          {differs.due && <span className="sub">{t.latest(latestDue)}</span>}
        </fieldset>
        {err != null && <ErrorNote error={err} />}
        <div className="actions">
          <button type="submit" className="primary" disabled={!changed || v.badSlot}>{conflict ? t.confirmSave : t.saveSchedule}</button>
          {ok && <span role="status">{t.saved}</span>}
        </div>
      </form>
    </section>
  );
}
