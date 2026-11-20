import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText, useT } from "../../app/i18n";
import { Go } from "../../app/page";
import { useRaw } from "../../app/raw";

import { strings } from "./strings";

export interface Agent {
  id: string; harness: string; name: string; enabled: boolean; rev: number; values?: Record<string, string>;
  /** 档位与二档的项目范围（GET /agents，设计 §14.3）；缺省是只整理。 */
  tier?: string; tier_projects?: string[];
}
export interface AgentView {
  agents: Agent[];
  selected: { agent_id?: string; agent_name?: string; selection_source?: string; permission_cap?: string };
  reason: string;
}

/**
 * 「用哪个 Agent」选择器（界面设计 §3.2 公共件，全站只有这一份）：写的是所在对象自己的属性（事项、规则、安排），
 * 显示「现在会用谁、因为什么」——不指定时是「跟随默认（现在是某某）」。选了某个 Agent 时旁边给「去看这个 Agent」。
 * collapsed 为真时默认只显示一行「由某某处理」，点「改」才展开（规则页用）。生效顺序由服务端判定（§13.6），这里只取预览。
 */
export function AgentPick({ value, onChange, rule, main, item, inheritRule = true, collapsed, label }: {
  value: string; onChange: (id: string) => void; rule?: string; main?: boolean; item?: boolean; inheritRule?: boolean; collapsed?: boolean; label?: string;
}) {
  const { api } = useSession();
  const t = useT(strings);
  const raw = useRaw();
  const [view, setView] = useState<AgentView>();
  const [error, setError] = useState("");
  const [open, setOpen] = useState(!collapsed);
  useEffect(() => {
    let active = true;
    const query = new URLSearchParams({ runner: main ? "main_session" : "task", rule: inheritRule ? rule ?? "" : "" });
    api.get<AgentView>("/agents?" + query).then(v => { if (active) { setView(v); setError(""); } }, e => { if (active) setError(errorText(e)); });
    return () => { active = false; };
  }, [api, rule, main, inheritRule]);
  if (!view) return error ? <small className="sub">{error}</small> : null;
  const fallback = view.selected?.agent_id ? view.selected.agent_name || view.selected.agent_id : "";
  const follow = view.selected?.selection_source === "rule" && inheritRule ? t.pickFollowRule : main ? t.pickFollowMain : t.pickFollowDefault;
  const chosen = view.agents.find((a) => a.id === value);
  const now = value ? chosen?.name ?? value : fallback;
  if (!open)
    return (
      <p className="sub">
        {now ? t.pickHandledBy(now) : t.pickNone}{" "}
        <button type="button" className="link" onClick={() => setOpen(true)}>{t.pickChange}</button>
      </p>
    );
  return (
    <span className="actions">
      <label>{label ?? (item ? t.pickForItem : t.pickLabel)}
        <select value={value} onChange={e => onChange(e.target.value)}>
          <option value="">{fallback ? t.pickFollow(follow, fallback) : t.pickFollowNone(follow)}</option>
          {value && !chosen && <option value={value} disabled>{value}</option>}
          {view.agents.map(a => <option key={a.id} value={a.id} disabled={!a.enabled}>{a.name}{!a.enabled && t.cardDisabled}</option>)}
        </select>
      </label>
      {(value || view.selected?.agent_id) && <Go className="quiet" to={"/settings/agents/" + encodeURIComponent(value || view.selected.agent_id!)}>{t.pickSee}</Go>}
      {raw && view.reason && <code className="raw-id">{view.reason}</code>}
    </span>
  );
}
