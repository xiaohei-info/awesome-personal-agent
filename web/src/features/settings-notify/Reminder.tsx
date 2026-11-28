import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from "react";
import { useLocation } from "react-router-dom";
import { errorText as msg, useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { useLabel } from "../../app/values";
import { CHANNEL_PREFIX } from "../../app/vocab";
import type { Delivery, Target } from "./matrix";
import { apply, choiceOf, choicesFor, current, kindOf, push, switchOf, targetOf, type Choice, type State } from "./reminders";
import { rstrings } from "./strings";

// 每类提醒唯一可编辑的位置（设计 v2 §3、§4）：一个单选、一句开关后果、一句按当前数据的预览，长解释收在"说明"里。
// 一个页面里的单选共用一个 ReminderScope：同一份 disturb 文档，保存后用服务端规整过的结果替换，免得两处各拿旧文档互相覆盖。
// 写的仍是 PUT /settings/policy 的 disturb 段（整段，已保存的原样带回）与 PATCH /rules/{key}/mode；校验在服务端。

export interface Channels { ids: string[]; names: Record<string, string>; plain: Record<string, string> }

/** 可选的渠道：启用中的内置渠道连接按名字列出，没吊销的渠道扩展密钥按渠道名列出。 */
export function useChannels(onOff: (name: string) => string = (n) => n): Channels {
  const { api } = useSession();
  const [conns, setConns] = useState<{ id: string; name: string; enabled: boolean }[]>([]);
  const [ext, setExt] = useState<string[]>([]);
  useEffect(() => {
    api.get<{ connections: { id: string; name: string; enabled: boolean }[] }>("/connections?direction=channel").then((r) => setConns(Array.isArray(r?.connections) ? r.connections : []), () => setConns([]));
    api.get<{ credentials: { scope?: string; revoked_at?: string }[] }>("/settings/ext-keys").then(
      (r) => setExt([...new Set((Array.isArray(r?.credentials) ? r.credentials : []).filter((c) => !c.revoked_at && c.scope?.startsWith("channel.")).map((c) => c.scope!.slice("channel.".length)))]),
      () => setExt([]),
    );
  }, [api]);
  const names: Record<string, string> = {}, plain: Record<string, string> = {};
  for (const c of conns) { names[CHANNEL_PREFIX + c.id] = c.enabled ? c.name : onOff(c.name); plain[CHANNEL_PREFIX + c.id] = c.name; }
  for (const n of ext) { names[CHANNEL_PREFIX + n] = n; plain[CHANNEL_PREFIX + n] = n; }
  const ids = [...conns.filter((c) => c.enabled).map((c) => CHANNEL_PREFIX + c.id), ...ext.map((n) => CHANNEL_PREFIX + n)];
  return { ids, names, plain };
}

export interface OverviewRow { key: string; produced: boolean; blocked?: string; rules?: string[]; events: number; pushed: number }
export interface Overview {
  since: string;
  rows: OverviewRow[];
  now: { decisions_pending: number; deadlines: number; overdue: number; waiting: number; stale_now: number; stale_from?: string; proposals_new: number; rule_observed: Record<string, number> };
  source_alert: { streak_failures: number; low_frequency_over_sec: number; sources: { id: string; name: string; schedule: string; alert_after: number }[] };
}
export interface Instance { id: string; pack: string; enabled: boolean; params: { title?: string; schedule?: string } }
type Doc = Record<string, any> & { version: number; disturb: Record<string, any> };

interface Scope {
  doc: Doc;
  state: State;
  channels: Channels;
  overview: Overview | null;
  instances: Instance[];
  /** 选了某一项：写 disturb 与（需要时）规则模式。 */
  choose: (key: string, c: Choice) => Promise<void>;
  /** 只改 disturb 里的几个字段（数字参数、到点提醒的折叠设置），其余原样带回。 */
  putDisturb: (fields: Record<string, unknown>) => Promise<number>;
}

const Ctx = createContext<Scope | null>(null);

export function useReminderScope(): Scope {
  const s = useContext(Ctx);
  if (!s) throw new Error("Reminder outside a loaded ReminderScope");
  return s;
}

/** 数据还没取到（或取不到）时为 null：单选不显示，页面其余部分照常。 */
export const useScopeReady = () => useContext(Ctx) !== null;

/** 只在数据就绪时渲染 children（children 里可以放心用 useReminderScope）。 */
export function WhenReady({ children }: { children: ReactNode }) {
  return useScopeReady() ? <>{children}</> : null;
}

/** 一个页面的提醒单选共用的数据。取不到策略时什么都不显示（页面其余部分照常）。
 * 定时安排页自己维护实例列表（新建、改动即时可见），由 instances 传入；其余页面由这里读。 */
export function ReminderScope({ children, instances: given, onSaved }: { children: ReactNode; instances?: Instance[]; onSaved?: () => void }) {
  const { api } = useSession();
  const channels = useChannels();
  const [doc, setDoc] = useState<Doc | null>(null);
  const [modes, setModes] = useState<Record<string, string>>({});
  const [loaded, setInstances] = useState<Instance[]>([]);
  const instances = given ?? loaded;
  const [overview, setOverview] = useState<Overview | null>(null);
  const loadOverview = useCallback(() => {
    api.get<Overview>("/settings/notify/overview").then((o) => setOverview(Array.isArray(o?.rows) && o.now && o.source_alert ? o : null), () => setOverview(null));
  }, [api]);
  useEffect(() => {
    // 只认形状对的响应（旧服务端或别的页面的替身可能答别的）：取不到矩阵时整块不显示
    api.get<Doc>("/settings/policy").then((d) => setDoc(d?.disturb?.delivery?.rows ? d : null), () => setDoc(null));
    api.get<{ rules: { key: string; mode: string }[] }>("/rules").then((r) => setModes(Object.fromEntries((Array.isArray(r?.rules) ? r.rules : []).map((x) => [x.key, x.mode]))), () => setModes({}));
    if (!given) api.get<{ instances: Instance[] }>("/instances").then((r) => setInstances(Array.isArray(r?.instances) ? r.instances : []), () => setInstances([]));
    loadOverview();
  }, [api, loadOverview, !given]); // eslint-disable-line react-hooks/exhaustive-deps
  if (!doc) return <Ctx.Provider value={null}>{children}</Ctx.Provider>;
  const packs = Object.fromEntries(instances.map((i) => [i.id, i.pack]));
  const state: State = { disturb: doc.disturb, modes, packs };
  const putDisturb = async (fields: Record<string, unknown>) => {
    const { delivery: _d, delivery_saved: _s, ...rest } = doc.disturb;
    const next = await api.put<Doc>("/settings/policy", { disturb: { ...rest, ...fields } });
    setDoc(next);
    loadOverview();
    onSaved?.();
    return next.version;
  };
  const choose = async (key: string, c: Choice) => {
    const ch = apply(state, key, c);
    if (ch.mode) {
      const r = await api.patch<{ key: string; mode: string }>(`/rules/${encodeURIComponent(ch.mode.rule)}/mode`, { mode: ch.mode.mode });
      setModes((m) => ({ ...m, [r.key]: r.mode ?? ch.mode!.mode }));
    }
    if (ch.disturb) {
      setDoc(await api.put<Doc>("/settings/policy", { disturb: ch.disturb }));
      onSaved?.(); // 写策略会换版本号：同页另存一份策略文档的地方要重读
    }
    loadOverview();
  };
  return <Ctx.Provider value={{ doc, state, channels, overview, instances, choose, putDisturb }}>{children}</Ctx.Provider>;
}

/** 选"不提醒"时在哪里还能看到。 */
function whereOf(key: string, pack?: string): string {
  switch (kindOf(key)) {
    case "decision":
      return "decision";
    case "instance:":
    case "task_result:":
      return pack === "routine" ? "schedule" : "today";
    case "source_failing":
      return "sources";
    case "run_failed":
    case "delegation_result":
      return "runs";
    case "health":
      return "status";
    case "new_proposals":
      return "proposals";
  }
  return "today";
}

/** 一句按当前数据的预览；没有数据时为空。 */
export function usePreview(key: string): string {
  const t = useT(rstrings);
  const { overview: o, state, doc } = useReminderScope();
  const sched = useSchedulePreview(key === "scheduled" ? doc.disturb.proactive : undefined);
  if (key === "scheduled") return sched;
  if (!o) return "";
  const r = o.rows.find((x) => x.key === key);
  if (r?.blocked) return t.preview.blocked[r.blocked] ?? "";
  const n = o.now;
  switch (kindOf(key)) {
    case "decision":
      return t.preview.decision(r?.events ?? 0, r?.pushed ?? 0, n.decisions_pending);
    case "overdue":
      return t.preview.deadlines(n.deadlines, n.overdue);
    case "followup":
      return t.preview.waiting(n.waiting);
    case "stale": {
      if (n.stale_now) return t.preview.staleNow(n.stale_now);
      if (!n.stale_from) return t.preview.staleNone;
      const d = new Date(n.stale_from);
      return t.preview.staleFrom(d.getMonth() + 1, d.getDate());
    }
    case "new_proposals":
      return t.preview.proposals(n.proposals_new, push(state.disturb, "new_proposals").min ?? 10);
    case "instance:":
    case "health":
      return t.preview.items(r?.events ?? 0);
  }
  return t.preview.times(r?.events ?? 0);
}

/** 到点提醒的预览复用 POST /settings/proactive/preview（按已保存的设置，假设开着）。 */
function useSchedulePreview(p: Record<string, unknown> | undefined): string {
  const t = useT(rstrings);
  const { api } = useSession();
  const [text, setText] = useState("");
  const sig = JSON.stringify(p ?? null);
  useEffect(() => {
    if (!p) return;
    const { since: _s, task_push: _t, instance_push: _i, ...body } = p as Record<string, unknown>;
    api.post<{ total: number; batches: number }>("/settings/proactive/preview", { work_start: "09:00", work_end: "18:00", workdays: [1, 2, 3, 4, 5], shift: true, start: true, end: true, lead_min: 0, dedup_min: 60, max_items: 20, max_batches: 6, ...body }).then(
      (r) => setText(r.total ? t.preview.next24(r.total, r.batches) : t.preview.nextNone),
      () => setText(""),
    );
  }, [api, sig]); // eslint-disable-line react-hooks/exhaustive-deps
  return text;
}

/** 说明：节流规矩的人话、对话收不到时怎样、按优先级的覆盖。 */
function Explain({ k, target }: { k: string; target: Target }) {
  const t = useT(rstrings);
  const { doc, channels } = useReminderScope();
  const d = doc.disturb;
  const r = t.rules;
  const merge = Math.round((d.merge_window_sec ?? 0) / 60);
  const cap = r.cap(d.daily_cap ?? 20);
  const mergeLine = merge ? r.merge(merge) : "";
  const lines: Record<string, string[]> = {
    decision: [d.quiet_exempt ? r.quietSkipExempt : r.quietSkip, cap],
    agent_answered: [r.quietSkip, mergeLine, cap],
    followup: [r.quietSkip, mergeLine, cap],
    rule_reminder: [r.quietSkip, mergeLine, cap],
    scheduled: [r.quietSkip, r.batch],
    overdue: [r.quietSkip, mergeLine, r.oncePerDeadline],
    stale: [r.quietSkip, r.oncePerBatch],
    "instance:": [r.quietSkip, r.oncePerResult],
    "task_result:": [r.quietSkip, r.oncePerResult],
    "instance_failed:": [r.quietHeld, r.hourly],
    "task_failed:": [r.quietHeld, r.hourly],
    source_failing: [r.quietHeld, r.oncePerStreak],
    run_failed: [r.quietHeld, r.hourly],
    delegation_result: [r.quietSkip, r.oncePerResult],
    health: [r.quietSend, r.noCap],
    new_proposals: [r.quietHeld, r.twoHours],
    source_alert: [r.quietSkip, cap, r.oncePerItem],
    source_result: [r.quietSkip, r.oncePerResult],
  };
  const elseText = (v: string) => (v === "next_chat" ? t.opts.chat_next : v === "default" ? t.pushTo(channels.plain[d.delivery?.default_channel] ?? t.noDefault) : t.pushTo(channels.plain[v] ?? t.goneChannel));
  const out = [...(lines[kindOf(k)] ?? []).filter(Boolean)];
  if (target.agent === "main") out.push(target.else ? r.chatElse(elseText(target.else)) : r.chatFallback);
  if (target.agent === "next_chat") out.push(r.nextChatNote);
  const prios = Object.keys(target.by_priority ?? {}).length;
  if (prios) out.push(r.byPrio(prios));
  out.push(r.globals);
  return (
    <details>
      <summary>{t.explain}</summary>
      <ul className="sub">{out.map((l) => <li key={l}>{l}</li>)}</ul>
    </details>
  );
}

/** 一类提醒的单选。label 不给时取文案表；pack 是实例行的定时任务类型（决定"不提醒"时在哪能看到）。 */
export function Reminder(props: { row: string; label?: string; pack?: string; children?: ReactNode }) {
  return useScopeReady() ? <ReminderRow {...props} /> : null;
}

function ReminderRow({ row: key, label, pack, children }: { row: string; label?: string; pack?: string; children?: ReactNode }) {
  const t = useT(rstrings);
  const scope = useReminderScope();
  const { state, channels, choose } = scope;
  const preview = usePreview(key);
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const loc = useLocation();
  const id = "reminder-" + key;
  useEffect(() => {
    if (loc.hash === "#" + id) ref.current?.scrollIntoView?.({ block: "center" });
  }, [loc.hash, id]);
  const m = state.disturb.delivery as Delivery;
  const target = targetOf(m, key, state.packs);
  const cur = current(state, key);
  const title = label ?? t.rows[key] ?? key;
  const def = m.default_channel ? channels.plain[m.default_channel] ?? t.goneChannel : t.noDefault;
  const userName = target.user === "default" || target.user === "off" ? def : channels.plain[target.user] ?? t.goneChannel;
  const optText = (c: Choice) => {
    switch (c) {
      case "phone":
        return target.user !== "off" && target.user !== "default" ? t.pushTo(userName) : t.opts.phone(def);
      case "both":
        return t.opts.both(userName);
      case "off":
        return t.opts.off(t.where[whereOf(key, pack)]);
      default:
        return t.opts[c];
    }
  };
  const sw = switchOf(key);
  const trial = sw?.kind === "rule" && state.modes[sw.rule] === "observe";
  const pick = async (c: Choice) => {
    setBusy(true);
    setNote({ ok: true, text: t.saving });
    try {
      await choose(key, c);
      setNote({ ok: true, text: t.saved });
    } catch (e) {
      setNote({ ok: false, text: msg(e) });
    } finally {
      setBusy(false);
    }
  };
  const noPhone = !m.default_channel && target.user === "off";
  return (
    <div ref={ref} id={id} role="group" aria-label={title} data-reminder={key} className={"stack reminder" + (loc.hash === "#" + id ? " target" : "")}>
      <strong>{title}</strong>
      <fieldset className="seg-radio" role="radiogroup" aria-label={title} disabled={busy}>
        {choicesFor(key, cur).map((c) => (
          <label key={c}>
            <input type="radio" name={id} value={c} checked={cur === c} disabled={c === "phone" && noPhone} onChange={() => void pick(c)} />
            {optText(c)}
          </label>
        ))}
      </fieldset>
      {noPhone && <p className="sub">{t.phoneNeedsDefault}</p>}
      <p className="sub">{t.effect[key] ?? t.effect[kindOf(key)] ?? ""}</p>
      {trial && sw?.kind === "rule" && (
        <p className="sub">
          {t.trial(scope.overview?.now.rule_observed[sw.rule] ?? 0)}{" "}
          <button type="button" className="link" onClick={() => void pick(choiceOf(target) === "off" ? "chat_now" : choiceOf(target))}>{t.makeLive}</button>
        </p>
      )}
      {preview && <p className="sub" role="note" aria-label={t.previewOf(title)}>{preview}</p>}
      {children}
      <Explain k={key} target={target} />
      {note && <span role={note.ok ? "status" : "alert"} className={"saved-note" + (note.ok ? "" : " bad")}>{note.text}</span>}
    </div>
  );
}

/** 实例行的显示名：词表里的类型名，有标题时带上标题。 */
export function useInstanceName(): (i: Instance) => string {
  const label = useLabel();
  return (i) => label("pack", i.pack) + (i.params.title ? " · " + i.params.title : "");
}

/** 定时安排卡上的两个单选：这份结果怎么送、这份没出来时提醒不提醒。没开（实例停用或规则关）时预览写明不会产生。 */
export function InstanceReminders({ i }: { i: Instance }) {
  const t = useT(rstrings);
  const s = useContext(Ctx);
  if (!s) return null;
  return (
    <div className="stack" aria-label={t.result}>
      <Reminder row={"instance:" + i.id} label={t.result} pack={i.pack} />
      <Reminder row={"instance_failed:" + i.id} label={t.failed} pack={i.pack} />
    </div>
  );
}

/** 页面顶部（或底部）一行：自带数据范围的单个单选（来源、Agent、建议、状态页）。 */
export function PageReminder({ row, children, onSaved }: { row: string; children?: ReactNode; onSaved?: () => void }) {
  return (
    <ReminderScope onSaved={onSaved}>
      <section className="panel stack" aria-label={rstrings.zh.rows[row]}>
        <Reminder row={row}>{children}</Reminder>
      </section>
    </ReminderScope>
  );
}

const every = (t: typeof rstrings.zh, sec: number) => (sec % 3600 === 0 ? t.hours(sec / 3600) : t.minutes(Math.round(sec / 60)));
const durSec = (s: string) => {
  let sec = 0;
  for (const [, n, u] of s.matchAll(/(\d+(?:\.\d+)?)(h|m|s)/g)) sec += Number(n) * (u === "h" ? 3600 : u === "m" ? 60 : 1);
  return sec;
};

/** 来源失败的提醒线：依据是内核的常量（GET /settings/notify/overview 的 source_alert），按每个来源的拉取周期列出。 */
export function SourceAlertRule() {
  const t = useT(rstrings);
  const overview = useContext(Ctx)?.overview;
  if (!overview) return null;
  const a = overview.source_alert;
  return (
    <details>
      <summary>{t.alertLine}</summary>
      {t.sourceRule(a.streak_failures, every(t, a.low_frequency_over_sec)).map((l) => <p key={l} className="sub">{l}</p>)}
      <ul className="sub">{a.sources.map((s) => <li key={s.id}>{t.sourceLine(s.name || s.id, every(t, durSec(s.schedule)), s.alert_after)}</li>)}</ul>
    </details>
  );
}

/** 新建议攒到几条推一次（push.new_proposals.min）：只在这里改。 */
export function ProposalsMin() {
  return <WhenReady><MinInput /></WhenReady>;
}

function MinInput() {
  const t = useT(rstrings);
  const { state, putDisturb } = useReminderScope();
  const saved = push(state.disturb, "new_proposals");
  const [v, setV] = useState(String(saved.min ?? 10));
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => setV(String(saved.min ?? 10)), [saved.min]);
  const n = Number(v);
  const save = async () => {
    if (!Number.isInteger(n) || n < 1) return setNote({ ok: false, text: t.minInvalid });
    const all: Record<string, unknown> = {};
    for (const cat of ["source_failing", "template_failed", "run_failed", "new_proposals"]) {
      const r = push(state.disturb, cat);
      all[cat] = { on: r.on, ...(state.disturb.push?.[cat]?.route ? { route: state.disturb.push[cat].route } : {}), ...(cat === "new_proposals" ? { min: n } : {}) };
    }
    try {
      await putDisturb({ push: all });
      setNote({ ok: true, text: t.saved });
    } catch (e) {
      setNote({ ok: false, text: msg(e) });
    }
  };
  return (
    <div className="sentence">
      <label>{t.minLabel} <input type="number" min={1} aria-label={t.minLabel} value={v} onChange={(e) => setV(e.target.value)} /></label>
      <button type="button" disabled={n === (saved.min ?? 10)} onClick={() => void save()}>{t.save}</button>
      {note && <span role={note.ok ? "status" : "alert"} className={"saved-note" + (note.ok ? "" : " bad")}>{note.text}</span>}
    </div>
  );
}
