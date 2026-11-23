import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { locale, useLang, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useItemTitle } from "../../app/names";
import { useRaw } from "../../app/raw";
import { entryOf } from "../../app/values";
import { RULE_MODE } from "../../app/ids";
import { strings } from "./strings";

// 设置 › 自动规则（界面设计 §3.1、§6.1；设计 v2 §4）：上面「提醒我」一组是提醒类的单选（到点、截止已过、跟进、14 天没动静），
// 下面只列动作类与其他规则；提醒类规则（class: reminder）的模式由「提醒我」里的单选写（不提醒即关），不在列表里重复。
// 规则自带提醒（create_timer、then: alert、notify_urgent）时，那一行提醒挂在那条规则下面。
// 模式、用哪个 Agent、在哪里做都是即改即存，旁边写「已保存」；用哪个 Agent 默认收起成一行。
// 形状对应 GET /v1/rules（含 class）、PATCH /v1/rules/{id}/mode（附录 B.2）、GET /v1/packs 与 GET /v1/audit/events（§14.8）。
// 谁能切、切到什么、观察记录怎么产生都由服务端判定；本页只搬运选择，被拒绝时模式保持原值。只改模式，不编辑规则内容。
// 以 Agent 本人身份运行的规则（runner=main_session）另可覆盖 Agent 在哪里做：写 /settings/policy 的 main_session_launch.by_rule，
// 按 Agent 的设置原样带回；没覆盖就沿用 Agent 设置（§13.6）。
import { AgentPick } from "../settings-agents/AgentPick";
import { Reminder, ReminderScope, WhenReady, useReminderScope } from "../settings-notify/Reminder";
import { ProactiveCard } from "../settings-notify/Proactive";

interface Rule { key: string; pack: string; group: string; id: string; mode: string; class?: "reminder" | "action"; runner?: string }
interface Launch { by_harness?: Record<string, string>; by_rule?: Record<string, string> }
interface Pack { name: string; description?: string }
interface Observed { seq: number; object: string; at: string; reason?: string; payload?: { would_do?: unknown } }

const WEEK = 7 * 24 * 3600 * 1000;
const observed = (key: string) => "/audit/events?" + new URLSearchParams({ type: "rule.observed", actor: "rule:" + key, limit: "500" });

function Line({ e }: { e: Observed }) {
  const t = useT(strings);
  const raw = useRaw();
  const title = useItemTitle(e.object);
  return (
    <li>
      {t.observed(title, new Date(e.at).toLocaleString(locale()))}
      {raw && <code className="raw-id">{`#${e.seq} ${e.reason ?? ""} ${JSON.stringify(e.payload?.would_do ?? null)}`}</code>}
    </li>
  );
}

/** 试运行的规则：本周本来会做几次，点开是一句话列表。 */
function Trial({ rule, name }: { rule: Rule; name: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [events, setEvents] = useState<Observed[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [open, setOpen] = useState(false);
  useEffect(() => {
    api.get<{ events: Observed[] }>(observed(rule.key)).then((r) => setEvents(r.events), setError);
  }, [api, rule.key]);
  if (error != null) return <ErrorNote error={error} />;
  if (!events) return null;
  const week = events.filter((e) => Date.now() - new Date(e.at).getTime() < WEEK);
  return (
    <>
      <button type="button" className="quiet" aria-expanded={open} disabled={!week.length} onClick={() => setOpen(!open)}>
        {open ? t.hide : t.week(week.length)}
      </button>
      {open && (
        <ul aria-label={t.observations(name)} className="sub-list">
          {[...week].reverse().map((e) => <Line key={e.seq} e={e} />)}
        </ul>
      )}
    </>
  );
}

function Modes({ id, name, value, onChange }: { id: string; name: string; value: string; onChange: (m: string) => void }) {
  const lang = useLang();
  const t = useT(strings);
  return (
    <fieldset className="seg-radio" role="radiogroup" aria-label={t.mode(name)}>
      {RULE_MODE.map((m) => (
        <label key={m}>
          <input type="radio" name={"mode-" + id} value={m} checked={value === m} onChange={() => onChange(m)} />
          {entryOf(lang, "ruleMode", m)?.label ?? m}
        </label>
      ))}
    </fieldset>
  );
}

function LaunchPick({ name, value, onChange }: { name: string; value: string; onChange: (v: string) => void }) {
  const t = useT(strings);
  return (
    <select aria-label={t.launchOf(name)} value={value || "inherit"} onChange={(e) => onChange(e.target.value === "inherit" ? "" : e.target.value)}>
      {["inherit", "headless", "wake"].map((v) => <option key={v} value={v}>{t.launchHow[v]}</option>)}
    </select>
  );
}

export function RulesPage() {
  const { api } = useSession();
  const lang = useLang();
  const raw = useRaw();
  const t = useT(strings);
  const [rules, setRules] = useState<Rule[] | null>(null);
  const [packs, setPacks] = useState<Pack[]>([]);
  const [error, setError] = useState<unknown>(null);

  const load = useCallback(async () => {
    try {
      setRules((await api.get<{ rules: Rule[] }>("/rules")).rules);
      setError(null);
    } catch (e) {
      setRules(null);
      setError(e);
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => { api.get<{ packs: Pack[] }>("/packs").then((r) => setPacks(r.packs), () => setPacks([])); }, [api]);
  const [ruleAgents, setRuleAgents] = useState<Record<string,string>>({});
  const [version, setVersion] = useState<number>();
  const [launch, setLaunch] = useState<Launch | null>(null);
  const [saved, setSaved] = useState<string | null>(null); // 即改即存：刚存好的那条规则旁短暂写「已保存」
  useEffect(() => { api.get<{ version:number; rule_agent?:Record<string,string>; main_session_launch?: Launch }>("/settings/policy").then((p) => {setRuleAgents(p.rule_agent ?? {}); setVersion(p.version); setLaunch(p.main_session_launch ?? {});}, () => setLaunch(null)); }, [api]);

  async function setAgent(key: string, id: string) {
    const next = {...ruleAgents}; if (id) next[key] = id; else delete next[key];
    try {const p = await api.put<{rule_agent: Record<string,string>; version:number}>("/settings/policy", {rule_agent:next, version}); setRuleAgents(p.rule_agent ?? {});setVersion(p.version);setSaved(key);} catch(e) {setError(e);}
  }
  async function setRuleLaunch(r: Rule, v: string) {
    const by_rule = { ...(launch?.by_rule ?? {}) };
    if (v) by_rule[r.key] = v;
    else delete by_rule[r.key];
    try {
      const next = await api.put<{ main_session_launch: Launch; version:number }>("/settings/policy", { main_session_launch: { ...launch, by_harness: launch?.by_harness ?? {}, by_rule } });
      setLaunch(next.main_session_launch);setVersion(next.version);
      setError(null);
      setSaved(r.key);
    } catch (e) {
      setError(e); // 选择框的值仍是服务端未改动的原值
    }
  }

  async function setMode(r: Rule, mode: string) {
    try {
      const next = await api.patch<Rule>(`/rules/${encodeURIComponent(r.key)}/mode`, { mode });
      setRules((l) => l!.map((x) => (x.key === r.key ? { ...x, ...next } : x)));
      setError(null);
      setSaved(r.key);
    } catch (e) {
      setError(e); // 单选的值仍是 r.mode，即服务端未改动的原模式
    }
  }
  // 规则名：词表按「包/规则 id」查；查不到时用包的说明。
  const nameOf = (r: Rule) => entryOf(lang, "rule", `${r.pack}/${r.id}`)?.label ?? packs.find((p) => p.name === r.pack)?.description ?? r.id;
  const groupOf = (r: Rule) => r.class ?? "other";
  const groups = (["action", "other"] as const)
    .map((g) => ({ g, list: (rules ?? []).filter((r) => groupOf(r) === g) }))
    .filter((x) => x.list.length);

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error != null && <ErrorNote error={error} />}
      {!rules && error == null && <p role="status">{t.loading}</p>}
      {rules && !groups.length && <p className="empty">{t.none}</p>}
      <ReminderScope>
      <WhenReady><RemindMe /></WhenReady>
      {groups.map(({ g, list }) => (
        <section key={g} aria-label={t.groups[g]} className="panel">
          <h2>{t.groups[g]}</h2>
          <ul className="rows">
            {list.map((r) => {
              const name = nameOf(r);
              return (
                <li key={r.key} className="rule" id={"rule-" + r.key}>
                  <div>
                    <strong>{name}</strong>
                    <p className="sub">
                      {entryOf(lang, "ruleGroup", r.group)?.label ?? r.group}
                      {raw && <code className="raw-id">{r.key}</code>}
                    </p>
                    {r.mode === "observe" && <Trial rule={r} name={name} />}
                    {r.runner && <AgentPick collapsed inheritRule={false} main={r.runner === "main_session"} value={ruleAgents[r.key] ?? ""} onChange={id => void setAgent(r.key,id)} />}
                    {r.runner === "main_session" && launch && <LaunchPick name={name} value={launch.by_rule?.[r.key] ?? ""} onChange={(v) => void setRuleLaunch(r, v)} />}
                    <WhenReady><RuleReminder rule={r.key} /></WhenReady>
                  </div>
                  <div>
                    <Modes id={r.key} name={name} value={r.mode} onChange={(m) => void setMode(r, m)} />
                    {saved === r.key && <span role="status" className="saved-note">{t.saved()}</span>}
                  </div>
                </li>
              );
            })}
          </ul>
          {(g === "action" || g === "other") && <p className="sub">{t.hint}</p>}
        </section>
      ))}
      </ReminderScope>
    </main>
  );
}

/** 「提醒我」：提醒类的四个单选；到点提醒的时间规则收在"什么时候提醒"里（开关就是单选本身）。 */
function RemindMe() {
  const t = useT(strings);
  const { doc, putDisturb } = useReminderScope();
  return (
    <section aria-label={t.remindMe} className="panel stack" id="remind-me">
      <h2>{t.remindMe}</h2>
      <Reminder row="scheduled">
        <details>
          <summary>{t.when}</summary>
          <ProactiveCard doc={doc} put={putDisturb} />
        </details>
      </Reminder>
      <Reminder row="overdue" />
      <Reminder row="followup" />
      <Reminder row="stale" />
    </section>
  );
}

/** 规则自带的提醒（规则设的提醒、来源即时提醒）挂在产生它的那条规则下面；线上没有这种规则时什么都不显示。 */
function RuleReminder({ rule }: { rule: string }) {
  const { overview } = useReminderScope();
  const row = overview?.rows.find((x) => (x.key === "rule_reminder" || x.key === "source_alert") && x.rules?.includes(rule));
  return row ? <Reminder row={row.key} /> : null;
}
