import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { useT } from "../../app/i18n";
import { HANDOFF_RULE, HANDOFF_TAG } from "../../app/ids";
import { AgentPick } from "../settings-agents/AgentPick";
import { strings } from "./strings";
import type { Item } from "./ItemsPage";

/** 标签的显示名：交给 Agent 的标签不露出英文名。 */
export const tagName = (tag: string, handoff: string) => (tag === HANDOFF_TAG ? handoff : tag);

type Status = { text: string; to: string } | null;

/**
 * 「交给 Agent 处理」：给任务事项加/去掉 agent-ok 标签（走 PATCH /items/{id}，服务端记 Event）。
 * 开着时用一句状态说明前提没满足的原因：规则没开、试运行、Agent 还不能在后台运行；读不到时不说。
 */
export function HandOff({ item, onSave, onAgent }: { onAgent: (v: string) => void; item: Item; onSave: (tags: string[]) => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const on = !!item.tags?.includes(HANDOFF_TAG);
  const [mode, setMode] = useState<string | null>(null);
  const [declared, setDeclared] = useState<boolean | null>(null);
  useEffect(() => {
    if (!on) return;
    api.get<{ rules: { key: string; mode: string }[] }>("/rules").then((r) => setMode(r.rules.find((x) => x.key === HANDOFF_RULE)?.mode ?? "off"), () => setMode(null));
    api.get<{ declarations: { declaration?: { headless?: unknown } }[] }>("/settings/commands").then((r) => setDeclared(r.declarations.some((d) => d.declaration?.headless != null)), () => setDeclared(null));
  }, [api, on]);

  const status: Status = !on ? null
    : mode === "off" ? { text: t.handoffOff, to: "/rules" }
    : mode === "observe" ? { text: t.handoffTrial, to: "/rules" }
    : declared === false ? { text: t.handoffWaitAgent, to: "/settings/agents" }
    : item.phase === "open" ? { text: t.handoffAlreadyOpen, to: "" }
    : null;
  const next = on ? (item.tags ?? []).filter((x) => x !== HANDOFF_TAG) : [...(item.tags ?? []), HANDOFF_TAG];
  return (
    <p className="panel">
      <label><input type="checkbox" checked={on} onChange={() => onSave(next)} /> {t.handoff}</label>
      <AgentPick item main rule={HANDOFF_RULE} value={item.preferred_agent_id ?? ""} onChange={onAgent} />
      {status && <span className="sub"> {status.to ? <Link to={status.to}>{status.text} ›</Link> : status.text}</span>}
    </p>
  );
}
