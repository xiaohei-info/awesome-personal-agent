import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { errorText as msg, useT } from "../../app/i18n";
import { PERMISSION_GROUP } from "../../app/ids";
import { useSession } from "../../app/session";
import { useLabel } from "../../app/values";
import type { AgentView } from "../settings-agents/AgentPick";
import { Reminder, ReminderScope } from "../settings-notify/Reminder";
import { PolicyPage } from "./shared";
import { strings } from "./strings";

// 设置 › 权限总览（路径 /settings，沿用原地址；设计 §14.5）：每个 Agent 的权限卡一览，只看，点名字去它自己的页面改——
// 权限卡只在 Agent 页改。读 GET /agents（每张卡带 permission，判定读的同一份）。「有问题等你决定时」怎么通知你也在这里。
export function SettingsPolicyPage() {
  const t = useT(strings);
  const label = useLabel();
  const { api } = useSession();
  const [view, setView] = useState<AgentView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = () => {
    setError(null);
    api.get<AgentView>("/agents").then(setView, (e) => setError(msg(e)));
  };
  useEffect(load, [api]); // eslint-disable-line react-hooks/exhaustive-deps
  const cell = (until: string | undefined, on: boolean) =>
    !on ? t.denied : until ? (new Date(until) < new Date() ? t.denied : t.allowedUntil(new Date(until).toLocaleString())) : t.allowed;
  return (
    <PolicyPage title={t.title} lede={t.lede} error={error} ready={!!view} onRetry={load}>
      {view && (
        <>
          {view.agents.length === 0 ? (
            <p className="empty">{t.noAgents}</p>
          ) : (
            <section aria-label={t.title} className="panel">
              <table>
                <thead>
                  <tr>
                    <th />
                    {view.agents.map((a) => (
                      <th key={a.id} scope="col"><Link to={"/settings/agents/" + encodeURIComponent(a.id) + "#permissions"}>{a.name || a.harness}</Link></th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {PERMISSION_GROUP.map((g) => (
                    <tr key={g}>
                      <th scope="row">{label("permissionGroup", g)}</th>
                      {view.agents.map((a) => {
                        const gr = a.permission?.groups[g];
                        return <td key={a.id}>{cell(gr?.until, !!gr)}</td>;
                      })}
                    </tr>
                  ))}
                </tbody>
              </table>
              <p className="sub">{t.ownerOnly}</p>
            </section>
          )}
          <ReminderScope>
            <section aria-label={t.notifyCard} className="panel stack" id="notify">
              <h2>{t.notifyCard}</h2>
              <Reminder row="decision" />
              <Reminder row="agent_answered" />
            </section>
          </ReminderScope>
        </>
      )}
    </PolicyPage>
  );
}
