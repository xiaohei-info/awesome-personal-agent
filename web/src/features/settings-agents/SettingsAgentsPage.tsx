import { useEffect, useState } from "react";
import { Link, Navigate, Route, Routes, useLocation } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText as msg, locale, useLang, useT } from "../../app/i18n";
import { EntityPick, useNames } from "../../app/names";
import { useLabel } from "../../app/values";
import { AddAgent } from "./AgentEditor";
import { AgentPage, KeyList, KindChoice, Secret } from "./AgentPage";
import { Persona } from "./Persona";
import { PolicyPage, usePolicyDocs, type Doc, type Save } from "../settings-policy/shared";
import { active, entries, hasKey, LEGACY, ready, useData, type Data, type Entry, type Grant, type Rec } from "./data";
import { strings } from "./strings";

// 设置 › Agent（界面设计 §4、§6.3）：列表加单页。列表上：分工（日常 Agent、主 Agent）、Agent 一行一个（名字、分工标记、一句状态加按钮）、
// 所有 Agent 共用的语言与语气、折叠的「很少用到」。一个 Agent 的全部属性只在它自己的页面（/settings/agents/<id>）改。
// 判定都在服务端；本页只搬运选择并以服务端状态为准。

export { GRANT_CATS } from "./AgentPage";
export { agentsOf } from "./data";
export type { Grant } from "./data";

/** 锚点里的授权单 id：新地址 #grant-<id>，旧地址 /settings/grants#<id> 改写过来时是 #<id>。 */
export const grantAnchor = (hash: string) => decodeURIComponent(hash.slice(1)).replace(/^grant-/, "");

export function SettingsAgentsPage() {
  return (
    <Routes>
      <Route index element={<AgentList />} />
      <Route path=":id" element={<AgentPage />} />
    </Routes>
  );
}

export function useEntryName(): (e: Entry) => string {
  const lang = useLang();
  const t = useT(strings);
  return (e) => (e.instance ? e.card!.name : e.card?.name || (e.live ?? e.pending)?.declaration.name?.[lang] || e.harness || t.agent);
}

function AgentList() {
  const t = useT(strings);
  const { docs, error: polError, load: loadPol, save } = usePolicyDocs(["policy", "access-limits"] as const);
  const { data, error, setError, load } = useData();
  const { hash } = useLocation();
  const [adding, setAdding] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const pol = docs?.policy;
  const list = data ? entries(data) : [];

  // 旧的授权单链接（/settings/grants#<id>、#grant-<id>）：签给某个 Agent 的落到它的页面；按钥匙签的旧授权在下面「很少用到」里。
  const target = data && hash ? data.grants.find((g) => g.id === grantAnchor(hash)) : undefined;
  if (target?.grantee.agent) {
    const id = target.grantee.instance ? target.grantee.agent : LEGACY + target.grantee.agent;
    return <Navigate replace to={"/settings/agents/" + encodeURIComponent(id) + "#grant-" + target.id} />;
  }

  return (
    <PolicyPage title={t.title} lede={t.lede} error={polError} ready={!!pol} onRetry={() => { void loadPol(); void load(); }}>
      {error && <div role="alert">{error}</div>}
      {pol && (
        <>
          {data && (
            <>
              <Roles pol={pol} save={save} list={list} />
              <section aria-label={t.list} className="panel">
                <h2>{t.list}</h2>
                {list.length === 0 && <p className="sub">{t.emptyCards}</p>}
                <ul className="rows">{list.map((e) => <Row key={e.id} e={e} pol={pol} />)}</ul>
                {!adding && <p><button type="button" onClick={() => setAdding(true)}>{t.add}</button></p>}
              </section>
              {adding && <AddAgent hosts={data.live.map((r) => r.declaration)} taken={list.map((e) => e.harness)} reload={load} onCancel={() => setAdding(false)} />}
            </>
          )}
          <h2>{t.common}</h2>
          <Persona doc={pol} save={save} />
          {data && <Other data={data} list={list} reload={load} onError={setError} secret={secret} onSecret={setSecret} />}
        </>
      )}
    </PolicyPage>
  );
}

/** 分工：日常 Agent、主 Agent，选了就存（§6.1 即改即存），旁边短暂写「已保存」。本人第一次保存前，预选旧的默认并标明。 */
function Roles({ pol, save, list }: { pol: Doc; save: Save; list: Entry[] }) {
  const t = useT(strings);
  const nameOf = useEntryName();
  const legacy = pol.default_agent_id == null && pol.main_agent_id == null && !!pol.default_harness;
  const roles = { default_agent_id: pol.default_agent_id ?? (pol.default_harness ? LEGACY + pol.default_harness : ""), main_agent_id: pol.main_agent_id ?? (pol.default_harness ? LEGACY + pol.default_harness : "") };
  const [note, setNote] = useState<{ role: string; ok: boolean; text: string } | null>(null);
  const pick = async (role: "default_agent_id" | "main_agent_id", id: string) => {
    setNote(null);
    try {
      await save("policy", { ...roles, [role]: id, version: pol.version });
      setNote({ role, ok: true, text: t.saved() });
    } catch (e) {
      setNote({ role, ok: false, text: msg(e) });
    }
  };
  return (
    <section aria-label={t.roles} className="panel stack" id="roles">
      <h2>{t.roles}</h2>
      {(["default_agent_id", "main_agent_id"] as const).map((role) => (
        <div key={role} className="act-row">
          <div>
            <label>{role === "default_agent_id" ? t.dailyRole : t.mainRole}
              <select value={roles[role]} onChange={(e) => void pick(role, e.target.value)}>
                <option value="">{t.roleNone}</option>
                {list.map((e) => <option key={e.id} value={e.id} disabled={e.card ? !e.card.enabled : false}>{nameOf(e)}</option>)}
              </select>
            </label>
            <span className="sub">{!roles[role] ? t.roleNoneHint : legacy ? t.roleLegacy : role === "default_agent_id" ? t.dailyHint : t.mainHint}</span>
          </div>
          {note?.role === role && <span role={note.ok ? "status" : "alert"} className={"saved-note" + (note.ok ? "" : " bad")}>{note.text}</span>}
        </div>
      ))}
    </section>
  );
}

/** 列表里的一行：名字、分工标记、一句状态。可用写「可用」；还差一步时写差哪一步并给「去接入」。 */
function Row({ e, pol }: { e: Entry; pol: Doc }) {
  const t = useT(strings);
  const nameOf = useEntryName();
  const roleId = (k: string) => pol[k] ?? (pol.default_harness ? LEGACY + pol.default_harness : "");
  const to = "/settings/agents/" + encodeURIComponent(e.id);
  const step = !hasKey(e) ? t.stepKey : !e.live ? (e.pending ? t.stepLaunch : t.stepNoRequest) : "";
  return (
    <li className="act-row" aria-label={nameOf(e)}>
      <div>
        <span>
          <Link to={to}>{nameOf(e)}</Link>{" "}
          {roleId("default_agent_id") === e.id && <span className="badge plain">{t.isDaily}</span>}{" "}
          {roleId("main_agent_id") === e.id && <span className="badge plain">{t.isMain}</span>}
        </span>
        <span className="sub">{e.card && !e.card.enabled ? t.stateOff : ready(e) ? t.stateReady : t.stateMissing(step)}</span>
      </div>
      {ready(e) ? <Link className="btn" to={to}>{t.open}</Link> : <Link className="btn" to={to + "#connect"}>{t.goConnect}</Link>}
    </li>
  );
}

// 页面级、很少用到的：给脚本和执行者的钥匙（含没归属的旧钥匙）、以前按钥匙签的临时授权（锚点 #grant-<id>）。
// 是否挪到「对外接口」本版不动（界面设计 §4）。
function Other({ data, list, reload, onError, secret, onSecret }: { data: Data; list: Entry[]; reload: () => Promise<void>; onError: (m: string | null) => void; secret: string | null; onSecret: (s: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const { hash } = useLocation();
  const [kind, setKind] = useState("observer");
  const [note, setNote] = useState("");
  const [executor, setExecutor] = useState("");
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
  const harnesses = list.map((e) => e.harness);
  const orphan = data.tokens.filter((c) => !c.harness && !(c.seen_harness && harnesses.includes(c.seen_harness)));
  const legacy = data.grants.filter((g) => !g.grantee.agent);
  return (
    <details className="panel" open={!!hash && legacy.some((g) => g.id === grantAnchor(hash))}>
      <summary>{t.rarely}</summary>
      <h3>{t.otherKeys}</h3>
      {orphan.length > 0 && <KeyList keys={orphan} reload={reload} onError={onError} />}
      <form className="panel" aria-label={t.otherKeys} onSubmit={(e) => { e.preventDefault(); void issue(); }}>
        <KindChoice kinds={["observer", "worker"]} value={kind} onChange={setKind} />
        {kind === "worker" && <label>{t.executor} <EntityPick kind="executor" value={executor} onChange={setExecutor} empty={t.noExecutor} /></label>}
        <label>{t.note} <input value={note} onChange={(e) => setNote(e.target.value)} /></label>
        <div><button type="submit">{t.issueKey}</button></div>
        {secret && <Secret secret={secret} onDone={() => onSecret(null)} />}
      </form>
      {legacy.length > 0 && (
        <>
          <h3>{t.legacy}</h3>
          <ul className="rows">{legacy.map((g) => <LegacyGrant key={g.id} g={g} reload={reload} onError={onError} />)}</ul>
        </>
      )}
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

/** 程序标识 → Agent 的显示名（声明里的 name），供改动页等处称呼 Agent；取不到时称「Agent」。 */
export function useAgentName(): (h?: string) => string {
  const { api } = useSession();
  const lang = useLang();
  const t = useT(strings);
  const [decls, setDecls] = useState<Rec["declaration"][]>([]);
  useEffect(() => {
    Promise.all([api.get<{ declarations: Rec[] }>("/settings/commands"), api.get<{ declarations: Rec[] }>("/settings/declarations")]).then(
      (rs) => setDecls(rs.flatMap((r) => r.declarations.map((d) => d.declaration))),
      () => setDecls([]),
    );
  }, [api]);
  return (h) => decls.find((d) => d.harness === h)?.name?.[lang] || t.agent;
}

