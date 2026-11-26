import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { errorText as msg, useLang, useT } from "../../app/i18n";
import { FIXED_RULE, GRAVE_GROUP, PERM, PERMISSION_GROUP, type PermissionGroup } from "../../app/ids";
import { useNamed } from "../../app/names";
import { useSession } from "../../app/session";
import { entryOf, useLabel } from "../../app/values";
import { Card, useDraft } from "../../app/policy-cards";
import type { Entry } from "./data";
import { permission as strings } from "./strings";

// Agent 页上的权限卡与限制清单（设计 §14.5，草图见方案 2.5）：十组各自「不允许 / 允许」，后果较大的五组折叠、可选有效期，
// 物理删除只能限时；工具档位与「后台运行看哪些消息」也在卡上。改权限卡只有本人能做，保存时服务端可能要求再输口令（api 统一弹框）。
// 卡与清单都读 GET /agents/{id}/limits（判定读的同一份），写 PUT /settings/agents/{id}/card。

export interface PermissionCardDoc {
  groups: Partial<Record<PermissionGroup, { until?: string }>>;
  decide_projects?: string[];
  tool: string;
  runs_all_messages?: boolean;
}
interface Limits {
  card: PermissionCardDoc;
  keys: { id: string; kind: string; audience: string; sources: number }[];
  executor_projects: number;
  run_timeout_sec: number;
  long_runs: number;
  short_runs: number;
  chain_depth: number;
  trusted_only: boolean;
  capture_off: number;
  fixed_rules: string[];
  denied_week: number;
  denied: { at: string; object: string; group: string; code: string; reason: string; exit: number; web_url?: string }[];
}

/** 有效期：从现在起算的到期时刻；forever 为空。 */
function untilOf(d: string): string | undefined {
  const now = new Date();
  if (d === "hour") return new Date(now.getTime() + 3600_000).toISOString();
  if (d === "week") return new Date(now.getTime() + 7 * 86400_000).toISOString();
  if (d === "today") {
    const end = new Date(now);
    end.setHours(23, 59, 59, 0);
    return end.toISOString();
  }
  return undefined;
}

export function useLimits(agent: string) {
  const { api } = useSession();
  const [limits, setLimits] = useState<Limits | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    try {
      setLimits(await api.get<Limits>("/agents/" + encodeURIComponent(agent) + "/limits"));
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, [api, agent]);
  useEffect(() => void load(), [load]);
  return { limits, error, load };
}

export function Permission({ e, version, onSaved }: { e: Entry; version: number; onSaved: () => Promise<void> }) {
  const { limits, error, load } = useLimits(e.id);
  if (error) return <div role="alert">{error}</div>;
  if (!limits) return null;
  return (
    <>
      <PermissionCard e={e} card={limits.card} version={version} onSaved={async () => { await Promise.all([load(), onSaved()]); }} />
      <LimitsList l={limits} />
    </>
  );
}

function PermissionCard({ e, card, version, onSaved }: { e: Entry; card: PermissionCardDoc; version: number; onSaved: () => Promise<void> }) {
  const t = useT(strings);
  const label = useLabel();
  const lang = useLang();
  const { api } = useSession();
  const projects = useNamed("project");
  const [draft, setDraft, dirty] = useDraft(card, () => ({ ...card, groups: { ...card.groups }, decide_projects: card.decide_projects ?? [] }));
  const [pick, setPick] = useState<Partial<Record<PermissionGroup, string>>>({});
  const offered = Object.keys(e.live?.declaration.headless?.access ?? {});
  const tools = (p: string) => (e.live?.declaration.headless?.tools?.[p] ?? []).map((x) => x[lang]);
  const set = (g: PermissionGroup, on: boolean, d?: string) => {
    const groups = { ...draft.groups };
    if (on) {
      const dur = d ?? pick[g] ?? (g === "purge" ? "hour" : "forever");
      groups[g] = untilOf(dur) ? { until: untilOf(dur) } : {};
    } else delete groups[g];
    setDraft({ ...draft, groups });
  };
  const row = (g: PermissionGroup) => {
    const on = !!draft.groups[g];
    const until = draft.groups[g]?.until;
    const grave = GRAVE_GROUP.includes(g);
    return (
      <fieldset key={g} className="choice" aria-label={label("permissionGroup", g)}>
        <legend>{label("permissionGroup", g)}<small>{entryOf(lang, "permissionGroup", g)?.hint}</small></legend>
        <label><input type="radio" name={`perm-${e.id}-${g}`} checked={!on} onChange={() => set(g, false)} />{t.deny}</label>
        <label><input type="radio" name={`perm-${e.id}-${g}`} checked={on} onChange={() => set(g, true)} />{t.allow}</label>
        {grave && (
          <label>
            {t.until}{" "}
            <select aria-label={label("permissionGroup", g) + " · " + t.until} value={pick[g] ?? (g === "purge" ? "hour" : "forever")}
              onChange={(ev) => { setPick({ ...pick, [g]: ev.target.value }); if (on) set(g, true, ev.target.value); }}>
              {["hour", "today", "week", ...(g === "purge" ? [] : ["forever"])].map((d) => <option key={d} value={d}>{t.durations[d]}</option>)}
            </select>
          </label>
        )}
        {until && <span className="sub">{new Date(until) < new Date() ? t.expired : t.untilNow(new Date(until).toLocaleString())}</span>}
      </fieldset>
    );
  };
  const flip = (id: string) => {
    const cur = draft.decide_projects ?? [];
    setDraft({ ...draft, decide_projects: cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id] });
  };
  const submit = async () => {
    await api.put("/settings/agents/" + encodeURIComponent(e.id) + "/card", { ...draft, version });
    await onSaved();
    return 0;
  };
  const everyday = PERMISSION_GROUP.filter((g) => !GRAVE_GROUP.includes(g));
  return (
    <Card title={t.title} hint={t.lede} saveLabel={t.save} onSave={submit} dirty={dirty} id="permissions">
      {everyday.map(row)}
      {draft.groups.decide && (
        <fieldset className="checks">
          <legend>{t.projects}</legend>
          <label><input type="checkbox" checked={!draft.decide_projects?.length} onChange={() => setDraft({ ...draft, decide_projects: [] })} />{t.allProjects}</label>
          {projects.map((p) => (
            <label key={p.id}><input type="checkbox" checked={!!draft.decide_projects?.includes(p.id)} onChange={() => flip(p.id)} />{p.name}</label>
          ))}
        </fieldset>
      )}
      <details>
        <summary>{t.more}</summary>
        {GRAVE_GROUP.map(row)}
      </details>
      <fieldset className="seg-radio" role="radiogroup" aria-label={t.tools}>
        <legend>{t.tools}</legend>
        {PERM.map((p) => (
          <label key={p}>
            <input type="radio" name={"tool-" + e.id} checked={draft.tool === p} disabled={!offered.includes(p)} onChange={() => setDraft({ ...draft, tool: p })} />
            {label("perm", p)}{e.live && !offered.includes(p) ? ` · ${t.notOffered}` : ""}
          </label>
        ))}
      </fieldset>
      <p className="sub">{t.toolsNote}{tools(draft.tool).length ? " " + tools(draft.tool).join(" · ") : ""}</p>
      <fieldset className="seg-radio" role="radiogroup" aria-label={t.messages}>
        <legend>{t.messages}</legend>
        <label><input type="radio" name={"msgs-" + e.id} checked={!draft.runs_all_messages} onChange={() => setDraft({ ...draft, runs_all_messages: false })} />{t.trustedOnly}</label>
        <label><input type="radio" name={"msgs-" + e.id} checked={!!draft.runs_all_messages} onChange={() => setDraft({ ...draft, runs_all_messages: true })} />{t.allMessages}</label>
      </fieldset>
      <p className="sub">{t.ownerOnly} {t.saveNote}</p>
    </Card>
  );
}

function LimitsList({ l }: { l: Limits }) {
  const t = useT(strings);
  const label = useLabel();
  const rows: [string, string, string][] = [
    [t.keys, l.keys.length ? l.keys.map(t.keyLine).join("；") : t.keysNone, "#keys"],
    [t.executors, t.executorsLine(l.executor_projects), "/projects"],
    [t.timeout, t.hours(l.run_timeout_sec), "/settings/agents"],
    [t.runs, t.runsLine(l.long_runs, l.short_runs), "/settings/agents"],
    [t.chain, t.chainLine(l.chain_depth), "/settings/agents"],
    [t.messages, l.trusted_only ? t.trustedOnly : t.allMessages, "#permissions"],
    [t.capture, t.captureLine(l.capture_off), "/projects"],
  ];
  return (
    <section aria-label={t.others} className="panel stack">
      <h2>{t.others} <span className="sub">{t.othersNote}</span></h2>
      <ul className="rows">
        {rows.map(([k, v, to]) => (
          <li key={k} className="act-row"><div><span>{k}</span> <span className="sub">{to.startsWith("#") ? v : <Link to={to}>{v}</Link>}</span></div></li>
        ))}
      </ul>
      <details>
        <summary>{t.fixed}</summary>
        <ul>{FIXED_RULE.filter((r) => l.fixed_rules.includes(r)).map((r) => <li key={r}>{label("fixedRule", r)}</li>)}</ul>
      </details>
      <details>
        <summary>{t.deniedWeek(l.denied_week)} · {t.details}</summary>
        <ul className="rows">
          {l.denied.map((d) => (
            <li key={d.at + d.object}>
              <span>{new Date(d.at).toLocaleString()}</span>{" "}
              <span className="sub">{d.group && PERMISSION_GROUP.includes(d.group as PermissionGroup) ? label("permissionGroup", d.group) : label("rejectReason", d.reason || d.code)}</span>{" "}
              {d.web_url && <Link to={d.web_url}>→</Link>}
            </li>
          ))}
        </ul>
      </details>
    </section>
  );
}
