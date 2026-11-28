import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useSession } from "../../app/session";
import { locale, useT } from "../../app/i18n";
import { HANDOFF_RULE, HANDOFF_TAG } from "../../app/ids";
import { Prereqs, type Prereq } from "../../app/Prereqs";
import { ErrorNote } from "../../app/errors";
import { useLabel } from "../../app/values";
import { ExecutorPick, executorName, type ExecutorView } from "../executors/ExecutorPick";
import { strings } from "./strings";
import type { Item } from "./ItemsPage";

/** 标签的显示名：交给 Agent 的标签不露出英文名。 */
export const tagName = (tag: string, handoff: string) => (tag === HANDOFF_TAG ? handoff : tag);

const RULE_AT = "/settings/agents#rule-" + encodeURIComponent(HANDOFF_RULE);

/** 交给 Agent 的前提（读 GET /rules 与 GET /executors 的选择预览，判定都在服务端）：规则模式、会用哪个执行者、它现在能不能用。 */
export function useHandoff(itemExecutor?: string, on = true) {
  const { api } = useSession();
  const [mode, setMode] = useState<string | null>(null);
  const [preview, setPreview] = useState<ExecutorView | null>(null);
  useEffect(() => {
    if (!on) return;
    let live = true;
    api.get<{ rules: { key: string; mode: string }[] }>("/rules").then((r) => live && setMode(r.rules.find((x) => x.key === HANDOFF_RULE)?.mode ?? "off"), () => live && setMode(null));
    const query = new URLSearchParams({ runner: "main_session", rule: HANDOFF_RULE, item: itemExecutor ?? "" });
    api.get<ExecutorView>("/executors?" + query).then((v) => live && setPreview(v), () => live && setPreview(null));
    return () => { live = false; };
  }, [api, on, itemExecutor]);
  return { mode, preview };
}

/**
 * 前提清单的几步。auto 为真时（接受建议时一并交出，靠「进入进行中」触发规则）要求规则已打开；
 * 本人点「现在开始」不看规则模式（§13.6），只要求选出的执行者现在能用；去改落到它所属 Agent 的接入清单，没选时去执行者页的分工。
 */
export function handoffPrereqs(t: (typeof strings)["zh"], h: { mode: string | null; preview: ExecutorView | null }, auto: boolean): Prereq[] {
  const id = h.preview?.selected?.executor;
  const ex = h.preview?.executors.find((e) => e.id === id);
  const ready = !!h.preview && !!id && !h.preview.selected?.reason;
  return [
    ...(auto && h.mode !== null ? [{ ok: h.mode === "auto", text: h.mode === "observe" ? t.preTrial : t.preOff, action: { label: t.preTurnOn, to: RULE_AT } }] : []),
    ...(h.preview ? [{
      ok: ready,
      text: id ? t.preAgent(ex ? executorName(ex) : id) : t.preNoAgent,
      action: ex?.harness ? { label: t.preAllow, to: "/settings/agents/" + encodeURIComponent(ex.harness) + "#connect" } : { label: t.preChoose, to: "/executors#roles" },
    }] : []),
  ];
}

interface Run { id: string; target: string; state: string; started_at?: string; finished_at?: string; agent_name?: string }
const FLIGHT = new Set(["queued", "running"]);

/**
 * 事项详情的「Agent 处理」区块（界面设计 §3.2、§7 路径一）：交给 Agent、用哪个 Agent、现在谁在做到哪了、最近一次的结果，
 * 以及下一步——已在进行中、没有在途运行时「现在开始」（item.start，只有本人在 Web 能点）。
 * 前提没满足时列成清单，每条带落点与回程。运行状态取 GET /runs?target=，只读。
 */
export function AgentBlock({ item, onSave, onAgent }: { item: Item; onSave: (tags: string[]) => void; onAgent: (v: string) => void }) {
  const t = useT(strings);
  const label = useLabel();
  const { api } = useSession();
  const on = !!item.tags?.includes(HANDOFF_TAG);
  const h = useHandoff(item.preferred_executor, on);
  const [runs, setRuns] = useState<Run[] | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [error, setError] = useState<unknown>(null);
  const loadRuns = useCallback(
    () => api.get<{ runs: Run[] }>("/runs?target=" + encodeURIComponent(item.id) + "&limit=20").then((r) => setRuns(r.runs), () => setRuns([])),
    [api, item.id],
  );
  useEffect(() => { void loadRuns(); }, [loadRuns, item.version]);
  const last = runs?.length ? runs[runs.length - 1] : undefined;
  const busy = !!last && FLIGHT.has(last.state);
  const open = item.phase === "open";
  const ended = item.phase === "done" || item.phase === "dropped";
  const pre = on && !ended ? handoffPrereqs(t, h, item.phase === "proposed") : [];
  const ready = pre.every((x) => x.ok);
  const start = async () => {
    setError(null);
    setNote(null);
    try {
      await api.post("/items/" + encodeURIComponent(item.id) + "/start", {});
      setNote(t.started);
      await loadRuns();
    } catch (e) {
      setError(e);
    }
  };
  const next = on ? (item.tags ?? []).filter((x) => x !== HANDOFF_TAG) : [...(item.tags ?? []), HANDOFF_TAG];
  const when = (s?: string) => (s ? new Date(s).toLocaleString(locale(), { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "");
  // 已结束的事项只回看：谁做的、做完没有、什么时候；没交给过 Agent 的不显示这一块。
  if (ended && !last) return null;
  return (
    <section aria-label={t.agentBlock} id="agent" className="panel stack">
      <h2>{t.agentBlock}</h2>
      {!ended && <label><input type="checkbox" checked={on} onChange={() => onSave(next)} /> {t.handoff}</label>}
      {on && !ended && <ExecutorPick item runner="main_session" rule={HANDOFF_RULE} value={item.preferred_executor ?? ""} onChange={onAgent} />}
      {last && (
        <p className="sub">
          {[last.agent_name, label("runState", last.state), when(last.finished_at ?? last.started_at)].filter(Boolean).join(" · ")}{" "}
          <Link to="/runs">{t.seeRuns}</Link>
        </p>
      )}
      {on && <Prereqs label={t.prereqs} items={pre} />}
      {on && open && !busy && ready && (
        <div className="actions">
          <button type="button" className="primary" onClick={() => void start()}>{t.startNow}</button>
          {!last && <span className="sub">{t.startHint}</span>}
        </div>
      )}
      {note && <p role="status">{note}</p>}
      {error != null && <ErrorNote error={error} />}
    </section>
  );
}
