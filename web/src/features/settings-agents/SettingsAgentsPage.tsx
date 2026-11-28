import { useEffect, useState } from "react";
import { Link, Navigate, Route, Routes, useLocation, useParams } from "react-router-dom";
import { PageReminder } from "../settings-notify/Reminder";
import { useSession } from "../../app/session";
import { errorText as msg, useLang, useT } from "../../app/i18n";
import { EntityPick } from "../../app/names";
import { useLabel } from "../../app/values";
import { AgentPage, KeyList, KindChoice, Secret } from "./AgentPage";
import { Onboard } from "./OnboardForm";
import { AutoAct } from "./AutoAct";
import { AudienceChoice, SourceScopeTable, useSources, type Scopes } from "../key-scopes/KeyScopes";
import { RunTimeoutCard } from "./RunTimeout";
import { BackgroundPersonaCard } from "./BackgroundPersona";
import { ThinkingCard } from "./Thinking";
import { PolicyPage, usePolicyDocs } from "../../app/policy-cards";
import { entries, hasKey, ready, useData, type Data, type Entry, type Rec } from "./data";
import { cardOf, PermissionOverview } from "./PermissionOverview";
import type { PermissionCardDoc } from "./Permission";
import { strings } from "./strings";

// 设置 › Agent（界面设计 §4、§6.3）：只管接入与权限。列表上：Agent 一行一个（名字、一句状态加按钮）、接入新的程序、权限总览、替我动手、
// 所有 Agent 共用的后台运行时限、人格、思考强度、折叠的「很少用到」。一个 Agent 的接入、钥匙、权限卡、在哪里做只在它自己的页面
// （/settings/agents/<声明>）改；谁来干活（分工、各处用哪个执行者）在执行者页与各对象自己的页面。判定都在服务端。

export { agentsOf } from "./data";

export function SettingsAgentsPage() {
  return (
    <Routes>
      <Route index element={<AgentList />} />
      <Route path=":id" element={<AgentRoute />} />
    </Routes>
  );
}

const OLD = "legacy:";

/** 执行者统一之前的地址 /settings/agents/legacy:<程序> 落到 /settings/agents/<程序>。 */
function AgentRoute() {
  const { id = "" } = useParams();
  const { hash } = useLocation();
  if (id.startsWith(OLD)) return <Navigate replace to={"/settings/agents/" + encodeURIComponent(id.slice(OLD.length)) + hash} />;
  return <AgentPage />;
}

export function useEntryName(): (e: Entry) => string {
  const lang = useLang();
  const t = useT(strings);
  return (e) => (e.live ?? e.pending)?.declaration.name?.[lang] || e.id || t.agent;
}

function AgentList() {
  const t = useT(strings);
  const { docs, error: polError, load: loadPol, save } = usePolicyDocs(["policy", "access-limits"] as const);
  const { data, error, setError, load } = useData();
  const [secret, setSecret] = useState<string | null>(null);
  const pol = docs?.policy;
  const cards = docs?.["access-limits"]?.agent_cards;
  const list = data ? entries(data) : [];
  const nameOf = useEntryName();

  return (
    <PolicyPage title={t.title} lede={t.lede} error={polError} ready={!!pol} onRetry={() => { void loadPol(); void load(); }}>
      {error && <div role="alert">{error}</div>}
      {pol && (
        <>
          {data && (
            <>
              <section aria-label={t.list} className="panel">
                <h2>{t.list}</h2>
                {list.length === 0 && <p className="sub">{t.emptyCards}</p>}
                <ul className="rows">{list.map((e) => <Row key={e.id} e={e} list={list} cards={cards} />)}</ul>
                <p className="sub">{t.whoWorks} <Link to="/executors">{t.whoWorksLink}</Link></p>
              </section>
              <Onboard taken={list.map((e) => e.id)} reload={load} />
              <PermissionOverview agents={list.map((e) => ({ id: e.id, name: nameOf(e), card: cardOf(cards, e.id) }))} />
            </>
          )}
          <AutoAct pol={pol} save={save} />
          <h2>{t.common}</h2>
          <RunTimeoutCard doc={pol} save={save} />
          <BackgroundPersonaCard doc={pol} save={save} />
          <ThinkingCard doc={pol} save={save} />
          {data && <Other data={data} list={list} reload={load} onError={setError} secret={secret} onSecret={setSecret} />}
          <PageReminder row="run_failed" onSaved={() => void loadPol()} />
          <PageReminder row="delegation_result" onSaved={() => void loadPol()} />
        </>
      )}
    </PolicyPage>
  );
}

/** 列表里的一行：名字、一句状态。可用写「可用」；还差一步时写差哪一步并给「去接入」。隔离另写的声明说明它与哪个程序同一程序。 */
function Row({ e, list, cards }: { e: Entry; list: Entry[]; cards?: Record<string, PermissionCardDoc> }) {
  const t = useT(strings);
  const nameOf = useEntryName();
  const label = useLabel();
  const base = e.base ? list.find((x) => x.id === e.base) : undefined;
  const to = "/settings/agents/" + encodeURIComponent(e.id);
  const step = !hasKey(e) ? t.stepKey : !e.live ? (e.pending ? t.stepLaunch : t.stepNoRequest) : "";
  return (
    <li className="act-row" aria-label={nameOf(e)}>
      <div>
        <span><Link to={to}>{nameOf(e)}</Link></span>
        <span className="sub">{ready(e) ? t.stateReady : t.stateMissing(step)}{cardOf(cards, e.id).groups.decide ? " · " + label("permissionGroup", "decide") : ""}</span>
        {e.base && <span className="sub">{t.isolated(base ? nameOf(base) : e.base)}</span>}
      </div>
      {ready(e) ? <Link className="btn" to={to}>{t.open}</Link> : <Link className="btn" to={to + "#connect"}>{t.goConnect}</Link>}
    </li>
  );
}

// 页面级、很少用到的：给脚本和执行者的钥匙（含没归属的旧钥匙）。
// 是否挪到「对外接口」本版不动（界面设计 §4）。
function Other({ data, list, reload, onError, secret, onSecret }: { data: Data; list: Entry[]; reload: () => Promise<void>; onError: (m: string | null) => void; secret: string | null; onSecret: (s: string | null) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const [kind, setKind] = useState("observer");
  const [note, setNote] = useState("");
  const [executor, setExecutor] = useState("");
  const [audience, setAudience] = useState<string | null>(null); // 不预选（§14.2）
  const [scopes, setScopes] = useState<Scopes>({});
  const sources = useSources();
  const issue = async () => {
    onError(null);
    const body: Record<string, unknown> = { kind, audience };
    if (audience === "external") body.source_scopes = scopes;
    if (note.trim()) body.label = note.trim();
    if (kind === "worker" && executor) body.executor = executor;
    try {
      onSecret((await api.post<{ secret: string }>("/settings/tokens", body)).secret);
      setNote("");
      setExecutor("");
      setScopes({});
    } catch (e) {
      onError(msg(e));
    }
    await reload();
  };
  const harnesses = list.map((e) => e.id);
  const orphan = data.tokens.filter((c) => !c.harness && !(c.seen_harness && harnesses.includes(c.seen_harness)));
  return (
    <details className="panel">
      <summary>{t.rarely}</summary>
      <h3>{t.otherKeys}</h3>
      {orphan.length > 0 && <KeyList keys={orphan} reload={reload} onError={onError} />}
      <form className="panel" aria-label={t.otherKeys} onSubmit={(e) => { e.preventDefault(); void issue(); }}>
        <KindChoice kinds={["observer", "worker"]} value={kind} onChange={setKind} />
        {kind === "worker" && <label>{t.executor} <EntityPick kind="executor" value={executor} onChange={setExecutor} empty={t.noExecutor} /></label>}
        <AudienceChoice value={audience} onChange={setAudience} />
        {audience === "external" && <SourceScopeTable sources={sources} value={scopes} onChange={setScopes} />}
        <label>{t.note} <input value={note} onChange={(e) => setNote(e.target.value)} /></label>
        <div><button type="submit" disabled={!audience}>{t.issueKey}</button></div>
        {secret && <Secret secret={secret} onDone={() => onSecret(null)} />}
      </form>
      <p className="sub">{t.executorsHint} <Link to="/executors">{t.executorsLink}</Link></p>
    </details>
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

