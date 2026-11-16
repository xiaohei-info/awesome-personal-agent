import { useCallback, useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText as msg, locale, useLang, useT } from "../../app/i18n";
import { PERM } from "../../app/ids";
import { EntityPick, useNamed, useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { Pick, Val, useLabel } from "../../app/values";
import { Card, PolicyPage, reject, useDraft, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { strings } from "./strings";

// 设置 › Agent（§4.1）：每个 Agent 一张卡，合并钥匙（/settings/tokens）、后台运行声明（/settings/declarations 待确认、
// /settings/commands 已生效）、权限上限（/settings/access-limits 的 permission_cap.by_harness）与长期授权
// （PUT /settings/agents/{harness}/grant 整体替换）。判定都在服务端；本页只搬运选择并以服务端状态为准。
// 每档给 Agent 的工具只取声明里的 headless.tools / tool_notes（附录 A.2），不解析命令行参数。

type Label = { zh: string; en: string };
interface Declaration {
  harness: string;
  name?: Label;
  headless?: { command?: string[]; access?: Record<string, unknown>; tools?: Record<string, Label[]>; tool_notes?: Record<string, Label> };
  main_session?: { wake?: boolean; launch?: string };
}
interface Rec { harness: string; rev: number; state: string; declaration: Declaration }
interface Credential { id: string; kind: string; label?: string; harness?: string; seen_harness?: string; revoked_at?: string }
export interface Grant {
  id: string;
  grantee: { credential?: string; harness?: string; agent?: string; background?: boolean };
  scope: { projects?: string[]; kinds?: string[]; items?: string[]; all?: boolean };
  actions: string[];
  expires_at?: string;
  created_at: string;
  revoked_at?: string;
}

/** 卡片上的六类决定（DESIGN §5.2）：合起来正好是可授权动作全集（settings-agents.test 守住）。 */
export const GRANT_CATS: [string, string[]][] = [
  ["dismiss", ["proposal.dismiss"]],
  ["accept", ["proposal.accept", "proposal.attach"]],
  ["answer", ["decision.answer"]],
  ["items", ["item.create", "item.update", "item.transition", "item.relate"]],
  ["docs", ["revision.confirm", "revision.reject"]],
  ["reports", ["delegate.report", "followmsg.handback"]],
];
const AGENT_KINDS = ["delegate", "worker"];

/** 锚点里的授权单 id：新地址 #grant-<id>，旧地址 /settings/grants#<id> 改写过来时是 #<id>。 */
const grantAnchor = (hash: string) => decodeURIComponent(hash.slice(1)).replace(/^grant-/, "");

const active = (g: Grant, now = Date.now()) => !g.revoked_at && (!g.expires_at || Date.parse(g.expires_at) > now);

interface Data { pending: Rec[]; live: Rec[]; tokens: Credential[]; grants: Grant[] }

function useData() {
  const { api } = useSession();
  const [data, setData] = useState<Data | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    try {
      const [p, c, t, g] = await Promise.all([
        api.get<{ declarations: Rec[] }>("/settings/declarations"),
        api.get<{ declarations: Rec[] }>("/settings/commands"),
        api.get<{ credentials: Credential[] }>("/settings/tokens"),
        api.get<{ grants: Grant[] }>("/settings/grants"),
      ]);
      setData({ pending: p.declarations, live: c.declarations, tokens: t.credentials, grants: g.grants });
    } catch (e) {
      setError(msg(e));
    }
  }, [api]);
  useEffect(() => void load(), [load]);
  return { data, error, setError, load };
}

/** 页面上出现哪些 Agent：有声明的、有归属钥匙的、钥匙最近自报过的。 */
export function agentsOf(d: Data): string[] {
  const live = d.tokens.filter((c) => !c.revoked_at && AGENT_KINDS.includes(c.kind));
  return [...new Set([
    ...d.pending.map((r) => r.harness),
    ...d.live.map((r) => r.harness),
    ...live.flatMap((c) => (c.harness ? [c.harness] : c.seen_harness ? [c.seen_harness] : [])),
  ])].sort();
}

export function SettingsAgentsPage() {
  const t = useT(strings);
  const { docs, error: polError, load: loadPol, save } = usePolicyDocs(["policy", "access-limits"] as const);
  const { data, error, setError, load } = useData();
  const [secret, setSecret] = useState<string | null>(null);
  const { hash } = useLocation();
  const pol = docs?.policy, lim = docs?.["access-limits"];
  const agents = data ? agentsOf(data) : [];
  useEffect(() => {
    if (!data || !hash) return;
    const id = grantAnchor(hash);
    (document.getElementById("grant-" + id) ?? document.getElementById(id))?.scrollIntoView?.();
  }, [data, hash]);
  return (
    <PolicyPage title={t.title} lede={t.lede} error={polError ?? (data ? null : error)} ready={!!pol && !!lim && !!data} onRetry={() => { void loadPol(); void load(); }}>
      {error && data && <div role="alert">{error}</div>}
      {secret && (
        <div role="alert" className="secret">
          <p>{t.secretOnce}</p>
          <code>{secret}</code>
          <div><button onClick={() => setSecret(null)}>{t.savedIt}</button></div>
        </div>
      )}
      {data && pol && lim && (
        <>
          {agents.length === 0 && <p className="sub">{t.none}</p>}
          {agents.map((h) => (
            <AgentCard key={h} h={h} data={data} pol={pol} lim={lim} save={save} reload={load} onError={setError} onSecret={setSecret} />
          ))}
          <Other data={data} agents={agents} reload={load} onError={setError} onSecret={setSecret} />
        </>
      )}
    </PolicyPage>
  );
}

function useName(): (d?: Declaration) => string {
  const lang = useLang();
  const t = useT(strings);
  return (d) => d?.name?.[lang] || t.agent;
}

function AgentCard({ h, data, pol, lim, save, reload, onError, onSecret }: {
  h: string; data: Data; pol: Doc; lim: Doc; save: Save; reload: () => Promise<void>; onError: (m: string | null) => void; onSecret: (s: string) => void;
}) {
  const t = useT(strings);
  const { api } = useSession();
  const raw = useRaw();
  const nameOf = useName();
  const pending = data.pending.find((r) => r.harness === h);
  const live = data.live.find((r) => r.harness === h);
  const name = nameOf(live?.declaration ?? pending?.declaration);
  const keys = data.tokens.filter((c) => c.harness === h);
  const unclaimed = data.tokens.filter((c) => !c.harness && c.seen_harness === h && !c.revoked_at && AGENT_KINDS.includes(c.kind));
  const connected = keys.some((c) => !c.revoked_at);
  const isDefault = pol.default_harness === h;

  const act = async (f: () => Promise<unknown>) => {
    onError(null);
    try {
      await f();
    } catch (e) {
      onError(msg(e));
    }
    await reload();
  };
  const decide = (verb: "confirm" | "reject") => act(() => api.post(`/settings/declarations/${encodeURIComponent(h)}/${pending!.rev}/${verb}`));

  return (
    <section aria-label={name} className="panel stack" id={"agent-" + h}>
      <h2>
        {name} {isDefault && <span className="badge plain">{t.isDefault}</span>}
        {raw && <code className="raw-id">{h}</code>}
      </h2>
      <ul className="plain">
        <li>{connected ? "● " + t.connected : "○ " + t.notConnected}</li>
        {unclaimed.map((c) => (
          <li key={c.id} className="sub">
            {t.claimHint(name)} · <button className="link" onClick={() => act(() => api.patch("/settings/tokens/" + encodeURIComponent(c.id), { harness: h }))}>{t.claim}</button>
          </li>
        ))}
        <li>{pending ? "○ " + t.launch.pending : live ? "● " + t.launch.active : "○ " + t.launch.none}</li>
      </ul>
      {pending && (
        <div>
          <p>{t.wantsToRun(name)}<code>{(pending.declaration.headless?.command ?? []).join(" ")}</code></p>
          <div className="actions">
            <button className="danger" onClick={() => void decide("confirm")}>{t.allow}</button> <button onClick={() => void decide("reject")}>{t.reject}</button>
          </div>
        </div>
      )}
      <label>
        <input type="checkbox" checked={isDefault} onChange={(e) => void save("policy", { default_harness: e.target.checked ? h : "" }).catch((err) => onError(msg(err)))} />
        {t.useDefault}
      </label>
      <Tools h={h} live={live?.declaration} lim={lim} save={save} />
      {live && <Launch h={h} live={live.declaration} pol={pol} save={save} />}
      <Decide h={h} grants={data.grants.filter((g) => g.grantee.agent === h)} reload={reload} />
      <details>
        <summary>{t.advanced}</summary>
        <Keys h={h} keys={keys} reload={reload} onError={onError} onSecret={onSecret} />
        {(live ?? pending) && (
          <details>
            <summary>{t.fullDecl}</summary>
            <pre data-testid={"declaration-" + h}>{JSON.stringify((pending ?? live)!.declaration, null, 2)}</pre>
          </details>
        )}
      </details>
    </section>
  );
}

// 权限上限：写 permission_cap.by_harness[h]，其余 Agent 的与默认值原样带回（PUT 按段替换）。
function Tools({ h, live, lim, save }: { h: string; live?: Declaration; lim: Doc; save: Save }) {
  const t = useT(strings);
  const lang = useLang();
  const label = useLabel();
  const cap = lim.permission_cap as { default: string; by_harness?: Record<string, string> };
  const [tier, setTier] = useDraft(lim.permission_cap, () => cap.by_harness?.[h] ?? cap.default);
  const [asking, setAsking] = useState(false);
  const offered = Object.keys(live?.headless?.access ?? {});
  const tools = (p: string) => (live?.headless?.tools?.[p] ?? []).map((x) => x[lang]);
  const note = (p: string) => live?.headless?.tool_notes?.[p]?.[lang];
  const choose = (p: string) => (p === "full" && tier !== "full" ? setAsking(true) : (setAsking(false), setTier(p)));
  const submit = () => {
    if (!live) return reject(t.allowFirst);
    return save("access-limits", { permission_cap: { default: cap.default, by_harness: { ...(cap.by_harness ?? {}), [h]: tier } } });
  };
  return (
    <Card title={t.tools} hint={live ? undefined : t.allowFirst} onSave={submit}>
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
        live && <p data-testid={"tools-" + h}>{[...tools(tier), note(tier)].filter(Boolean).join(" · ")}</p>
      )}
    </Card>
  );
}

/** 声明能不能在对话里接着做：新字段 wake，或旧声明 launch: wake（§13.6）。 */
export const canWake = (d?: Declaration) => !!(d?.main_session?.wake || d?.main_session?.launch === "wake");

// 以 Agent 本人身份做事时在哪里做：写 main_session_launch.by_harness[h]，按规则的覆盖原样带回（PUT 按段替换）。
// 没选过时显示旧声明留下的初始值，再没有就是单独后台会话；换选择不用重新确认声明。声明生效前不出现。
function Launch({ h, live, pol, save }: { h: string; live: Declaration; pol: Doc; save: Save }) {
  const t = useT(strings);
  const cur = (pol.main_session_launch ?? {}) as { by_harness?: Record<string, string>; by_rule?: Record<string, string> };
  const initial = cur.by_harness?.[h] ?? (live.main_session?.launch === "wake" ? "wake" : "headless");
  const [how, setHow] = useDraft(pol.main_session_launch, () => initial);
  const wake = canWake(live);
  const submit = () => save("policy", { main_session_launch: { by_harness: { ...(cur.by_harness ?? {}), [h]: how }, by_rule: cur.by_rule ?? {} } });
  return (
    <Card title={t.launchTitle} onSave={submit}>
      <fieldset className="seg-radio" role="radiogroup" aria-label={t.launchTitle}>
        {["headless", "wake"].map((v) => (
          <label key={v}>
            <input type="radio" name={"launch-" + h} value={v} checked={how === v} disabled={v === "wake" && !wake} onChange={() => setHow(v)} />
            {t.launchHow[v]}
          </label>
        ))}
      </fieldset>
      {!wake && <p className="sub">{t.noWake}</p>}
    </Card>
  );
}

// Agent 可以替我决定：六类默认都不勾；一类都没勾且没有在用的单时不签单。保存即整体替换（全部取消即只撤销）。
function Decide({ h, grants, reload }: { h: string; grants: Grant[]; reload: () => Promise<void> }) {
  const t = useT(strings);
  const { api } = useSession();
  const projects = useNamed("project");
  const cur = grants.find((g) => active(g));
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
  const submit = async () => {
    const body: Record<string, unknown> = {
      actions: GRANT_CATS.filter(([c]) => cats.includes(c)).flatMap(([, a]) => a),
      scope: all ? { all: true } : { projects: picked },
      background: bg,
    };
    if (until) body.expires_at = new Date(until + "T23:59:59").toISOString();
    await api.put("/settings/agents/" + encodeURIComponent(h) + "/grant", body);
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
      <section aria-label={t.decide} className="panel stack" id={"decide-" + h}>
        <h2>{t.decide}</h2>
        <p className="sub">{t.noGrant}</p>
        {checks}
        <p className="sub">{t.notGrantable}</p>
      </section>
    );
  return (
    <div id={cur ? "grant-" + cur.id : "decide-" + h}>
    <Card title={t.decide} onSave={submit}>
      {checks}
      <fieldset className="checks">
        <legend>{t.where}</legend>
        <label><input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />{t.allProjects}</label>
        {!all && projects.map((p) => (
          <label key={p.id}><input type="checkbox" checked={picked.includes(p.id)} onChange={() => setPicked(flip(picked, p.id))} />{p.name}</label>
        ))}
      </fieldset>
      <label>
        {t.until}
        <input type="date" aria-label={t.until} value={until} onChange={(e) => setUntil(e.target.value)} />
        {!until && <span className="sub"> {t.always}</span>}
      </label>
      <label><input type="checkbox" checked={bg} onChange={(e) => setBg(e.target.checked)} />{t.background}</label>
      <p className="sub" data-testid={"grant-preview-" + h}>{cats.length ? t.preview(what, where, when) : t.previewRevoke}</p>
      {cur && count != null && (
        <p className="sub">
          {t.used(count)} · <button type="button" className="link" onClick={() => api.get<{ events: { seq: number; type: string; object: string; at: string }[] }>("/audit/events?" + new URLSearchParams({ grant: cur.id, limit: "100", order: "desc" })).then((r) => setUsage(r.events), () => setUsage([]))}>{t.showUsage}</button>
        </p>
      )}
      {usage && (usage.length === 0 ? <p className="sub">{t.noUsage}</p> : (
        <ul className="plain" aria-label={t.showUsage}>
          {usage.map((e) => <li key={e.seq}><Val k="event" id={e.type} /> · {e.object} · {new Date(e.at).toLocaleString(locale())}</li>)}
        </ul>
      ))}
      <p className="sub">{t.notGrantable}</p>
    </Card>
    </div>
  );
}

function Keys({ h, keys, reload, onError, onSecret }: { h: string; keys: Credential[]; reload: () => Promise<void>; onError: (m: string | null) => void; onSecret: (s: string) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [kind, setKind] = useState("delegate");
  const [note, setNote] = useState("");
  const issue = async () => {
    onError(null);
    try {
      const body: Record<string, string> = { kind, harness: h };
      if (note.trim()) body.label = note.trim();
      onSecret((await api.post<{ secret: string }>("/settings/tokens", body)).secret);
      setNote("");
    } catch (e) {
      onError(msg(e));
    }
    await reload();
  };
  return (
    <div role="group" aria-label={t.keys}>
      <h3>{t.keys}</h3>
      <KeyList keys={keys} reload={reload} onError={onError} />
      <form className="panel" onSubmit={(e) => { e.preventDefault(); void issue(); }}>
        <label>{t.keyFor} <Pick k="credKind" value={kind} onChange={setKind} ids={AGENT_KINDS} /></label>
        <label>{t.note} <input value={note} onChange={(e) => setNote(e.target.value)} /></label>
        <div><button type="submit">{t.issueKey}</button></div>
      </form>
    </div>
  );
}

function KeyList({ keys, reload, onError }: { keys: Credential[]; reload: () => Promise<void>; onError: (m: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const revoke = async (id: string) => {
    onError(null);
    try {
      await api.del("/settings/tokens/" + encodeURIComponent(id));
    } catch (e) {
      onError(msg(e));
    }
    await reload();
  };
  return (
    <ul className="rows">
      {keys.map((c) => (
        <li key={c.id}>
          <strong>{c.label || c.id}</strong> <span className="badge plain"><Val k="credKind" id={c.kind} /></span>
          {c.revoked_at ? <span className="badge">{t.revoked}</span> : <button className="danger" onClick={() => void revoke(c.id)}>{t.revoke}</button>}
        </li>
      ))}
    </ul>
  );
}

// 页面级的高级：没归属的钥匙、以前按钥匙签的临时授权（锚点 #grant-<id>）、手写登记。
function Other({ data, agents, reload, onError, onSecret }: { data: Data; agents: string[]; reload: () => Promise<void>; onError: (m: string | null) => void; onSecret: (s: string) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { hash } = useLocation();
  const [text, setText] = useState("");
  const [kind, setKind] = useState("observer");
  const [note, setNote] = useState("");
  const [executor, setExecutor] = useState("");
  // 给执行者或其他程序的钥匙（只看、干活的，可绑定执行者）：不属于某个 Agent。
  const issue = async () => {
    onError(null);
    const body: Record<string, string> = { kind };
    if (note.trim()) body.label = note.trim();
    if (kind === "worker" && executor) body.executor = executor;
    try {
      onSecret((await api.post<{ secret: string }>("/settings/tokens", body)).secret);
      setNote("");
      setExecutor("");
    } catch (e) {
      onError(msg(e));
    }
    await reload();
  };
  const orphan = data.tokens.filter((c) => !c.harness && !(c.seen_harness && agents.includes(c.seen_harness)));
  const legacy = data.grants.filter((g) => !g.grantee.agent);
  const register = async () => {
    onError(null);
    let body: unknown;
    try {
      body = JSON.parse(text);
    } catch {
      return onError(t.badJSON);
    }
    try {
      await api.post("/settings/commands", body);
      setText("");
    } catch (e) {
      onError(msg(e));
    }
    await reload();
  };
  return (
    <details className="panel" open={!!hash && legacy.some((g) => g.id === grantAnchor(hash))}>
      <summary>{t.other}</summary>
      <h3>{t.otherKeys}</h3>
      {orphan.length > 0 && <KeyList keys={orphan} reload={reload} onError={onError} />}
      <form className="panel" aria-label={t.otherKeys} onSubmit={(e) => { e.preventDefault(); void issue(); }}>
        <label>{t.keyFor} <Pick k="credKind" value={kind} onChange={setKind} ids={["observer", "worker"]} /></label>
        <label>{t.note} <input value={note} onChange={(e) => setNote(e.target.value)} /></label>
        {kind === "worker" && <label>{t.executor} <EntityPick kind="executor" value={executor} onChange={setExecutor} empty={t.noExecutor} /></label>}
        <div><button type="submit">{t.issueKey}</button></div>
      </form>
      {legacy.length > 0 && (
        <>
          <h3>{t.legacy}</h3>
          <ul className="rows">{legacy.map((g) => <LegacyGrant key={g.id} g={g} reload={reload} onError={onError} />)}</ul>
        </>
      )}
      <form onSubmit={(e) => { e.preventDefault(); void register(); }}>
        <label>{t.declaration}<textarea aria-label={t.declaration} rows={6} value={text} onChange={(e) => setText(e.target.value)} /></label>
        <div><button type="submit" disabled={!text.trim()}>{t.registerBtn}</button></div>
      </form>
    </details>
  );
}

function LegacyGrant({ g, reload, onError }: { g: Grant; reload: () => Promise<void>; onError: (m: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const label = useLabel();
  const cred = useNames("credential");
  const [reason, setReason] = useState("");
  const st = g.revoked_at ? "revoked" : active(g) ? "active" : "expired";
  const revoke = async () => {
    onError(null);
    try {
      await api.post("/settings/grants/" + encodeURIComponent(g.id) + "/revoke", { reason: reason.trim() });
    } catch (e) {
      onError(msg(e));
    }
    await reload();
  };
  return (
    <li id={"grant-" + g.id} data-grant={g.id}>
      <strong>{t.legacyCan(cred(g.grantee.credential ?? ""), g.actions.map((a) => label("grantAction", a)).join(t.listSep))}</strong>{" "}
      <span className="badge">{t.status[st]}</span>
      {g.expires_at && <span className="sub"> {t.untilDate(new Date(g.expires_at).toLocaleDateString(locale()))}</span>}
      {st === "active" && (
        <div className="actions full">
          <label>{t.revokeReason}<input value={reason} onChange={(e) => setReason(e.target.value)} /></label>
          <button className="danger" disabled={!reason.trim()} onClick={() => void revoke()}>{t.revokeGrant}</button>
        </div>
      )}
    </li>
  );
}


/** harness → Agent 的显示名（声明里的 name），供改动页等处称呼 Agent；取不到时称「Agent」。 */
export function useAgentName(): (h?: string) => string {
  const { api } = useSession();
  const nameOf = useName();
  const [decls, setDecls] = useState<Declaration[]>([]);
  useEffect(() => {
    Promise.all([api.get<{ declarations: Rec[] }>("/settings/commands"), api.get<{ declarations: Rec[] }>("/settings/declarations")]).then(
      (rs) => setDecls(rs.flatMap((r) => r.declarations.map((d) => d.declaration))),
      () => setDecls([]),
    );
  }, [api]);
  return (h) => nameOf(decls.find((d) => d.harness === h));
}
