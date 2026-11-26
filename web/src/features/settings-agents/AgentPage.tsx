import { useEffect, useId, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText as msg, useLang, useT } from "../../app/i18n";
import { useRaw } from "../../app/raw";
import { Val } from "../../app/values";
import { Basics } from "./AgentEditor";
import { Permission } from "./Permission";
import { PolicyPage, useDraft, usePolicyDocs, type Doc, type Save } from "../../app/policy-cards";
import { AGENT_KINDS, entries, hasKey, LEGACY, useData, type Credential, type Declaration, type Entry } from "./data";
import { useEntryName } from "./SettingsAgentsPage";
import { strings } from "./strings";
import { AudienceChoice, ScopeEditor, ScopeSummary, SourceScopeTable, useSources, type Scopes } from "../key-scopes/KeyScopes";

// 设置 › Agent › 某个 Agent（界面设计 §3.2、§4、§6.3；设计 §14.3）：这个 Agent 的全部属性只在这里改。
// ①接入清单（没完成时置顶，每步就地做完）②基本信息 ③钥匙（能碰哪些事）④权限卡与限制清单 ⑤在哪里做 ⑥被谁使用（只读）⑦它做过什么 ⑧高级。
// 钥匙（/settings/tokens）只定范围，权限卡定能做哪些组，两者互不影响；判定都在服务端。
// 每档给 Agent 的工具只取声明里的 headless.tools / tool_notes（附录 A.2），不解析命令行参数。

export function AgentPage() {
  const t = useT(strings);
  const { id = "" } = useParams();
  const { docs, error: polError, load: loadPol, save } = usePolicyDocs(["policy"] as const);
  const { data, error, setError, load } = useData();
  const nameOf = useEntryName();
  const pol = docs?.policy;
  const e = data ? entries(data).find((x) => x.id === id) : undefined;
  const reloadAll = async () => { await load(); await loadPol(); };
  if (data && pol && !e)
    return (
      <main>
        <p className="eyebrow"><Link to="/settings/agents">{t.back}</Link></p>
        <p className="empty">{t.notFound}</p>
      </main>
    );
  const name = e ? nameOf(e) : t.agent;
  return (
    <PolicyPage title={name} error={polError ?? (data ? null : error)} ready={!!pol && !!data && !!e} onRetry={() => { void loadPol(); void load(); }}>
      <p className="eyebrow"><Link to="/settings/agents">{t.back}</Link></p>
      {error && data && <div role="alert">{error}</div>}
      {e && pol && (
        <div className="stack">
          <Connect e={e} name={name} reload={load} onError={setError} />
          <RolesLine e={e} pol={pol} />
          {e.instance ? <Basics key={e.card!.rev} card={e.card!} host={e.live?.declaration} onSaved={reloadAll} /> : <LegacyBasics e={e} />}
          <section aria-label={t.keys} className="panel stack" id="keys">
            <h2>{t.keys}</h2>
            <p className="sub">{t.keysScope}</p>
            <Keys e={e} reload={load} onError={setError} />
          </section>
          <Permission e={e} version={Number(pol.version)} onSaved={loadPol} />
          {e.live && <Launch e={e} name={name} pol={pol} save={save} />}
          <UsedBy e={e} pol={pol} />
          <Records e={e} />
          <details className="panel">
            <summary>{t.advanced}</summary>
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
  const [audience, setAudience] = useState<string | null>(null); // 不预选：内部还是外部由本人决定（§14.2）
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
    const body: Record<string, string> = { kind: "delegate", harness: e.harness, audience: audience! };
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
          {!key && !secret && <button type="button" className="primary" disabled={!audience} onClick={() => void issue()}>{t.issueKey}</button>}
        </li>
        {!key && !secret && <li><AudienceChoice value={audience} onChange={setAudience} /></li>}
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

/** 它做过什么：直达诊断里这个 Agent 的改动、规矩改动（已按它筛好），以及运行记录。 */
function Records({ e }: { e: Entry }) {
  const t = useT(strings);
  const q = "?agent=" + encodeURIComponent(e.id);
  return (
    <section aria-label={t.records} className="panel stack">
      <h2>{t.records}</h2>
      <ul className="plain">
        <li><Link to={"/agent-changes" + q}>{t.recChanges}</Link></li>
        <li><Link to={"/rule-changes" + q}>{t.recRules}</Link></li>
        <li><Link to="/runs?kind=agent">{t.recRuns}</Link></li>
      </ul>
    </section>
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
    rules > 0 && <li key="r"><Link to="/settings/agents#auto-action">{t.usedRules(rules)}</Link></li>,
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
  const [audience, setAudience] = useState<string | null>(null);
  const [scopes, setScopes] = useState<Scopes>({});
  const sources = useSources();
  const issue = async () => {
    onError(null);
    try {
      const body: Record<string, unknown> = { kind, harness: e.harness, audience };
      if (audience === "external") body.source_scopes = scopes;
      if (e.instance) body.agent_id = e.card!.id;
      if (note.trim()) body.label = note.trim();
      setSecret((await api.post<{ secret: string }>("/settings/tokens", body)).secret);
      setNote("");
      setScopes({});
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
        <AudienceChoice value={audience} onChange={setAudience} />
        {audience === "external" && <SourceScopeTable sources={sources} value={scopes} onChange={setScopes} />}
        <label>{t.note} <input value={note} onChange={(ev) => setNote(ev.target.value)} /></label>
        <div><button type="submit" disabled={!audience}>{t.issueKey}</button></div>
        {secret && <Secret secret={secret} onDone={() => setSecret(null)} />}
      </form>
    </div>
  );
}

export function KeyList({ keys, reload, onError }: { keys: Credential[]; reload: () => Promise<void>; onError: (m: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [asking, setAsking] = useState<string | null>(null);
  const sources = useSources();
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
          <strong>{c.label || c.id}</strong> <span className="badge plain"><Val k="credKind" id={c.kind} /></span> <ScopeSummary k={c} sources={sources} />{" "}
          <ScopeEditor k={c} group="tokens" canInternal sources={sources} onSaved={reload} />
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

