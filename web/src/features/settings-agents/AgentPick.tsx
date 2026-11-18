import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText, useT } from "../../app/i18n";

import { strings } from "./strings";

export interface Agent {
  id: string; harness: string; name: string; enabled: boolean; rev: number; values?: Record<string, string>;
}
export interface AgentView {
  agents: Agent[];
  selected: { agent_id?: string; agent_name?: string; selection_source?: string; permission_cap?: string };
  reason: string;
}

export function AgentPick({ value, onChange, rule, main, item, inheritRule = true }: {
  value: string; onChange: (id: string) => void; rule?: string; main?: boolean; item?: boolean; inheritRule?: boolean;
}) {
  const { api } = useSession();
  const t = useT(strings);
  const [view, setView] = useState<AgentView>();
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    const query = new URLSearchParams({ runner: main ? "main_session" : "task", rule: inheritRule ? rule ?? "" : "" });
    api.get<AgentView>("/agents?" + query).then(v => { if (active) { setView(v); setError(""); } }, e => { if (active) setError(errorText(e)); });
    return () => { active = false; };
  }, [api, rule, main, inheritRule]);
  if (!view) return error ? <small className="sub">{error}</small> : null;
  const source = view?.selected?.selection_source === "rule" ? t.cardUseRule : main ? t.cardUseMainAgent : t.cardUseDefault;
  return <label>{item ? t.cardAgentForThisItem : t.cardUseAgent}
    <select value={value} onChange={e => onChange(e.target.value)}>
      <option value="">{source}（{view.reason || view.selected?.agent_name || t.cardNotSet}）</option>
      {value && !view.agents.some(a => a.id === value) && <option value={value} disabled>{value}</option>}
      {view.agents.map(a => <option key={a.id} value={a.id} disabled={!a.enabled}>{a.name}{!a.enabled && t.cardDisabled}</option>)}
    </select>
  </label>;
}
