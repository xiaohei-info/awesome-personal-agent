import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText as msg, useLang, useT } from "../../app/i18n";
import { RULE_MODE } from "../../app/ids";
import { EntityPick, useItemTitle } from "../../app/names";
import { Pick, entryOf } from "../../app/values";
import { CHANNEL_PREFIX } from "../../app/vocab";
import { Card, PolicyPage, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { ProactiveCard } from "./Proactive";
import { GROUPS, OFF, TASK_TYPES, recordOnly, type Delivery, type Target } from "./matrix";
import { strings } from "./strings";

// 设置 › 提醒与推送：策略的 disturb 段（§7.4）。上半是投递矩阵（disturb.delivery，各类提醒送到哪的唯一可编辑处）：
// 每行两层——推给 Agent（不推 / 推到对话里：马上或下次对话时）与推给你（不推 / 默认渠道 / 某个渠道）；产不产生的开关放在对应行里。
// 下半是全局节流：每天上限、暂停、安静时段、合并窗口。每节各自保存，发整段 disturb：已保存的原样带回，只换这一节改过的字段。
// 矩阵一经保存，旧键 routes、push.*.route、proactive.task_push/instance_push 冻结（服务端校验）：本页不改它们，原样带回。
// GET 带的 delivery 在没保存过时是服务端按旧键换算的值；只有矩阵这一节保存时才写 delivery，其余节不带（不提前冻结旧键）。
const ROUTES = ["policy"] as const;
const MERGE_MIN = [0, 15, 30, 60];
const FOLLOWUP_RULE = "follow-up/clock_rules/follow-up-interval";
const STALE_RULE = "follow-up/clock_rules/stale-check";

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

interface Channels { ids: string[]; names: Record<string, string>; plain: Record<string, string> }

/** 可选的渠道：启用中的内置渠道连接按名字列出，没吊销的渠道扩展密钥按渠道名列出。 */
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
  const names: Record<string, string> = {}, plain: Record<string, string> = {};
  for (const c of conns) { names[CHANNEL_PREFIX + c.id] = c.enabled ? t.pushTo(c.name) : t.pushToOff(c.name); plain[CHANNEL_PREFIX + c.id] = c.name; }
  for (const n of ext) { names[CHANNEL_PREFIX + n] = t.pushTo(n); plain[CHANNEL_PREFIX + n] = n; }
  const ids = [...conns.filter((c) => c.enabled).map((c) => CHANNEL_PREFIX + c.id), ...ext.map((n) => CHANNEL_PREFIX + n)];
  return { ids, names, plain };
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
  const t = useT(strings);
  const { delivery: _d, delivery_saved: _s, ...rest } = doc.disturb;
  const put: Put = (fields) => save("policy", { disturb: { ...rest, ...fields } });
  return (
    <>
      <MatrixCard doc={doc} channels={channels} put={put} />
      <h2>{t.throttleHead}</h2>
      <Cap doc={doc} put={put} />
      <Mutes doc={doc} put={put} />
      <Quiet doc={doc} put={put} />
      <More doc={doc} put={put} />
    </>
  );
}

// ---- 投递矩阵 ----

const PUSH = ["source_failing", "template_failed", "run_failed", "new_proposals"] as const;
const PUSH_DEFAULT: Record<string, Rule> = { source_failing: { on: true }, template_failed: { on: true }, run_failed: { on: true }, new_proposals: { on: false, min: 10 } };
interface Rule { on: boolean; route?: string; min?: number }
interface Instance { id: string; pack: string; params: { title?: string; schedule?: string } }
interface RuleState { key: string; mode: string }
const FLAGS = ["remind_on_deadline", "threshold_judge", "followup_defer"] as const;

interface Draft { delivery: Delivery; push: Record<string, Rule>; flags: Record<string, boolean> }

function MatrixCard({ doc, channels, put }: { doc: Doc; channels: Channels; put: Put }) {
  const t = useT(strings);
  const { api } = useSession();
  const savedPush = (doc.disturb.push ?? {}) as Record<string, Rule>;
  const init = (): Draft => ({
    delivery: (doc.disturb.delivery as Delivery | undefined) ?? { default_channel: "", rows: {} },
    push: Object.fromEntries(PUSH.map((c) => [c, { ...PUSH_DEFAULT[c], ...savedPush[c] }])),
    flags: Object.fromEntries(FLAGS.map((k) => [k, !!doc.disturb[k]])),
  });
  const [d, setD, dirty] = useDraft([doc.disturb.delivery, doc.disturb.push, ...FLAGS.map((k) => doc.disturb[k])], init);
  const [instances, setInstances] = useState<Instance[]>([]);
  useEffect(() => {
    api.get<{ instances: Instance[] }>("/instances").then((r) => setInstances(r.instances.filter((i) => (TASK_TYPES as readonly string[]).includes(i.pack))), () => setInstances([]));
  }, [api]);
  const m = d.delivery;
  const setRow = (k: string, v: Target | undefined) => {
    const rows = { ...m.rows };
    if (v) rows[k] = v;
    else delete rows[k];
    setD({ ...d, delivery: { ...m, rows } });
  };
  const setPush = (c: string, r: Partial<Rule>) => setD({ ...d, push: { ...d.push, [c]: { ...d.push[c], ...r } } });
  const setFlag = (k: string, v: boolean) => setD({ ...d, flags: { ...d.flags, [k]: v } });
  const submit = () => {
    const fields: Record<string, unknown> = { delivery: m };
    const i0 = init();
    if (JSON.stringify(d.push) !== JSON.stringify(i0.push)) {
      const out: Record<string, Rule> = {};
      for (const c of PUSH) {
        const r = d.push[c];
        if (c === "new_proposals" && !(Number.isInteger(Number(r.min)) && Number(r.min) >= 1)) return reject(t.pushMinInvalid);
        // route 是冻结键：原样带回已存的值
        out[c] = { on: r.on, ...(savedPush[c]?.route ? { route: savedPush[c].route } : {}), ...(c === "new_proposals" ? { min: Number(r.min) } : {}) };
      }
      fields.push = out;
    }
    for (const k of FLAGS) if (d.flags[k] !== i0.flags[k]) fields[k] = d.flags[k];
    return put(fields);
  };
  const defIds = [...channels.ids, ...(m.default_channel && !channels.ids.includes(m.default_channel) ? [m.default_channel] : [])];
  const defName = m.default_channel ? channels.plain[m.default_channel] ?? t.pushToGone : t.noDefault;
  const kind = (pack: string) => t.taskKinds[pack] ?? pack;
  const row = (k: string, extra?: ReactNode, pack?: string) => (
    <MatrixRow key={k} id={k} label={t.rows[k] ?? k} throttle={t.throttle[k.split(":")[0]] ?? ""} target={m.rows[k] ?? OFF} channels={channels}
      defName={defName} hasDefault={!!m.default_channel} record={recordOnly(k, pack)} onChange={(v) => setRow(k, v)}>{extra}</MatrixRow>
  );
  const extras: Record<string, ReactNode> = {
    decision: (
      <>
        <label><input type="checkbox" checked={d.flags.threshold_judge} onChange={(e) => setFlag("threshold_judge", e.target.checked)} /> {t.thresholdJudge}</label>
        <p className="sub"><Link to="/settings">{t.tierLink}</Link></p>
      </>
    ),
    scheduled: (
      <details>
        <summary>{t.proactive}</summary>
        <ProactiveCard doc={doc} put={put} />
      </details>
    ),
    overdue: <label><input type="checkbox" role="switch" checked={d.flags.remind_on_deadline} onChange={(e) => setFlag("remind_on_deadline", e.target.checked)} /> {t.remindDeadline}</label>,
    followup: (
      <>
        <RuleModes ruleKey={FOLLOWUP_RULE} name={t.rows.followup} />
        <label><input type="checkbox" checked={d.flags.followup_defer} onChange={(e) => setFlag("followup_defer", e.target.checked)} /> {t.followupDefer}</label>
      </>
    ),
    stale: <RuleModes ruleKey={STALE_RULE} name={t.rows.stale} />,
    rule_reminder: <p className="sub"><Link to="/settings/rules">{t.ruleOwned}</Link></p>,
    source_alert: <p className="sub"><Link to="/settings/rules">{t.ruleOwned}</Link></p>,
    source_failing: <PushSwitch label={t.rows.source_failing} on={d.push.source_failing.on} onChange={(on) => setPush("source_failing", { on })} />,
    run_failed: <PushSwitch label={t.rows.run_failed} on={d.push.run_failed.on} onChange={(on) => setPush("run_failed", { on })} />,
    new_proposals: (
      <>
        <PushSwitch label={t.rows.new_proposals} on={d.push.new_proposals.on} onChange={(on) => setPush("new_proposals", { on })} />
        <label>{t.pushMin} <input type="number" min={1} value={d.push.new_proposals.min ?? 10} aria-label={t.pushMin} onChange={(e) => setPush("new_proposals", { min: Number(e.target.value) })} /></label>
      </>
    ),
  };
  return (
    <Card title={t.matrix} hint={t.matrixHint} onSave={submit} dirty={dirty} id="matrix">
      <label>
        {t.defaultChannel}
        <select aria-label={t.defaultChannel} value={m.default_channel} onChange={(e) => setD({ ...d, delivery: { ...m, default_channel: e.target.value } })}>
          <option value="">{t.noDefault}</option>
          {defIds.map((id) => <option key={id} value={id}>{channels.plain[id] ?? t.pushToGone}</option>)}
        </select>
      </label>
      {GROUPS.map((g) => (
        <section key={g.id} aria-label={t.groups[g.id]} className="stack">
          <h3>{t.groups[g.id]}</h3>
          {g.rows.map((k) => (
            <div key={k}>
              {k === "task_failed:brief" && (
                <div className="line"><span>{t.templateOn}</span><PushSwitch label={t.templateOn} on={d.push.template_failed.on} onChange={(on) => setPush("template_failed", { on })} /></div>
              )}
              {row(k, extras[k])}
            </div>
          ))}
          {g.id === "D" && instances.flatMap((i) => {
            const name = i.params.title || i.params.schedule || i.id;
            return ([["instance:", t.instanceRow, "task_result:"], ["instance_failed:", t.instanceFailedRow, "task_failed:"]] as const).map(([prefix, label, type]) => {
              const k = prefix + i.id;
              const own = m.rows[k];
              const typeKey = type + i.pack;
              if (own)
                return (
                  <MatrixRow key={k} id={k} label={label(kind(i.pack), name)} throttle={t.throttle[type.slice(0, -1)]} target={own} channels={channels}
                    defName={defName} hasDefault={!!m.default_channel} record={recordOnly(k, i.pack)} onChange={(v) => setRow(k, v)}>
                    <button type="button" className="link" onClick={() => setRow(k, undefined)}>{t.backToType}</button>
                  </MatrixRow>
                );
              return (
                <div key={k} className="line" role="group" aria-label={label(kind(i.pack), name)}>
                  <span>{label(kind(i.pack), name)}</span>
                  <span className="sub">{t.followType}</span>
                  <button type="button" className="link" onClick={() => setRow(k, { ...(m.rows[typeKey] ?? OFF) })}>{t.ownRow}</button>
                </div>
              );
            });
          })}
        </section>
      ))}
    </Card>
  );
}

function PushSwitch({ label, on, onChange }: { label: string; on: boolean; onChange: (on: boolean) => void }) {
  const t = useT(strings);
  return <label><input type="checkbox" role="switch" checked={on} aria-label={t.pushOn(label)} onChange={(e) => onChange(e.target.checked)} /> {t.produce}</label>;
}

/** 一行的两层去处。改去处保留按优先级的覆盖（by_priority），原样回传。 */
function MatrixRow({ id, label, throttle, target, channels, defName, hasDefault, record, onChange, children }: {
  id: string; label: string; throttle: string; target: Target; channels: Channels; defName: string; hasDefault: boolean; record: boolean;
  onChange: (t: Target) => void; children?: ReactNode;
}) {
  const t = useT(strings);
  const set = (x: Partial<Target>) => {
    const next = { ...target, ...x };
    if (next.agent !== "main") delete next.else;
    onChange(next);
  };
  const chan = (v: string) => (v && v.startsWith(CHANNEL_PREFIX) && !channels.ids.includes(v) ? [v] : []);
  const userIds = ["off", "default", ...channels.ids, ...chan(target.user)];
  const elseIds = ["", "next_chat", ...(hasDefault ? ["default"] : []), ...channels.ids, ...chan(target.else ?? "")];
  const userText = (v: string) => (v === "off" ? t.userOff : v === "default" ? t.userDefault(defName) : channels.names[v] ?? (v.startsWith(CHANNEL_PREFIX) ? t.pushToGone : v));
  const elseText = (v: string) => (v === "" ? t.elseWeb : v === "next_chat" ? t.elseNext : userText(v));
  const agentOn = target.agent !== "off";
  const prios = Object.keys(target.by_priority ?? {}).length;
  return (
    <div role="group" aria-label={label} className="stack" id={"row-" + id}>
      <div className="line">
        <strong>{label}</strong>
        <span className="sentence">
          <select aria-label={t.agentLayer(label)} value={agentOn ? "on" : "off"} onChange={(e) => set({ agent: e.target.value === "on" ? "main" : "off" })}>
            <option value="off">{t.agentOpts.off}</option>
            <option value="on">{t.agentOpts.on}</option>
          </select>
          {agentOn && (
            <select aria-label={t.agentHow(label)} value={target.agent} onChange={(e) => set({ agent: e.target.value })}>
              <option value="main">{t.howOpts.main}</option>
              <option value="next_chat">{t.howOpts.next_chat}</option>
            </select>
          )}
          <select aria-label={t.userLayer(label)} value={target.user} onChange={(e) => set({ user: e.target.value })}>
            {[...new Set(userIds)].map((v) => <option key={v} value={v} disabled={v === "default" && !hasDefault && target.user !== "default"}>{userText(v)}</option>)}
          </select>
        </span>
      </div>
      {target.agent === "main" && (
        <label className="sub">
          {t.elseLayer(label)}
          <select aria-label={t.elseLayer(label)} value={target.else ?? ""} onChange={(e) => set({ else: e.target.value || undefined })}>
            {[...new Set(elseIds)].map((v) => <option key={v} value={v}>{elseText(v)}</option>)}
          </select>
        </label>
      )}
      {target.agent === "next_chat" && <p className="sub">{t.nextNote}</p>}
      {agentOn && target.user !== "off" && <p className="sub" role="note">{t.both}</p>}
      {!agentOn && target.user === "off" && record && <p className="sub">{t.floorRecord}</p>}
      {prios > 0 && <p className="sub">{t.byPrio(prios)}</p>}
      {throttle && <p className="sub">{throttle}</p>}
      {children}
    </div>
  );
}

/** 跟进、停滞两条规则的模式（即改即存，PATCH /rules/{key}/mode）：这两条只在这里切。 */
function RuleModes({ ruleKey, name }: { ruleKey: string; name: string }) {
  const t = useT(strings);
  const lang = useLang();
  const { api } = useSession();
  const [mode, setMode] = useState<string | null>(null);
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => {
    api.get<{ rules: RuleState[] }>("/rules").then((r) => setMode(r.rules.find((x) => x.key === ruleKey)?.mode ?? null), () => setMode(null));
  }, [api, ruleKey]);
  if (mode === null) return null;
  const change = async (m: string) => {
    try {
      const next = await api.patch<RuleState>(`/rules/${encodeURIComponent(ruleKey)}/mode`, { mode: m });
      setMode(next.mode ?? m);
      setNote({ ok: true, text: t.ruleModeSaved });
    } catch (e) {
      setNote({ ok: false, text: msg(e) });
    }
  };
  return (
    <div className="line">
      <span>{t.produce}</span>
      <fieldset className="seg-radio" role="radiogroup" aria-label={t.ruleMode(name)}>
        {RULE_MODE.map((m) => (
          <label key={m}>
            <input type="radio" name={"mode-" + ruleKey} value={m} checked={mode === m} onChange={() => void change(m)} />
            {entryOf(lang, "ruleMode", m)?.label ?? m}
          </label>
        ))}
      </fieldset>
      {note && <span role={note.ok ? "status" : "alert"} className="saved-note">{note.text}</span>}
    </div>
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

// 更多：低优先级提醒的合并窗口。截止后再提醒、两个"让模型判断"的开关在矩阵对应的行里。
function More({ doc, put }: { doc: Doc; put: Put }) {
  const t = useT(strings);
  const { patch, val, set, dirty } = usePatch(doc, ["merge_window_sec"]);
  const mergeMin = Math.round(((val("merge_window_sec") as number) ?? 0) / 60);
  return (
    <details className="panel">
      <summary>{t.more}</summary>
      <Card title={t.more} onSave={() => put(patch)} dirty={dirty}>
        <label>
          {t.merge}
          <select value={mergeMin} onChange={(e) => set("merge_window_sec", Number(e.target.value) * 60)}>
            {[...new Set([...MERGE_MIN, mergeMin])].sort((a, b) => a - b).map((m) => (
              <option key={m} value={m}>{m ? t.minutes(m) : t.mergeOff}</option>
            ))}
          </select>
        </label>
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

