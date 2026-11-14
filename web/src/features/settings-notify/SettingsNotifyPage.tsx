import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { PRIORITY } from "../../app/ids";
import { EntityPick, useItemTitle } from "../../app/names";
import { Pick, useLabel } from "../../app/values";
import { CHANNEL_PREFIX } from "../../app/vocab";
import { Card, PolicyPage, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { strings } from "./strings";

// 设置 › 提醒与打扰：策略的 disturb 段（§7.4）。PUT 替换整段，所以保存时带上整段：没在这页改过的字段原样带回。
// 路由只能选合法值：下次聊天时告诉我（inject）、只放网页未读（web）、已接入的渠道（channel:<名>，取自渠道扩展的密钥）；
// 唤醒宿主（wake）不是提醒本人，只在「高级」里出现。文档里已有的其他取值保留为选项，不丢。
const ROUTES = ["policy"] as const;
const CLASSES = ["decision", "brief", "reminder", "health"] as const;
const MERGE_MIN = [0, 15, 30, 60];

export function SettingsNotifyPage() {
  const t = useT(strings);
  const { docs, error, load, save } = usePolicyDocs(ROUTES);
  const p = docs?.policy;
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!p} onRetry={() => void load()}>
      {p && <DisturbCard doc={p} save={save} />}
    </PolicyPage>
  );
}

/** 已接入的渠道名：ext 密钥里 scope 为 channel.<名> 且没吊销的。 */
function useChannels(): string[] {
  const { api } = useSession();
  const [names, setNames] = useState<string[]>([]);
  useEffect(() => {
    api.get<{ credentials: { scope?: string; revoked_at?: string }[] }>("/settings/ext-keys").then(
      (r) => setNames([...new Set(r.credentials.filter((c) => !c.revoked_at && c.scope?.startsWith("channel.")).map((c) => c.scope!.slice("channel.".length)))]),
      () => setNames([]), // 渠道只用来列选项，取不到就只给内置的两种
    );
  }, [api]);
  return names;
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

function DisturbCard({ doc, save }: { doc: Doc; save: Save }) {
  const t = useT(strings);
  const label = useLabel();
  const channels = useChannels();
  const today = useToday();
  const routesIn = doc.disturb.routes as Record<string, string>;
  // 类别级路由（含文档里的其他键，如 wake）与「类别:优先级」级分开编辑，保存时合并；两行写到同一个键时在本地报重复。
  const [base, setBase] = useDraft(doc.disturb, () => Object.fromEntries(Object.entries(routesIn).filter(([k]) => !k.includes(":"))));
  const [overrides, setOverrides] = useDraft(doc.disturb, () => Object.entries(routesIn).filter(([k]) => k.includes(":")).map(([k, v]): Override => [k.split(":")[0], k.split(":")[1], v]));
  const [cap, setCap] = useDraft(doc.disturb, () => String(doc.disturb.daily_cap ?? 20));
  const [mutes, setMutes] = useDraft(doc.disturb, () => ((doc.disturb.mutes ?? []) as { scope: string; id: string; until?: string }[]).map((m): Mute => [m.scope, m.id, toLocalInput(m.until)]));
  // 其余字段只在改过时才写进请求体，没改的随整段原样带回。
  const [patch, setPatch] = useDraft<Record<string, unknown>>(doc.disturb, () => ({}));
  const val = (k: string) => (k in patch ? patch[k] : doc.disturb[k]);
  const set = (k: string, v: unknown) => setPatch({ ...patch, [k]: v });

  const routeIds = (cur: string, withWake = false) => {
    const ids = ["inject", "web", ...(withWake ? ["wake"] : []), ...channels.map((c) => CHANNEL_PREFIX + c)];
    return cur && !ids.includes(cur) ? [...ids, cur] : ids;
  };
  const setOverride = (i: number, j: number, v: string) => setOverrides(overrides.map((o, n) => (n === i ? (o.map((x, m) => (m === j ? v : x)) as Override) : o)));
  const setMute = (i: number, j: number, v: string) => setMutes(mutes.map((r, n) => (n === i ? (r.map((x, m) => (m === j ? v : j === 0 && m === 1 ? "" : x)) as Mute) : r)));
  const quiet = val("quiet") as { start: string; end: string; zone?: string } | undefined;
  const mergeMin = Math.round(((val("merge_window_sec") as number) ?? 0) / 60);

  const submit = () => {
    const n = Number(cap);
    if (cap.trim() === "" || !Number.isInteger(n) || n < 0) return reject(t.capInvalid);
    const routes: Record<string, string> = { ...base };
    for (const [cls, prio, route] of overrides) {
      const k = cls + ":" + prio;
      if (k in routes) return reject(t.dup(`${label("deliveryClass", cls)} · ${label("priority", prio)}`));
      routes[k] = route;
    }
    const list = mutes
      .filter(([, id]) => id.trim())
      .map(([scope, id, until]) => ({ scope, id: id.trim(), ...(until ? { until: new Date(until).toISOString() } : {}) }));
    return save("policy", { disturb: { ...doc.disturb, routes, daily_cap: n, mutes: list, ...patch } });
  };

  const capN = Number(cap);
  return (
    <Card title={t.card} onSave={submit}>
      <div>
        {CLASSES.map((c) => (
          <div key={c} className="line">
            <span>{label("deliveryClass", c)}</span>
            <Pick k="route" label={label("deliveryClass", c)} value={base[c] ?? "web"} ids={routeIds(base[c] ?? "web")} onChange={(v) => setBase({ ...base, [c]: v })} />
          </div>
        ))}
        {overrides.map(([cls, prio, route], i) => (
          <div key={i} className="line" role="group" aria-label={t.byPriority + " " + (i + 1)}>
            <span className="sentence">
              <Pick k="deliveryClass" label={t.overrideClass(i + 1)} value={cls} ids={[...CLASSES, ...(CLASSES as readonly string[]).includes(cls) ? [] : [cls]]} onChange={(v) => setOverride(i, 0, v)} />
              <Pick k="priority" label={t.overridePrio(i + 1)} value={prio} ids={(PRIORITY as readonly string[]).includes(prio) ? PRIORITY : [...PRIORITY, prio]} onChange={(v) => setOverride(i, 1, v)} />
            </span>
            <Pick k="route" label={t.overrideRoute(i + 1)} value={route} ids={routeIds(route)} onChange={(v) => setOverride(i, 2, v)} />
            <button type="button" aria-label={t.delOverride(i + 1)} onClick={() => setOverrides(overrides.filter((_, n) => n !== i))}>✕</button>
          </div>
        ))}
        <button type="button" className="link" onClick={() => setOverrides([...overrides, ["decision", "urgent", base.decision ?? "web"]])}>{t.addOverride}</button>
      </div>

      <h3>{t.capHead}</h3>
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

      <h3>{t.mutes}</h3>
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
      <p className="sub">{t.mutesHint}</p>

      <h3>{t.quiet}</h3>
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

      <details>
        <summary>{t.more}</summary>
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
            <Pick k="route" value={base.wake ?? "web"} ids={routeIds(base.wake ?? "web", true)} onChange={(v) => setBase({ ...base, wake: v })} />
          </label>
        </div>
      </details>
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

