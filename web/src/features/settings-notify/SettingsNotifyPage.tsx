import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { PRIORITY } from "../../app/ids";
import { EntityPick, useItemTitle } from "../../app/names";
import { Pick, useLabel } from "../../app/values";
import { CHANNEL_PREFIX } from "../../app/vocab";
import { Card, PolicyPage, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { ProactiveCard, TaskPushCard } from "./Proactive";
import { strings } from "./strings";

// 设置 › 提醒与打扰：策略的 disturb 段（§7.4）。按使用者的一件事拆成几节、各自保存（界面设计 §6.1）：怎么送、每天上限、暂停、安静时段、更多。
// PUT 替换整段，所以每节保存时带上整段：已保存的原样带回，只换这一节改过的字段。
// 路由只能选合法值：下次聊天时告诉我（inject）、只放在今天页的提醒里（web）、启用中的内置渠道（channel:<连接 id>，按连接名字显示）、
// 自写程序的渠道（channel:<名>，取自渠道扩展的密钥）；唤醒 Agent（wake）不是提醒本人，只在「高级」里出现。
// 文档里已有的其他取值保留为选项，不丢；指向已停用或已删除连接的路由照样显示并标明。
const ROUTES = ["policy"] as const;
const CLASSES = ["decision", "brief", "reminder", "health"] as const;
const MERGE_MIN = [0, 15, 30, 60];

export function SettingsNotifyPage() {
  const t = useT(strings);
  const { docs, error, load, save } = usePolicyDocs(ROUTES);
  const p = docs?.policy;
  const channels = useChannels();
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!p} onRetry={() => void load()}>
      {p && <DisturbCard doc={p} channels={channels} save={save} />}
    </PolicyPage>
  );
}

interface Channels { ids: string[]; names: Record<string, string> }

/** 可选的渠道：启用中的内置渠道连接按名字列出，没吊销的渠道扩展密钥按渠道名列出。取不到就只给内置的两种。 */
function useChannels(): Channels {
  const t = useT(strings);
  const { api } = useSession();
  const [conns, setConns] = useState<{ id: string; name: string; enabled: boolean }[]>([]);
  const [ext, setExt] = useState<string[]>([]);
  useEffect(() => {
    api.get<{ connections: { id: string; name: string; enabled: boolean }[] }>("/connections?direction=channel").then((r) => setConns(r.connections), () => setConns([]));
    api.get<{ credentials: { scope?: string; revoked_at?: string }[] }>("/settings/ext-keys").then(
      (r) => setExt([...new Set(r.credentials.filter((c) => !c.revoked_at && c.scope?.startsWith("channel.")).map((c) => c.scope!.slice("channel.".length)))]),
      () => setExt([]),
    );
  }, [api]);
  const names: Record<string, string> = {};
  for (const c of conns) names[CHANNEL_PREFIX + c.id] = c.enabled ? t.pushTo(c.name) : t.pushToOff(c.name);
  for (const n of ext) names[CHANNEL_PREFIX + n] = t.pushTo(n);
  const ids = [...conns.filter((c) => c.enabled).map((c) => CHANNEL_PREFIX + c.id), ...ext.map((n) => CHANNEL_PREFIX + n)];
  return { ids, names };
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

type Override = [cls: string, prio: string, route: string];
type Mute = [scope: string, id: string, until: string];
const pad = (n: number) => String(n).padStart(2, "0");
const toLocalInput = (iso?: string) => {
  if (!iso) return "";
  const d = new Date(iso);
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
};
const zone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

/** 每一节保存时发整段 disturb：已保存的内容加上这一节自己的改动；别的节没保存的草稿不带上（界面设计 §6.1）。 */
type Put = (fields: Record<string, unknown>) => Promise<number>;

function DisturbCard({ doc, channels, save }: { doc: Doc; channels: Channels; save: Save }) {
  const put: Put = (fields) => save("policy", { disturb: { ...doc.disturb, ...fields } });
  const t = useT(strings);
  const routesIn = doc.disturb.routes as Record<string, string>;
  const names: Record<string, string> = { ...channels.names };
  // 列表里没有的渠道路由：连接或自写程序渠道已删除，照样列出并标明，不显示原始 id。
  for (const r of Object.values(routesIn)) if (r.startsWith(CHANNEL_PREFIX) && !names[r]) names[r] = t.pushToGone;
  const routeIds = (cur: string, withWake = false) => {
    const ids = ["inject", "web", ...(withWake ? ["wake"] : []), ...channels.ids];
    return cur && !ids.includes(cur) ? [...ids, cur] : ids;
  };
  return (
    <>
      <Routes doc={doc} names={names} routeIds={routeIds} put={put} />
      <Push doc={doc} channels={channels} names={names} put={put} />
      <ProactiveCard doc={doc} put={put} />
      <TaskPushCard doc={doc} put={put} />
      <Cap doc={doc} put={put} />
      <Mutes doc={doc} put={put} />
      <Quiet doc={doc} put={put} />
      <More doc={doc} names={names} routeIds={routeIds} put={put} />
    </>
  );
}

// 推送哪些事（§7.4）：逐类开关、选渠道；没选渠道的跟「需要你决定」走。缺的类别取内核默认：要本人动手的开，纯信息的关。
// 同一串来源失败、同一次运行只推一次，新建议攒够才推且两次至少隔 2 小时（内核去重），这里只管开关与去向。
const PUSH = ["source_failing", "template_failed", "run_failed", "new_proposals"] as const;
const PUSH_DEFAULT: Record<string, Rule> = { source_failing: { on: true }, template_failed: { on: true }, run_failed: { on: true }, new_proposals: { on: false, min: 10 } };
interface Rule { on: boolean; route?: string; min?: number }
function Push({ doc, channels, names, put }: { doc: Doc; channels: Channels; names: Record<string, string>; put: Put }) {
  const t = useT(strings);
  const label = useLabel();
  const saved = (doc.disturb.push ?? {}) as Record<string, Rule>;
  const decision = (doc.disturb.routes as Record<string, string>).decision ?? "web";
  const [rules, setRules, dirty] = useDraft(doc.disturb.push, () => Object.fromEntries(PUSH.map((c) => [c, { ...PUSH_DEFAULT[c], ...saved[c] }])));
  const set = (c: string, r: Partial<Rule>) => setRules({ ...rules, [c]: { ...rules[c], ...r } });
  const opts = { ...names, "": t.pushFollow(names[decision] ?? label("route", decision)) };
  const submit = () => {
    const out: Record<string, Rule> = {};
    for (const c of PUSH) {
      const r = rules[c];
      if (c === "new_proposals" && !(Number.isInteger(Number(r.min)) && Number(r.min) >= 1)) return reject(t.pushMinInvalid);
      out[c] = { on: r.on, ...(r.route ? { route: r.route } : {}), ...(c === "new_proposals" ? { min: Number(r.min) } : {}) };
    }
    return put({ push: out });
  };
  return (
    <Card title={t.pushCard} hint={t.pushHint} onSave={submit} dirty={dirty}>
      <div className="line">
        <span>{t.pushKinds.decision}</span>
        <span className="sub">{t.pushDecision(names[decision] ?? label("route", decision))}</span>
      </div>
      {PUSH.map((c) => (
        <div key={c} className="line" role="group" aria-label={t.pushKinds[c]}>
          <label>
            <input type="checkbox" role="switch" checked={rules[c].on} aria-label={t.pushKinds[c]} onChange={(e) => set(c, { on: e.target.checked })} /> {t.pushKinds[c]}
            <small className="sub"> {t.pushWhen[c]}</small>
          </label>
          {c === "new_proposals" && (
            <label>{t.pushMin} <input type="number" min={1} value={rules[c].min ?? 10} aria-label={t.pushMin} onChange={(e) => set(c, { min: Number(e.target.value) })} /></label>
          )}
          <Pick k="route" names={opts} label={t.pushRoute(t.pushKinds[c])} value={rules[c].route ?? ""} ids={["", ...channels.ids]} disabled={!rules[c].on} onChange={(v) => set(c, { route: v })} />
        </div>
      ))}
    </Card>
  );
}

// 各类提醒怎么送：类别级路由（含文档里的其他键，如 wake）与「类别:优先级」级分开编辑，保存时合并；两行写到同一个键时在本地报重复。
function Routes({ doc, names, routeIds, put }: { doc: Doc; names: Record<string, string>; routeIds: (cur: string) => string[]; put: Put }) {
  const t = useT(strings);
  const label = useLabel();
  const routesIn = doc.disturb.routes as Record<string, string>;
  const [base, setBase, d1] = useDraft(doc.disturb.routes, () => Object.fromEntries(Object.entries(routesIn).filter(([k]) => !k.includes(":"))));
  const [overrides, setOverrides, d2] = useDraft(doc.disturb.routes, () => Object.entries(routesIn).filter(([k]) => k.includes(":")).map(([k, v]): Override => [k.split(":")[0], k.split(":")[1], v]));
  const setOverride = (i: number, j: number, v: string) => setOverrides(overrides.map((o, n) => (n === i ? (o.map((x, m) => (m === j ? v : x)) as Override) : o)));
  const submit = () => {
    const routes: Record<string, string> = { ...base };
    for (const [cls, prio, route] of overrides) {
      const k = cls + ":" + prio;
      if (k in routes) return reject(t.dup(`${label("deliveryClass", cls)} · ${label("priority", prio)}`));
      routes[k] = route;
    }
    return put({ routes });
  };
  return (
    <Card title={t.card} onSave={submit} dirty={d1 || d2}>
      <div>
        {CLASSES.map((c) => (
          <div key={c} className="line">
            <span>{label("deliveryClass", c)}</span>
            <Pick k="route" names={names} label={label("deliveryClass", c)} value={base[c] ?? "web"} ids={routeIds(base[c] ?? "web")} onChange={(v) => setBase({ ...base, [c]: v })} />
          </div>
        ))}
        {overrides.map(([cls, prio, route], i) => (
          <div key={i} className="line" role="group" aria-label={t.byPriority + " " + (i + 1)}>
            <span className="sentence">
              <Pick k="deliveryClass" label={t.overrideClass(i + 1)} value={cls} ids={[...CLASSES, ...(CLASSES as readonly string[]).includes(cls) ? [] : [cls]]} onChange={(v) => setOverride(i, 0, v)} />
              <Pick k="priority" label={t.overridePrio(i + 1)} value={prio} ids={(PRIORITY as readonly string[]).includes(prio) ? PRIORITY : [...PRIORITY, prio]} onChange={(v) => setOverride(i, 1, v)} />
            </span>
            <Pick k="route" names={names} label={t.overrideRoute(i + 1)} value={route} ids={routeIds(route)} onChange={(v) => setOverride(i, 2, v)} />
            <button type="button" aria-label={t.delOverride(i + 1)} onClick={() => setOverrides(overrides.filter((_, n) => n !== i))}>✕</button>
          </div>
        ))}
        <button type="button" className="link" onClick={() => setOverrides([...overrides, ["decision", "urgent", base.decision ?? "web"]])}>{t.addOverride}</button>
      </div>
    </Card>
  );
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
      {today && (
        <div aria-label={t.today(today.pushed)}>
          {capN > 0 && <div className="meter"><b style={{ width: `${Math.min(100, (today.pushed / capN) * 100)}%` }} /></div>}
          <p className="sub">{t.today(today.pushed)}{capN > 0 ? ` · ${t.overflow}` : ""}</p>
        </div>
      )}
    </Card>
  );
}

function Mutes({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  const [mutes, setMutes, dirty] = useDraft(doc.disturb.mutes, () => ((doc.disturb.mutes ?? []) as { scope: string; id: string; until?: string }[]).map((m): Mute => [m.scope, m.id, toLocalInput(m.until)]));
  const setMute = (i: number, j: number, v: string) => setMutes(mutes.map((r, n) => (n === i ? (r.map((x, m) => (m === j ? v : j === 0 && m === 1 ? "" : x)) as Mute) : r)));
  const submit = () =>
    put({ mutes: mutes.filter(([, id]) => id.trim()).map(([scope, id, until]) => ({ scope, id: id.trim(), ...(until ? { until: new Date(until).toISOString() } : {}) })) });
  return (
    <Card title={t.mutes} hint={t.mutesHint} onSave={submit} dirty={dirty}>
      {mutes.map(([scope, id, until], i) => (
        <div key={i} role="group" aria-label={t.mute(i + 1)} className="line">
          <span className="sentence">
            <Pick k="muteScope" label={t.muteScope(i + 1)} value={scope} onChange={(v) => setMute(i, 0, v)} />
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
    </Card>
  );
}

// 更多：合并窗口、截止后再提醒、两个只减不增的判断开关；唤醒 Agent 不是提醒本人，只在这里出现。
function More({ doc, names, routeIds, put }: { doc: Doc; names: Record<string, string>; routeIds: (cur: string, withWake?: boolean) => string[]; put: Put }) {
  const t = useT(strings);
  const { patch, val, set, dirty: d1 } = usePatch(doc, ["merge_window_sec", "remind_on_deadline", "threshold_judge", "followup_defer"]);
  const routesIn = doc.disturb.routes as Record<string, string>;
  const [wake, setWake, d2] = useDraft(routesIn.wake, () => routesIn.wake ?? "web");
  const mergeMin = Math.round(((val("merge_window_sec") as number) ?? 0) / 60);
  return (
    <details className="panel">
      <summary>{t.more}</summary>
      <Card title={t.more} onSave={() => put({ ...patch, ...(d2 ? { routes: { ...routesIn, wake } } : {}) })} dirty={d1 || d2}>
        <div className="stack">
          <label>
            {t.merge}
            <select value={mergeMin} onChange={(e) => set("merge_window_sec", Number(e.target.value) * 60)}>
              {[...new Set([...MERGE_MIN, mergeMin])].sort((a, b) => a - b).map((m) => (
                <option key={m} value={m}>{m ? t.minutes(m) : t.mergeOff}</option>
              ))}
            </select>
          </label>
          <label><input type="checkbox" checked={!!val("remind_on_deadline")} onChange={(e) => set("remind_on_deadline", e.target.checked)} /> {t.remindDeadline}</label>
          <label><input type="checkbox" checked={!!val("threshold_judge")} onChange={(e) => set("threshold_judge", e.target.checked)} /> {t.thresholdJudge}</label>
          <label><input type="checkbox" checked={!!val("followup_defer")} onChange={(e) => set("followup_defer", e.target.checked)} /> {t.followupDefer}</label>
          <label>
            {t.wake}
            <Pick k="route" names={names} value={wake} ids={routeIds(wake, true)} onChange={setWake} />
          </label>
        </div>
      </Card>
    </details>
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

