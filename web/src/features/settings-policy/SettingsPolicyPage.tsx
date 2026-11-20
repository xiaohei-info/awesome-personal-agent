import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { errorText as msg, useLang, useT } from "../../app/i18n";
import { AGENT_TIER } from "../../app/ids";
import { useSession } from "../../app/session";
import { entryOf, Val } from "../../app/values";
import type { AgentView } from "../settings-agents/AgentPick";
import { PolicyPage } from "./shared";
import { strings } from "./strings";

// 设置 › 我来拍板（路径 /settings，沿用原地址；设计 §14.3）：只读总览。每个 Agent 现在的档位（GET /agents，判定读的同一份），
// 三档各管什么，以及永不放开的那条硬线。档位只在各自的 Agent 页改；本页不写任何东西。
export function SettingsPolicyPage() {
  const t = useT(strings);
  const lang = useLang();
  const { api } = useSession();
  const [view, setView] = useState<AgentView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = () => {
    setError(null);
    api.get<AgentView>("/agents").then(setView, (e) => setError(msg(e)));
  };
  useEffect(load, [api]); // eslint-disable-line react-hooks/exhaustive-deps
  // 旧设置迁移来的（rev 0）没有自己的钥匙归属，判定恒为只整理（与 Agent 页一致）。
  const tierOf = (a: AgentView["agents"][number]) => (a.rev > 0 && a.tier) || "organize";
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!view} onRetry={load}>
      {view && (
        <>
          <section aria-label={t.agents} className="panel stack">
            <h2>{t.agents}</h2>
            {view.agents.length === 0 && <p className="sub">{t.noAgents}</p>}
            <ul className="rows">
              {view.agents.map((a) => (
                <li key={a.id} className="act-row" aria-label={a.name || a.harness}>
                  <div>
                    <span>{a.name || a.harness}</span>
                    <span className="sub"><Val k="agentTier" id={tierOf(a)} /></span>
                  </div>
                  <Link className="btn" to={"/settings/agents/" + encodeURIComponent(a.id) + "#tier-" + encodeURIComponent(a.id)}>{t.change}</Link>
                </li>
              ))}
            </ul>
          </section>
          <section aria-label={t.tiers} className="panel stack">
            <h2>{t.tiers}</h2>
            <dl>
              {AGENT_TIER.map((v) => (
                <div key={v}>
                  <dt><Val k="agentTier" id={v} /></dt>
                  <dd className="sub">{entryOf(lang, "agentTier", v)?.hint}</dd>
                </div>
              ))}
              <div>
                <dt>{t.hardLine}</dt>
                <dd className="sub">{t.hardLineWhat} · {t.hardLineHow}</dd>
              </div>
            </dl>
          </section>
        </>
      )}
    </PolicyPage>
  );
}
