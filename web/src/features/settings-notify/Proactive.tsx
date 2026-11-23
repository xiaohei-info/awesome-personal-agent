import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { Card, useDraft, type Doc } from "../settings-policy/shared";
import { strings } from "./strings";

// 到点提醒（§7.4）：策略 disturb.proactive 的时间规则；它是自动规则 › 提醒我 ›「到点提醒」单选下的"什么时候提醒"折叠。
// 开不开、送到哪由那个单选决定（不提醒即 proactive.on 为假），这里不再有开关，保存时 on 原样带回；预览按草稿、假设开着算。
// 静默时段沿用设置 › 提醒与推送的「安静时段」，不另设一份。开启时刻（since）由服务端记，页面不发。

export interface Proactive {
  on: boolean; since?: string; work_start: string; work_end: string; workdays: number[]; shift: boolean;
  start: boolean; end: boolean; lead_min: number; dedup_min: number; max_items: number; max_batches: number;
  task_push?: Record<string, boolean>; instance_push?: Record<string, boolean>; // 冻结键：只原样带回
}

/** 与 kernel/policy.DefaultProactive、TaskPushDefaults 一致：策略里没有这一段时就是它。 */
export const PROACTIVE_DEFAULT: Proactive = {
  on: false, work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true,
  start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6,
};

interface Preview { total: number; batches: number; next_at?: string; next_count: number; route: string; blocked?: string }

type Put = (fields: Record<string, unknown>) => Promise<number>;

const pad = (n: number) => String(n).padStart(2, "0");

/** 提交时只带服务端认的字段；since 由服务端记，不发。 */
function body(p: Proactive): Proactive {
  const { since: _since, ...rest } = p;
  return rest;
}

export function ProactiveCard({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  const { api } = useSession();
  const saved = (doc.disturb.proactive ?? null) as Proactive | null;
  const [p, setP, dirty] = useDraft(doc.disturb.proactive, () => ({ ...PROACTIVE_DEFAULT, ...saved }));
  const set = (x: Partial<Proactive>) => setP({ ...p, ...x });
  const [pv, setPv] = useState<Preview | null>(null);
  const [pvErr, setPvErr] = useState(false);
  const sig = JSON.stringify(body(p));
  useEffect(() => {
    let live = true;
    const h = setTimeout(() => {
      api.post<Preview>("/settings/proactive/preview", body(p)).then((r) => { if (live) { setPv(r); setPvErr(false); } }, () => { if (live) { setPv(null); setPvErr(true); } });
    }, 300);
    return () => { live = false; clearTimeout(h); };
  }, [api, sig]); // eslint-disable-line react-hooks/exhaustive-deps
  const quiet = doc.disturb.quiet as { start: string; end: string } | undefined;
  const day = (n: number) => (
    <label key={n}>
      <input type="checkbox" checked={p.workdays.includes(n)} onChange={(e) => set({ workdays: e.target.checked ? [...p.workdays, n].sort() : p.workdays.filter((d) => d !== n) })} /> {t.weekday[n - 1]}
    </label>
  );
  const num = (k: "lead_min" | "dedup_min" | "max_items" | "max_batches", label: string) => (
    <label>{label} <input type="number" min={0} aria-label={label} value={p[k]} onChange={(e) => set({ [k]: Number(e.target.value) } as Partial<Proactive>)} /></label>
  );
  return (
    <Card title={t.proactiveTimes} hint={t.proactiveHint} onSave={() => put({ proactive: body({ ...p, on: !!saved?.on }) })} dirty={dirty} id="proactive">
      <div role="status" aria-label={t.previewLabel} className="sub">
        {pv ? (
          <>
            {pv.total && pv.next_at ? (() => {
              const d = new Date(pv.next_at);
              return <><p>{t.previewNext(pv.next_count, t.when(d.getMonth() + 1, d.getDate(), `${pad(d.getHours())}:${pad(d.getMinutes())}`))}</p><p>{t.previewDay(pv.total, pv.batches)}</p></>;
            })() : <p>{t.previewNone}</p>}
            <p>{pv.blocked ? t.blocked[pv.blocked] ?? t.blocked.main_unavailable : t.toMain}</p>
            <p>{t.noBackfill}</p>
          </>
        ) : pvErr ? <p>{t.previewFailed}</p> : <p>{t.previewing}</p>}
      </div>
      <div className="sentence">
        <span>{t.workTime}</span>
        <input type="time" aria-label={t.workStart} value={p.work_start} onChange={(e) => set({ work_start: e.target.value })} />
        <span>{t.to}</span>
        <input type="time" aria-label={t.workEnd} value={p.work_end} onChange={(e) => set({ work_end: e.target.value })} />
      </div>
      <div role="group" aria-label={t.workdays} className="sentence"><span>{t.workdays}</span>{[1, 2, 3, 4, 5, 6, 7].map(day)}</div>
      <label><input type="checkbox" checked={p.shift} onChange={(e) => set({ shift: e.target.checked })} /> {t.shift}</label>
      <label><input type="checkbox" checked={p.start} onChange={(e) => set({ start: e.target.checked })} /> {t.remindStart}</label>
      <label><input type="checkbox" checked={p.end} onChange={(e) => set({ end: e.target.checked })} /> {t.remindEnd}</label>
      {num("lead_min", t.leadMin)}
      {num("dedup_min", t.dedupMin)}
      <div className="sentence">{num("max_items", t.maxItems)}{num("max_batches", t.maxBatches)}</div>
      <p className="sub">{quiet ? t.quietFollows(quiet.start, quiet.end) : t.quietNone}</p>
      <ul className="sub">{t.ruleLines.map((l) => <li key={l}>{l}</li>)}</ul>
    </Card>
  );
}
