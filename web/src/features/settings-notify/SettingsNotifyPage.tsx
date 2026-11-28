import { useEffect, useState } from "react";
import { Skeleton } from "../../app/ui";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { MUTE_SCOPE } from "../../app/ids";
import { EntityPick, useItemTitle } from "../../app/names";
import { Pick } from "../../app/values";
import { PageHead } from "../../app/page";
import { Card, reject, useDraft } from "../../app/policy-cards";
import type { Delivery } from "./matrix";
import { EDITORS, HOME_PATH, current, kindOf, targetOf, type Choice } from "./reminders";
import { ProactiveCard } from "./Proactive";
import { Reminder, ReminderScope, WhenReady, useInstanceName, usePreview, useReminderScope, useScopeReady, type OverviewRow } from "./Reminder";
import { rstrings, strings } from "./strings";

// 设置 › 提醒与推送（设计 v2 §3.1、§5）：全局规矩——暂停所有提醒、默认渠道、安静时段（含临近截止照发）、每天上限，
// 「更多」里是暂停名单与合并窗口；「有事要你拍板时怎么告诉你」与「自动规则」两组提醒单选也在这里。
// 其余各类提醒送到哪在它自己的页面上改（定时安排卡、来源、来源处理、Agent、建议、状态）。
// 末尾一张只读总览：行表取自服务端（GET /settings/notify/overview，与内核选去处同一张行表），每行一句话加「去改」。
// 每一节各自保存，发整段 disturb：已保存的原样带回，只换这一节改过的字段。
const MERGE_MIN = [0, 15, 30, 60];

export function SettingsNotifyPage() {
  const t = useT(strings);
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <ReminderScope>
        <Loading />
        <WhenReady>
          <Globals />
          <Decisions />
          <AutoRules />
          <OverviewTable />
        </WhenReady>
      </ReminderScope>
    </main>
  );
}

function Loading() {
  const t = useT(strings);
  return useScopeReady() ? null : <Skeleton shape="card" label={t.loading} />;
}

type Put = (fields: Record<string, unknown>) => Promise<number>;
interface Mute { scope: string; id: string; until?: string }

function Globals() {
  const t = useT(strings);
  const { doc, putDisturb } = useReminderScope();
  return (
    <>
      <PauseAll doc={doc} put={putDisturb} />
      <DefaultChannel />
      <Quiet doc={doc} put={putDisturb} />
      <Cap doc={doc} put={putDisturb} />
      <details className="panel">
        <summary>{t.more}</summary>
        <Mutes doc={doc} put={putDisturb} />
        <Merge doc={doc} put={putDisturb} />
      </details>
    </>
  );
}

function Decisions() {
  const t = useT(strings);
  return (
    <section aria-label={t.notifyCard} className="panel stack" id="notify">
      <h2>{t.notifyCard}</h2>
      <Reminder row="decision" />
      <Reminder row="agent_answered" />
    </section>
  );
}

/** 「自动规则」：到点、截止已过、跟进、停滞四个单选（到点的时间规则收在"什么时候提醒"里），以及规则自带的提醒（线上没有这种规则时不显示）。 */
function AutoRules() {
  const t = useT(strings);
  const { doc, putDisturb, overview } = useReminderScope();
  const own = (overview?.rows ?? []).filter((x) => (x.key === "rule_reminder" || x.key === "source_alert") && x.rules?.length);
  return (
    <section aria-label={t.autoRules} className="panel stack" id="auto-rules">
      <h2>{t.autoRules}</h2>
      <Reminder row="scheduled">
        <details>
          <summary>{t.remindWhen}</summary>
          <ProactiveCard doc={doc} put={putDisturb} />
        </details>
      </Reminder>
      <Reminder row="overdue" />
      <Reminder row="followup" />
      <Reminder row="stale" />
      {own.map((x) => <Reminder key={x.key} row={x.key} />)}
    </section>
  );
}

type Doc = { disturb: Record<string, any> };
const pad = (n: number) => String(n).padStart(2, "0");
const toLocalInput = (iso?: string) => {
  if (!iso) return "";
  const d = new Date(iso);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
};
const zone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
const mutesOf = (doc: Doc) => (doc.disturb.mutes ?? []) as Mute[];

/** 暂停所有提醒：暂停名单里一条范围 all（可带到期），与按来源、项目、事项的暂停同一套机制。 */
function PauseAll({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  const all = mutesOf(doc).find((m) => m.scope === "all");
  const [d, setD, dirty] = useDraft(all ?? null, () => ({ on: !!all, until: toLocalInput(all?.until) }));
  const submit = () => {
    const rest = mutesOf(doc).filter((m) => m.scope !== "all");
    return put({ mutes: d.on ? [...rest, { scope: "all", id: "", ...(d.until ? { until: new Date(d.until).toISOString() } : {}) }] : rest });
  };
  return (
    <Card title={t.pauseAll} onSave={submit} dirty={dirty} id="pause-all">
      <label><input type="checkbox" role="switch" checked={d.on} onChange={(e) => setD({ ...d, on: e.target.checked })} /> {t.pauseAllOn}</label>
      {d.on && (
        <label>{t.pauseUntil} <input type="datetime-local" aria-label={t.pauseUntil} value={d.until} onChange={(e) => setD({ ...d, until: e.target.value })} /></label>
      )}
      <p className="sub">{d.on ? t.pauseEffectOn : t.pauseEffectOff}</p>
    </Card>
  );
}

/** 默认渠道：各页选"推到手机"时用它。 */
function DefaultChannel() {
  const t = useT(strings);
  const { doc, channels, putDisturb, overview, state } = useReminderScope();
  const m = doc.disturb.delivery as Delivery;
  const [v, setV, dirty] = useDraft(m.default_channel, () => m.default_channel);
  const ids = [...channels.ids, ...(m.default_channel && !channels.ids.includes(m.default_channel) ? [m.default_channel] : [])];
  const label = useRowLabel();
  const using = (overview?.rows ?? []).filter((r) => shown(r) && targetOf(m, r.key, state.packs).user === "default" && ["phone", "both"].includes(current(state, r.key)));
  return (
    <Card title={t.defaultChannel} onSave={() => putDisturb({ delivery: { ...m, default_channel: v } })} dirty={dirty} id="default-channel">
      <select aria-label={t.defaultChannel} value={v} onChange={(e) => setV(e.target.value)}>
        <option value="">{t.noDefault}</option>
        {ids.map((id) => <option key={id} value={id}>{channels.plain[id] ?? t.pushToGone}</option>)}
      </select>
      <p className="sub">{t.defaultEffect}</p>
      {overview && <p className="sub" role="note">{t.defaultUsing(using.length, using.map((r) => label(r.key)).join("、"))}</p>}
    </Card>
  );
}

interface Today { pushed: number; cap: number }
function useToday(): Today | null {
  const { api } = useSession();
  const [today, setToday] = useState<Today | null>(null);
  useEffect(() => {
    api.get<Today>("/deliveries/today").then(setToday, () => setToday(null));
  }, [api]);
  return today;
}

function Cap({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  const today = useToday();
  const [cap, setCap, dirty] = useDraft(doc.disturb.daily_cap, () => String(doc.disturb.daily_cap ?? 20));
  const capN = Number(cap);
  const submit = () => {
    if (cap.trim() === "" || !Number.isInteger(capN) || capN < 0) return reject(t.capInvalid);
    return put({ daily_cap: capN });
  };
  return (
    <Card title={t.capHead} onSave={submit} dirty={dirty}>
      <div className="sentence">
        <input type="number" min={0} aria-label={t.cap} value={capN === 0 && cap.trim() !== "" ? "" : cap} disabled={capN === 0 && cap.trim() !== ""} onChange={(e) => setCap(e.target.value)} />
        <label>
          <input type="checkbox" checked={capN === 0 && cap.trim() !== ""} onChange={(e) => setCap(e.target.checked ? "0" : "20")} /> {t.noCap}
        </label>
      </div>
      <p className="sub">{t.overflow}</p>
      {today && (
        <div aria-label={t.today(today.pushed)}>
          {capN > 0 && <div className="meter"><b style={{ width: `${Math.min(100, (today.pushed / capN) * 100)}%` }} /></div>}
          <p className="sub">{t.today(today.pushed)}{capN > 0 ? t.ofCap(capN) : ""}</p>
        </div>
      )}
    </Card>
  );
}

/** 暂停名单（按来源、项目、事项）；"暂停所有提醒"那一条不在这里列，保存时原样带回。 */
function Mutes({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  type Row = [scope: string, id: string, until: string];
  const [mutes, setMutes, dirty] = useDraft(doc.disturb.mutes, () => mutesOf(doc).filter((m) => m.scope !== "all").map((m): Row => [m.scope, m.id, toLocalInput(m.until)]));
  const setMute = (i: number, j: number, v: string) => setMutes(mutes.map((r, n) => (n === i ? (r.map((x, m) => (m === j ? v : j === 0 && m === 1 ? "" : x)) as Row) : r)));
  const submit = () =>
    put({
      mutes: [
        ...mutes.filter(([, id]) => id.trim()).map(([scope, id, until]) => ({ scope, id: id.trim(), ...(until ? { until: new Date(until).toISOString() } : {}) })),
        ...mutesOf(doc).filter((m) => m.scope === "all"),
      ],
    });
  return (
    <Card title={t.mutes} hint={t.mutesHint} onSave={submit} dirty={dirty}>
      {mutes.length === 0 && <p className="sub">{t.noMutes}</p>}
      {mutes.map(([scope, id, until], i) => (
        <div key={i} role="group" aria-label={t.mute(i + 1)} className="line">
          <span className="sentence">
            <Pick k="muteScope" ids={MUTE_SCOPE.filter((s) => s !== "all")} label={t.muteScope(i + 1)} value={scope} onChange={(v) => setMute(i, 0, v)} />
            <MuteTarget scope={scope} id={id} label={t.muteTarget(i + 1)} placeholder={t.itemId} onChange={(v) => setMute(i, 1, v)} />
          </span>
          <input type="datetime-local" aria-label={t.muteUntil(i + 1)} title={t.forever} value={until} onChange={(e) => setMute(i, 2, e.target.value)} />
          <button type="button" aria-label={t.resume(i + 1)} onClick={() => setMutes(mutes.filter((_, n) => n !== i))}>✕</button>
        </div>
      ))}
      <div>
        <button type="button" className="link" onClick={() => setMutes([...mutes, ["source", "", ""]])}>{t.addMute}</button>
      </div>
    </Card>
  );
}

/** 只在改过时才写进请求体，没改的随整段原样带回。 */
function usePatch(doc: Doc, keys: string[]) {
  const sig = keys.map((k) => doc.disturb[k]);
  const [patch, setPatch] = useDraft<Record<string, unknown>>(sig, () => ({}));
  const val = (k: string) => (k in patch ? patch[k] : doc.disturb[k]);
  const set = (k: string, v: unknown) => setPatch({ ...patch, [k]: v });
  const dirty = Object.keys(patch).some((k) => JSON.stringify(patch[k]) !== JSON.stringify(doc.disturb[k]));
  return { patch, val, set, dirty };
}

function Quiet({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  const { patch, val, set, dirty } = usePatch(doc, ["quiet", "quiet_exempt"]);
  const quiet = val("quiet") as { start: string; end: string; zone?: string } | undefined;
  return (
    <Card title={t.quiet} onSave={() => put(patch)} dirty={dirty}>
      <label>
        <input type="checkbox" checked={!!quiet} onChange={(e) => set("quiet", e.target.checked ? { start: "22:00", end: "08:00", zone: zone() } : undefined)} /> {t.quietOn}
      </label>
      {quiet && (
        <>
          <div className="sentence">
            <span>{t.from}</span>
            <input type="time" aria-label={t.quiet + " " + t.from} value={quiet.start} onChange={(e) => set("quiet", { ...quiet, start: e.target.value })} />
            <span>{t.to}</span>
            <input type="time" aria-label={t.quiet + " " + t.to} value={quiet.end} onChange={(e) => set("quiet", { ...quiet, end: e.target.value })} />
          </div>
          <label>
            <input type="checkbox" checked={!!val("quiet_exempt")} onChange={(e) => set("quiet_exempt", e.target.checked)} /> {t.quietExempt}
          </label>
        </>
      )}
      <p className="sub">{quiet ? t.quietEffect(quiet.start, quiet.end) : t.quietOffEffect}</p>
    </Card>
  );
}

function Merge({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  const { patch, val, set, dirty } = usePatch(doc, ["merge_window_sec"]);
  const mergeMin = Math.round(((val("merge_window_sec") as number) ?? 0) / 60);
  return (
    <Card title={t.merge} onSave={() => put(patch)} dirty={dirty}>
      <select aria-label={t.merge} value={mergeMin} onChange={(e) => set("merge_window_sec", Number(e.target.value) * 60)}>
        {[...new Set([...MERGE_MIN, mergeMin])].sort((a, b) => a - b).map((m) => (
          <option key={m} value={m}>{m ? t.minutes(m) : t.mergeOff}</option>
        ))}
      </select>
    </Card>
  );
}

/** 暂停对象：来源与项目按名称选；事项填编号并显示标题。 */
function MuteTarget({ scope, id, label, placeholder, onChange }: { scope: string; id: string; label: string; placeholder: string; onChange: (v: string) => void }) {
  const title = useItemTitle(scope === "item" ? id : null);
  if (scope === "source" || scope === "project") return <EntityPick kind={scope} label={label} value={id} empty="" onChange={onChange} />;
  return (
    <>
      <input aria-label={label} placeholder={placeholder} value={id} onChange={(e) => onChange(e.target.value)} />
      {title && title !== id && <span className="sub">{title}</span>}
    </>
  );
}

// ---- 只读总览 ----

/** 总览里列哪些行：有产生方的；内置类型的类型行不单列（它们的生效值在各自安排卡上）。 */
const shown = (r: OverviewRow) => r.produced && !/^task_(result|failed):(brief|review|routine)$/.test(r.key);

function useRowLabel(): (key: string) => string {
  const r = useT(rstrings);
  const { instances } = useReminderScope();
  const name = useInstanceName();
  return (key) => {
    const m = /^(instance|instance_failed):(.+)$/.exec(key);
    if (m) {
      const i = instances.find((x) => x.id === m[2]);
      const n = i ? name(i) : m[2];
      return m[1] === "instance" ? r.instanceResult(n) : r.instanceFailed(n);
    }
    return r.rows[key] ?? key;
  };
}

function OverviewTable() {
  const t = useT(strings);
  const r = useT(rstrings);
  const { overview, state, channels } = useReminderScope();
  const label = useRowLabel();
  if (!overview) return null;
  const m = state.disturb.delivery as Delivery;
  const def = m.default_channel ? channels.plain[m.default_channel] ?? r.goneChannel : r.noDefault;
  const text = (key: string, c: Choice) => {
    const u = targetOf(m, key, state.packs).user;
    const ch = u === "default" || u === "off" ? def : channels.plain[u] ?? r.goneChannel;
    return c === "phone" ? r.opts.phone(ch) : c === "both" ? r.opts.both(ch) : c === "off" ? t.overviewOff : r.opts[c];
  };
  const rows = overview.rows.filter(shown).map((x) => ({ ...x, c: x.blocked ? null : current(state, x.key) }));
  rows.sort((a, b) => Number(a.c === "off" || a.c === null) - Number(b.c === "off" || b.c === null)); // 不提醒的排在最后
  return (
    <section aria-label={t.overview} className="panel stack" id="overview">
      <h2>{t.overview}</h2>
      <p className="sub">{t.overviewHint}</p>
      <table>
        <thead><tr><th>{t.colWhat}</th><th>{t.colHow}</th><th>{t.colData}</th><th>{t.colWhere}</th></tr></thead>
        <tbody>
          {rows.map((x) => {
            const home = EDITORS[kindOf(x.key)];
            return (
              <tr key={x.key} className={x.c === "off" || x.c === null ? "sub" : undefined} data-row={x.key}>
                <td>{label(x.key)}</td>
                <td>{x.c ? text(x.key, x.c) : r.preview.blocked[x.blocked!]}</td>
                <td><RowPreview k={x.key} /></td>
                <td><Link to={HOME_PATH[home] + "#reminder-" + x.key}>{t.homes[home]} ›</Link></td>
              </tr>
            );
          })}
          <tr className="sub"><td>{t.hostCron}</td><td colSpan={3}>{t.hostCronWhy}</td></tr>
        </tbody>
      </table>
    </section>
  );
}

function RowPreview({ k }: { k: string }) {
  return <>{usePreview(k)}</>;
}
