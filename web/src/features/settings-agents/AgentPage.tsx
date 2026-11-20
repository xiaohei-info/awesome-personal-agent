import { useEffect, useId, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText as msg, locale, useLang, useT } from "../../app/i18n";
import { PERM } from "../../app/ids";
import { useNamed } from "../../app/names";
import { useRaw } from "../../app/raw";
import { Val, useLabel } from "../../app/values";
import { Basics } from "./AgentEditor";
import { Card, PolicyPage, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { active, AGENT_KINDS, entries, hasKey, LEGACY, useData, type Credential, type Declaration, type Entry, type Grant } from "./data";
import { useEntryName } from "./SettingsAgentsPage";
import { strings } from "./strings";

// 设置 › Agent › 某个 Agent（界面设计 §3.2、§4、§6.3）：这个 Agent 的全部属性只在这里改。
// ①接入清单（没完成时置顶，每步就地做完）②基本信息 ③后台权限 ④在哪里做 ⑤可以替我决定 ⑥被谁使用（只读）⑦高级。
// 合并钥匙（/settings/tokens）、启动请求（/settings/declarations 待确认、/settings/commands 已生效）、权限上限（/settings/access-limits）、
// 在哪里做（/settings/policy 的 main_session_launch）与长期授权（PUT /settings/agents/{id}/grant 整体替换）。判定都在服务端。
// 每档给 Agent 的工具只取声明里的 headless.tools / tool_notes（附录 A.2），不解析命令行参数。

/** 可以替我决定的六类（界面设计 §4）：合起来正好是可授权动作全集（settings-agents.test 守住）。 */
export const GRANT_CATS: [string, string[]][] = [
  ["dismiss", ["proposal.dismiss"]],
  ["accept", ["proposal.accept", "proposal.attach"]],
  ["answer", ["decision.answer"]],
  ["items", ["item.create", "item.update", "item.transition", "item.relate", "item.progress"]],
  ["docs", ["revision.confirm", "revision.reject"]],
  ["reports", ["delegate.report", "followmsg.handback"]],
];

export function AgentPage() {
  const t = useT(strings);
  const { id = "" } = useParams();
  const { docs, error: polError, load: loadPol, save } = usePolicyDocs(["policy", "access-limits"] as const);
  const { data, error, setError, load } = useData();
  const nameOf = useEntryName();
  const pol = docs?.policy, lim = docs?.["access-limits"];
  const e = data ? entries(data).find((x) => x.id === id) : undefined;
  const reloadAll = async () => { await load(); await loadPol(); };
  if (data && pol && lim && !e)
    return (
      <main>
        <p className="eyebrow"><Link to="/settings/agents">{t.back}</Link></p>
        <p className="empty">{t.notFound}</p>
      </main>
    );
  const name = e ? nameOf(e) : t.agent;
  return (
    <PolicyPage title={name} error={polError ?? (data ? null : error)} ready={!!pol && !!lim && !!data && !!e} onRetry={() => { void loadPol(); void load(); }}>
      <p className="eyebrow"><Link to="/settings/agents">{t.back}</Link></p>
      {error && data && <div role="alert">{error}</div>}
      {e && pol && lim && (
        <div className="stack">
          <Connect e={e} name={name} reload={load} onError={setError} />
          <RolesLine e={e} pol={pol} />
          {e.instance ? <Basics key={e.card!.rev} card={e.card!} host={e.live?.declaration} onSaved={reloadAll} /> : <LegacyBasics e={e} />}
          <Tools e={e} lim={lim} save={save} />
          {e.live && <Launch e={e} name={name} pol={pol} save={save} />}
          <Decide e={e} reload={load} />
          <UsedBy e={e} pol={pol} />
          <details className="panel">
            <summary>{t.advanced}</summary>
            <Keys e={e} reload={load} onError={setError} />
            {(e.live ?? e.pending) && (
              <details>
                <summary>{t.fullDecl}</summary>
                <pre data-testid={"declaration-" + e.harness}>{JSON.stringify((e.pending ?? e.live)!.declaration, null, 2)}</pre>
              </details>
            )}
          </details>
        </div>
      )}
    </PolicyPage>
  );
}

/** 只显示一次的钥匙明文：出现在触发它的按钮旁，不放到页面顶部（§6.2）。 */
export function Secret({ secret, onDone }: { secret: string; onDone: () => void }) {
  const t = useT(strings);
  return (
    <div role="alert" className="secret">
      <p>{t.secretOnce}</p>
      <code>{secret}</code>
      <div><button type="button" onClick={onDone}>{t.savedIt}</button></div>
    </div>
  );
}

/**
 * 接入清单：①给它一把钥匙（就地生成，明文就在这一行）②允许 APA 在后台启动它（先看到它要运行的命令再允许）。
 * 全部打勾后收成一行；还没收到启动请求时说怎么让它发，而不是一个点不了的状态。
 */
function Connect({ e, name, reload, onError }: { e: Entry; name: string; reload: () => Promise<void>; onError: (m: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [secret, setSecret] = useState<string | null>(null);
  const [viewing, setViewing] = useState(false);
  const key = hasKey(e);
  const left = (key ? 0 : 1) + (e.live ? 0 : 1);
  const act = async (f: () => Promise<unknown>) => {
    onError(null);
    try {
      await f();
    } catch (err) {
      onError(msg(err));
    }
    await reload();
  };
  const issue = () => act(async () => {
    const body: Record<string, string> = { kind: "delegate", harness: e.harness };
    if (e.instance) body.agent_id = e.card!.id;
    setSecret((await api.post<{ secret: string }>("/settings/tokens", body)).secret);
  });
  const decide = (verb: "confirm" | "reject") => act(() => api.post(`/settings/declarations/${encodeURIComponent(e.harness)}/${e.pending!.rev}/${verb}`));
  return (
    <section aria-label={t.connect} id="connect" className="panel stack">
      <h2>{t.connect} <span className="sub">{left ? t.connectLeft(left) : t.connectDone}</span></h2>
      <ol className="rows">
        <li className="act-row">
          <div><span>{key ? "✓ " + t.stepKeyDone : t.stepKey}</span></div>
          {!key && !secret && <button type="button" className="primary" onClick={() => void issue()}>{t.issueKey}</button>}
        </li>
        {secret && <li><Secret secret={secret} onDone={() => setSecret(null)} /></li>}
        {e.unclaimed.map((c) => (
          <li key={c.id} className="act-row">
            <div><span className="sub">{t.claimHint(name)}</span></div>
            <button type="button" onClick={() => void act(() => api.patch("/settings/tokens/" + encodeURIComponent(c.id), { harness: e.harness }))}>{t.claim}</button>
          </li>
        ))}
        <li className="stack">
          <div className="act-row">
            <div><span>{e.live && !e.pending ? "✓ " + t.stepLaunchDone : e.pending ? t.stepLaunch : t.stepNoRequest}</span></div>
            {e.pending && !viewing && <button type="button" className="primary" onClick={() => setViewing(true)}>{t.viewAndAllow}</button>}
          </div>
          {!e.live && !e.pending && (
            <p className="sub">{t.noRequestHow} <code>{t.declareCmd}</code></p>
          )}
          {e.pending && viewing && (
            <div className="stack">
              <WakeChange from={e.live?.declaration} to={e.pending.declaration} />
              <p>{t.wantsToRun(name)}<code>{(e.pending.declaration.headless?.command ?? []).join(" ")}</code></p>
              <div className="actions">
                <button className="danger" onClick={() => void decide("confirm")}>{t.allow}</button> <button onClick={() => void decide("reject")}>{t.reject}</button>
              </div>
            </div>
          )}
        </li>
      </ol>
    </section>
  );
}

/** 分工只在列表页改；这里一行说它现在是不是日常或主 Agent，并给去改的链接。 */
function RolesLine({ e, pol }: { e: Entry; pol: Doc }) {
  const t = useT(strings);
  const role = (k: string) => (pol[k] ?? (pol.default_harness ? LEGACY + pol.default_harness : "")) === e.id;
  const says = [role("default_agent_id") && t.isDailyNow, role("main_agent_id") && t.isMainNow].filter(Boolean).join(t.listSep);
  return (
    <p className="sub">
      {says || null} <Link to="/settings/agents#roles">{says ? t.changeRoles : t.setRoles}</Link>
    </p>
  );
}

/** 旧设置迁移来的 Agent：名字取自程序，不能改名；要另起一份配置就复制成新的 Agent。 */
function LegacyBasics({ e }: { e: Entry }) {
  const t = useT(strings);
  const raw = useRaw();
  return (
    <section aria-label={t.basics} className="panel stack">
      <h2>{t.basics}</h2>
      <p className="sub">{t.legacyBasics}{raw && <code className="raw-id">{e.harness}</code>}</p>
    </section>
  );
}

// 后台权限（权限上限）：全站只有这一个控件。本人建的 Agent 写 permission_cap.by_agent[id]，旧的写 by_harness[程序]；
// 其余 Agent 的与默认值原样带回（PUT 按段替换）。完全放开先看后果再确认。
function Tools({ e, lim, save }: { e: Entry; lim: Doc; save: Save }) {
  const t = useT(strings);
  const lang = useLang();
  const label = useLabel();
  const live = e.live?.declaration;
  const scope = e.instance ? "by_agent" : "by_harness";
  const cap = lim.permission_cap as { by_agent?: Record<string, string>; default: string; by_harness?: Record<string, string> };
  const [tier, setTier, dirty] = useDraft(lim.permission_cap, () => cap[scope]?.[e.key] ?? (e.instance ? "read" : cap.default));
  const [asking, setAsking] = useState(false);
  const offered = Object.keys(live?.headless?.access ?? {});
  const tools = (p: string) => (live?.headless?.tools?.[p] ?? []).map((x) => x[lang]);
  const note = (p: string) => live?.headless?.tool_notes?.[p]?.[lang];
  const choose = (p: string) => (p === "full" && tier !== "full" ? setAsking(true) : (setAsking(false), setTier(p)));
  const submit = () => {
    if (!live) return reject(t.allowFirst);
    return save("access-limits", { permission_cap: { ...cap, [scope]: { ...(cap[scope] ?? {}), [e.key]: tier } } });
  };
  return (
    <Card title={t.tools} hint={live ? undefined : t.allowFirst} onSave={submit} dirty={dirty}>
      <div className="seg" role="group" aria-label={t.tools}>
        {PERM.map((p) => (
          <button key={p} type="button" aria-pressed={tier === p} disabled={!live || !offered.includes(p)} onClick={() => choose(p)}>
            {label("perm", p)}{live && !offered.includes(p) ? ` · ${t.notOffered}` : ""}
          </button>
        ))}
      </div>
      {asking ? (
        <div role="group" aria-label={t.fullPreview}>
          <p>{t.fullPreview}{[...tools("full"), note("full")].filter(Boolean).join(t.listSep)}</p>
          <button type="button" className="danger" onClick={() => { setTier("full"); setAsking(false); }}>{t.confirmFull}</button>{" "}
          <button type="button" onClick={() => setAsking(false)}>{t.cancel}</button>
        </div>
      ) : (
        live && <p data-testid={"tools-" + e.harness}>{[...tools(tier), note(tier)].filter(Boolean).join(" · ")}</p>
      )}
    </Card>
  );
}

/** 声明能不能在对话里接着做：新字段 wake，或旧声明 launch: wake（§13.6）。 */
export const canWake = (d?: Declaration) => !!(d?.main_session?.wake || d?.main_session?.launch === "wake");

/** 待确认的声明相对已生效的，能不能在对话里接着做变了没有；没变不显示。 */
function WakeChange({ from, to }: { from?: Declaration; to: Declaration }) {
  const t = useT(strings);
  const lang = useLang();
  if (canWake(from) === canWake(to)) return null;
  return <p>{canWake(to) ? t.changeWakeOn : t.changeWakeOff(to.main_session?.wake_missing?.[0]?.[lang] ?? "")}</p>;
}

// 在哪里做（以 Agent 本人身份做事时）：单选、可随时改回、不扩大权限，所以即改即存（§6.1）。
// 写 main_session_launch.by_agent[id] 或 by_harness[程序]，按规则的覆盖原样带回（PUT 按段替换）；换选择不用重新确认声明。
function Launch({ e, name, pol, save }: { e: Entry; name: string; pol: Doc; save: Save }) {
  const t = useT(strings);
  const lang = useLang();
  const live = e.live!.declaration;
  const missing = live.main_session?.wake_missing ?? [];
  const scope = e.instance ? "by_agent" : "by_harness";
  const cur = (pol.main_session_launch ?? {}) as { by_agent?: Record<string, string>; by_harness?: Record<string, string>; by_rule?: Record<string, string> };
  const saved = cur[scope]?.[e.key] ?? (!e.instance && live.main_session?.launch === "wake" ? "wake" : "headless");
  const [how, setHow] = useDraft(pol.main_session_launch, () => saved);
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);
  const wake = !e.instance && canWake(live);
  const pick = async (v: string) => {
    setHow(v);
    setNote(null);
    try {
      await save("policy", { main_session_launch: { ...cur, [scope]: { ...(cur[scope] ?? {}), [e.key]: v }, by_rule: cur.by_rule ?? {} } });
      setNote({ ok: true, text: t.saved() });
    } catch (err) {
      setHow(saved); // 失败时控件回到原值，旁边写原因
      setNote({ ok: false, text: msg(err) });
    }
  };
  return (
    <section aria-label={t.launchTitle} className="panel stack">
      <h2>{t.launchTitle}</h2>
      <fieldset className="seg-radio" role="radiogroup" aria-label={t.launchTitle}>
        {["headless", "wake"].map((v) => (
          <label key={v}>
            <input type="radio" name={"launch-" + e.id} value={v} checked={how === v} disabled={v === "wake" && !wake} onChange={() => void pick(v)} />
            {t.launchHow[v]}
          </label>
        ))}
      </fieldset>
      {note && <span role={note.ok ? "status" : "alert"} className={"saved-note" + (note.ok ? "" : " bad")}>{note.text}</span>}
      {!wake && <p className="sub">{missing.length ? t.wakeOff(name, missing[0][lang]) : t.wakeUnsupported(name)}</p>}
      {!wake && missing.length > 0 && (
        <details>
          <summary>{t.howToOpen}</summary>
          <ul>{missing.map((g) => <li key={g.en}>{g.how[lang]}</li>)}</ul>
        </details>
      )}
    </section>
  );
}

// 可以替我决定：扩大权限，所以先看后果再确认（§6.1）。六类默认都不勾；一类都没勾且没有在用的授权时不签。
// 确认即整体替换（全部取消即只撤销）。web_only 类不在可选之列。
function Decide({ e, reload }: { e: Entry; reload: () => Promise<void> }) {
  const t = useT(strings);
  const { api } = useSession();
  const projects = useNamed("project");
  const cur = e.grants.find((g) => active(g));
  const has = (cat: string[]) => !!cur && cat.every((a) => cur.actions.includes(a));
  const [cats, setCats] = useDraft(cur?.id, () => GRANT_CATS.filter(([, a]) => has(a)).map(([c]) => c));
  const [all, setAll] = useDraft(cur?.id, () => !cur || !!cur.scope.all);
  const [picked, setPicked] = useDraft(cur?.id, () => cur?.scope.projects ?? []);
  const [until, setUntil] = useDraft(cur?.id, () => (cur?.expires_at ? cur.expires_at.slice(0, 10) : ""));
  const [bg, setBg] = useDraft(cur?.id, () => cur?.grantee.background ?? true);
  const [usage, setUsage] = useState<{ seq: number; type: string; object: string; at: string }[] | null>(null);
  const [count, setCount] = useState<number | null>(null);
  useEffect(() => {
    setUsage(null);
    if (!cur) return setCount(null);
    api.get<{ events: unknown[] }>("/audit/events?" + new URLSearchParams({ grant: cur.id, limit: "100" })).then((r) => setCount(r.events.length), () => setCount(null));
  }, [api, cur?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  const flip = <T,>(list: T[], x: T) => (list.includes(x) ? list.filter((y) => y !== x) : [...list, x]);
  const what = GRANT_CATS.filter(([c]) => cats.includes(c)).map(([c]) => t.cats[c]).join(t.listSep);
  const where = all ? t.allProjects : t.someProjects(picked.length);
  const when = until ? t.untilDate(until) : t.always;
  const curCats = GRANT_CATS.filter(([, a]) => has(a)).map(([c]) => c);
  const dirty = !!cur
    ? JSON.stringify([cats.slice().sort(), all, picked.slice().sort(), until, bg]) !== JSON.stringify([curCats.slice().sort(), !!cur.scope.all, (cur.scope.projects ?? []).slice().sort(), cur.expires_at ? cur.expires_at.slice(0, 10) : "", cur.grantee.background ?? true])
    : cats.length > 0;
  const submit = async () => {
    const body: Record<string, unknown> = {
      actions: GRANT_CATS.filter(([c]) => cats.includes(c)).flatMap(([, a]) => a),
      scope: all ? { all: true } : { projects: picked },
      grantee: { ...(e.instance ? { instance: true } : {}), background: bg },
    };
    if (until) body.expires_at = new Date(until + "T23:59:59").toISOString();
    await api.put("/settings/agents/" + encodeURIComponent(e.key) + "/grant", body);
    await reload();
    return 0;
  };
  const checks = (
    <fieldset className="checks">
      <legend>{t.decide}</legend>
      {GRANT_CATS.map(([c]) => (
        <label key={c}>
          <input type="checkbox" checked={cats.includes(c)} onChange={() => setCats(flip(cats, c))} />
          {t.cats[c]}
        </label>
      ))}
    </fieldset>
  );
  if (!cur && cats.length === 0)
    return (
      <section aria-label={t.decide} className="panel stack" id={"decide-" + e.key}>
        <h2>{t.decide}</h2>
        <p className="sub">{t.noGrant}</p>
        {checks}
        <p className="sub">{t.notGrantable}</p>
      </section>
    );
  return (
    <Card title={t.decide} onSave={submit} dirty={dirty} saveLabel={t.confirmGrant} id={cur ? "grant-" + cur.id : "decide-" + e.key}>
      {checks}
      <fieldset className="checks">
        <legend>{t.where}</legend>
        <label><input type="checkbox" checked={all} onChange={(ev) => setAll(ev.target.checked)} />{t.allProjects}</label>
        {!all && projects.map((p) => (
          <label key={p.id}><input type="checkbox" checked={picked.includes(p.id)} onChange={() => setPicked(flip(picked, p.id))} />{p.name}</label>
        ))}
      </fieldset>
      <label>
        {t.until}
        <input type="date" aria-label={t.until} value={until} onChange={(ev) => setUntil(ev.target.value)} />
        {!until && <span className="sub"> {t.always}</span>}
      </label>
      <label><input type="checkbox" checked={bg} onChange={(ev) => setBg(ev.target.checked)} />{t.background}</label>
      <p className="sub" data-testid={"grant-preview-" + e.key}>{cats.length ? t.preview(what, where, when) : t.previewRevoke}</p>
      {cur && count != null && (
        <p className="sub">
          {t.used(count)} · <button type="button" className="link" onClick={() => api.get<{ events: { seq: number; type: string; object: string; at: string }[] }>("/audit/events?" + new URLSearchParams({ grant: cur.id, limit: "100", order: "desc" })).then((r) => setUsage(r.events), () => setUsage([]))}>{t.showUsage}</button>
        </p>
      )}
      {usage && (usage.length === 0 ? <p className="sub">{t.noUsage}</p> : (
        <ul className="plain" aria-label={t.showUsage}>
          {usage.map((x) => <li key={x.seq}><Val k="event" id={x.type} /> · {x.object} · {new Date(x.at).toLocaleString(locale())}</li>)}
        </ul>
      ))}
      <p className="sub">{t.notGrantable}</p>
    </Card>
  );
}

/** 被谁使用（只读）：哪些规则、定时安排、事项单独指定了它；改要去那条规则、那个安排、那件事自己的页面。 */
function UsedBy({ e, pol }: { e: Entry; pol: Doc }) {
  const t = useT(strings);
  const { api } = useSession();
  const [counts, setCounts] = useState<{ schedules: number; items: number } | null>(null);
  const rules = Object.values((pol.rule_agent ?? {}) as Record<string, string>).filter((x) => x === e.id).length;
  useEffect(() => {
    let live = true;
    Promise.all([
      api.get<{ instances: { params: { agent_id?: string } }[] }>("/instances").then((r) => r.instances.filter((i) => i.params.agent_id === e.id).length, () => 0),
      // shortcut: 前端从事项列表里筛（接口没有按指定的 Agent 筛选），个人规模够用；事项很多时要服务端筛选
      api.get<{ items: { preferred_agent_id?: string; phase: string }[] }>("/items").then((r) => r.items.filter((i) => i.preferred_agent_id === e.id && !["done", "dropped"].includes(i.phase)).length, () => 0),
    ]).then(([schedules, items]) => live && setCounts({ schedules, items }));
    return () => { live = false; };
  }, [api, e.id]);
  const lines = [
    rules > 0 && <li key="r"><Link to="/rules">{t.usedRules(rules)}</Link></li>,
    !!counts?.schedules && <li key="s"><Link to="/briefs/schedule">{t.usedSchedules(counts.schedules)}</Link></li>,
    !!counts?.items && <li key="i"><Link to="/items">{t.usedItems(counts.items)}</Link></li>,
  ].filter(Boolean);
  return (
    <section aria-label={t.usedBy} className="panel stack">
      <h2>{t.usedBy}</h2>
      {lines.length ? <ul className="plain">{lines}</ul> : counts && <p className="sub">{t.usedNone}</p>}
    </section>
  );
}

function Keys({ e, reload, onError }: { e: Entry; reload: () => Promise<void>; onError: (m: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [kind, setKind] = useState("delegate");
  const [note, setNote] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const issue = async () => {
    onError(null);
    try {
      const body: Record<string, string> = { kind, harness: e.harness };
      if (e.instance) body.agent_id = e.card!.id;
      if (note.trim()) body.label = note.trim();
      setSecret((await api.post<{ secret: string }>("/settings/tokens", body)).secret);
      setNote("");
    } catch (err) {
      onError(msg(err));
    }
    await reload();
  };
  return (
    <div role="group" aria-label={t.keys}>
      <h3>{t.keys}</h3>
      <KeyList keys={e.keys} reload={reload} onError={onError} />
      <form className="panel" onSubmit={(ev) => { ev.preventDefault(); void issue(); }}>
        <KindChoice kinds={AGENT_KINDS} value={kind} onChange={setKind} />
        <label>{t.note} <input value={note} onChange={(ev) => setNote(ev.target.value)} /></label>
        <div><button type="submit">{t.issueKey}</button></div>
        {secret && <Secret secret={secret} onDone={() => setSecret(null)} />}
      </form>
    </div>
  );
}

export function KeyList({ keys, reload, onError }: { keys: Credential[]; reload: () => Promise<void>; onError: (m: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [asking, setAsking] = useState<string | null>(null);
  const revoke = async (id: string) => {
    onError(null);
    setAsking(null);
    try {
      await api.del("/settings/tokens/" + encodeURIComponent(id));
    } catch (err) {
      onError(msg(err));
    }
    await reload();
  };
  return (
    <ul className="rows">
      {keys.map((c) => (
        <li key={c.id}>
          <strong>{c.label || c.id}</strong> <span className="badge plain"><Val k="credKind" id={c.kind} /></span>
          {c.revoked_at ? <span className="badge">{t.revoked}</span> : asking === c.id ? (
            <>{" "}<button className="danger" onClick={() => void revoke(c.id)}>{t.revoke}</button> <button onClick={() => setAsking(null)}>{t.cancel}</button></>
          ) : <button className="danger" onClick={() => setAsking(c.id)}>{t.revoke}</button>}
        </li>
      ))}
    </ul>
  );
}

/** 「给谁」：每种钥匙一个用途名和一句能做什么、不能做什么（§14.2）。 */
export function KindChoice({ kinds, value, onChange }: { kinds: string[]; value: string; onChange: (k: string) => void }) {
  const t = useT(strings);
  const id = useId();
  return (
    <fieldset className="choice">
      <legend>{t.keyFor}</legend>
      {kinds.map((k) => (
        <label key={k}>
          <input type="radio" name={id} checked={value === k} onChange={() => onChange(k)} />
          <span>{t.purpose[k].name}<small>{t.purpose[k].can}</small></span>
        </label>
      ))}
    </fieldset>
  );
}

export type { Grant };
