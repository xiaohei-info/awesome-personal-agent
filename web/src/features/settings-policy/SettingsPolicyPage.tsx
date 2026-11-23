import { useEffect, useState } from "react";
import { errorText as msg, useLang, useT } from "../../app/i18n";
import { AGENT_TIER, type AgentTier } from "../../app/ids";
import { useNamed } from "../../app/names";
import { useSession } from "../../app/session";
import { entryOf, useLabel } from "../../app/values";
import type { Agent, AgentView } from "../settings-agents/AgentPick";
import { Reminder, ReminderScope } from "../settings-notify/Reminder";
import { Card, PolicyPage, useDraft, usePolicyDocs } from "./shared";
import { strings } from "./strings";

// 设置 › 我来拍板（路径 /settings，沿用原地址；设计 §14.3）：每个 Agent 能替你做到哪一档，就在这里改——这是档位唯一可编辑的地方，
// Agent 页只读一行。「有问题等你决定时」怎么通知你也在这里（设计 v2 §4，即改即存，3a；与档位卡分开，不要口令）。档位读 GET /agents（判定读的同一份），写 PUT /settings/agents/{id}/tier；它属 3b，网页可能要求再输一次口令（api 统一弹框）。
export function SettingsPolicyPage() {
  const t = useT(strings);
  const { api } = useSession();
  const { docs, error: polError, load: loadPol } = usePolicyDocs(["policy"] as const);
  const [view, setView] = useState<AgentView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = () => {
    setError(null);
    api.get<AgentView>("/agents").then(setView, (e) => setError(msg(e)));
  };
  useEffect(load, [api]); // eslint-disable-line react-hooks/exhaustive-deps
  const reload = async () => { load(); await loadPol(); };
  const pol = docs?.policy;
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error ?? polError} ready={!!view && !!pol} onRetry={() => void reload()}>
      {view && pol && (
        <>
          {view.agents.length === 0 && <p className="empty">{t.noAgents}</p>}
          {view.agents.map((a) => <TierCard key={a.id} a={a} version={pol.version} reload={reload} />)}
          <ReminderScope>
            <section aria-label={t.notifyCard} className="panel stack" id="notify">
              <h2>{t.notifyCard}</h2>
              <Reminder row="decision" />
              <Reminder row="agent_answered" />
            </section>
          </ReminderScope>
          <section aria-label={t.hardLine} className="panel stack">
            <h2>{t.hardLine}</h2>
            <p className="sub">{t.hardLineWhat} · {t.hardLineHow}</p>
          </section>
        </>
      )}
    </PolicyPage>
  );
}

// 一档整理永远开；二档拍板一个开关，可限定项目；3a 改日常规矩。3b 永不放开，在页底一节说明。
function TierCard({ a, version, reload }: { a: Agent; version: number; reload: () => Promise<void> }) {
  const t = useT(strings);
  const label = useLabel();
  const lang = useLang();
  const { api } = useSession();
  const projects = useNamed("project");
  const cur = (a.tier || "organize") as AgentTier;
  const curProjects = a.tier_projects ?? [];
  const src = [cur, curProjects];
  const [tier, setTier, tierDirty] = useDraft(src, () => cur);
  const [some, setSome, someDirty] = useDraft(src, () => curProjects.length > 0);
  const [picked, setPicked, pickedDirty] = useDraft(src, () => curProjects);
  const flip = (id: string) => setPicked(picked.includes(id) ? picked.filter((x) => x !== id) : [...picked, id]);
  const submit = async () => {
    await api.put("/settings/agents/" + encodeURIComponent(a.id) + "/tier", { tier, projects: tier !== "organize" && some ? picked : [], version });
    await reload();
    return 0;
  };
  const name = a.name || a.harness;
  return (
    <Card title={name} saveLabel={t.save} onSave={submit} dirty={tierDirty || (tier !== "organize" && (someDirty || pickedDirty))} id={"tier-" + a.id}>
      <fieldset className="choice" aria-label={t.tier(name)}>
        {AGENT_TIER.map((v) => (
          <label key={v}>
            <input type="radio" name={"tier-" + a.id} value={v} checked={tier === v} onChange={() => setTier(v)} />
            <span>{label("agentTier", v)}<small>{entryOf(lang, "agentTier", v)?.hint}</small></span>
          </label>
        ))}
      </fieldset>
      {tier !== "organize" && (
        <fieldset className="checks">
          <legend>{t.tierWhere}</legend>
          <label><input type="checkbox" checked={!some} onChange={(ev) => setSome(!ev.target.checked)} />{t.allProjects}</label>
          {some && projects.map((p) => (
            <label key={p.id}><input type="checkbox" checked={picked.includes(p.id)} onChange={() => flip(p.id)} />{p.name}</label>
          ))}
        </fieldset>
      )}
    </Card>
  );
}
