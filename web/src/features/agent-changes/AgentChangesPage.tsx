import { useCallback, useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ErrorNote } from "../../app/errors";
import { useNames } from "../../app/names";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { locale, useT } from "../../app/i18n";
import { strings } from "./strings";
import { Val, useLabel } from "../../app/values";

// 形状对应 GET /v1/audit/events 返回的 Step。哪些能撤销、是否最新、授权都由服务端的 item.revert 判定。
interface Step {
  seq: number;
  type: string;
  object: string;
  actor: { type: string; principal: string; via: string; on_behalf_of?: string; harness?: string };
  at: string;
  reason?: string;
  resolved_via?: string;
  // 以下三项只出现在切换前的旧模型记录里（本人原话、代本人、授权单），新事件不再写：只用来标「旧模型记录」。
  evidence?: string;
  grant?: string;
  payload?: { op?: string; reverts?: number; before?: { title?: string }; item?: { title?: string }; state?: string; category?: string; template?: string };
}

const VISIT_KEY = "apa.agentChanges.lastVisit";
const PAGE = 500;
const REVERTIBLE = new Set(["item.updated", "item.phase_changed", "decision.resolved"]);


function Row({ s, reverted, resumed, onRevert }: { s: Step; reverted: boolean; resumed: boolean; onRevert: (s: Step) => Promise<void> }) {
  const t = useT(strings);
  const label = useLabel();
  const [busy, setBusy] = useState(false);
  const [asking, setAsking] = useState(false);
  const credential = useNames("credential");
  const title = s.payload?.item?.title;
  // Agent 替你回答了问题（二档放开，设计 §14.3）：直说它替你答了什么，可按次撤回；后台运行做的说是后台运行。
  const answered = s.type === "decision.resolved" && s.resolved_via === "agent";
  const byRun = s.actor.principal.startsWith("run:");
  const legacy = !!(s.grant || s.evidence || s.actor.on_behalf_of || s.resolved_via === "chat" || s.resolved_via === "grant");
  // 操作者显示 Agent 名而不是钥匙名或 id；钥匙名在悬停里。
  const who = [credential(s.actor.principal), legacy && t.legacy].filter(Boolean).join(" · ");
  const canRevert = REVERTIBLE.has(s.type) && !reverted;
  // 起草建议失败是 APA 自己调模型出的错（actor 记的是内核入站），不是 Agent 的后台运行。
  const draftFailed = s.type === "run.finished" && s.payload?.state === "failed" && s.payload.category === "generate" && s.payload.template === "triage-draft";

  return (
    <article aria-label={`${draftFailed ? t.draftFailed : label("event", s.type)} #${s.seq}`}>
      <h3>
        {draftFailed ? (
          t.draftFailed
        ) : answered ? (
          t.answered(title ?? s.object)
        ) : byRun ? (
          t.runDid(label("event", s.type), title ?? s.object)
        ) : (
          <>
            <Val k="event" id={s.type} />
            {t.colon}
            {title ?? s.object}
          </>
        )}
      </h3>
      <p className="meta">
        {draftFailed ? t.byApa : who}
        {" · "}{new Date(s.at).toLocaleString(locale())}
        {reverted && t.reverted}
      </p>
      {draftFailed ? <p>{t.draftFailedWhy}</p> : s.reason && <p>{s.reason}</p>}
      {answered && resumed && <p className="sub">{t.resumed}</p>}
      {canRevert && !asking && <button onClick={() => setAsking(true)}>{t.revert}</button>}
      {canRevert && asking && (
        <div className="actions">
          <p className="sub">{t.revertWhat}</p>
          <button
            className="primary"
            disabled={busy}
            onClick={async () => {
              setBusy(true);
              try {
                await onRevert(s);
              } finally {
                setBusy(false);
                setAsking(false);
              }
            }}
          >
            {t.confirmRevert}
          </button>
          <button onClick={() => setAsking(false)}>{t.cancel}</button>
        </div>
      )}
    </article>
  );
}

export function AgentChangesPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [steps, setSteps] = useState<Step[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [showOlder, setShowOlder] = useState(false);
  // 基线只在挂载时取一次；离开页面才推进，刷新不会让"新变更"凭空消失。
  const [since] = useState(() => localStorage.getItem(VISIT_KEY));
  // 按 Agent 筛（从 Agent 页「它做过什么」带 ?agent= 进来）：钥匙归属 → Agent id，与 names.tsx 同一规则。
  // shortcut: 后台运行（run:…）的写入对不上钥匙，按 Agent 筛时不显示；要显示得让审计事件带上 Agent id。
  const [params, setParams] = useSearchParams();
  const agent = params.get("agent") ?? "";
  const [agents, setAgents] = useState<{ id: string; name: string }[]>([]);
  const [owner, setOwner] = useState<Map<string, string>>(new Map());
  useEffect(() => {
    api.get<{ agents: { id: string; name: string; harness: string }[] }>("/agents").then((v) => setAgents(v.agents.map((a) => ({ id: a.id, name: a.name || a.harness }))), () => setAgents([]));
    api.get<{ credentials: { id: string; agent_id?: string; harness?: string }[] }>("/settings/tokens").then(
      (r) => setOwner(new Map(r.credentials.map((c) => [c.id, c.agent_id || (c.harness ? "legacy:" + c.harness : "")]))),
      () => setOwner(new Map()),
    );
  }, [api]);

  useEffect(() => () => localStorage.setItem(VISIT_KEY, new Date().toISOString()), []);

  const load = useCallback(async () => {
    try {
      const all: Step[] = [];
      for (let after = 0; ; ) {
        const { events } = await api.get<{ events: Step[] }>(`/audit/events?after=${after}&limit=${PAGE}`);
        all.push(...events);
        if (events.length < PAGE) break;
        after = events[events.length - 1].seq;
      }
      setSteps(all);
    } catch (e) {
      setError(e);
    }
  }, [api]);

  useEffect(() => {
    load();
  }, [load]);

  const revert = async (s: Step) => {
    setError(null);
    try {
      await api.post("/items/" + encodeURIComponent(s.object) + "/revert", { event: s.seq });
      await load();
    } catch (e) {
      setError(e); // 含 403/409：以服务端判定为准，不在前端猜
    }
  };

  const mine = (steps ?? []).filter((s) => s.actor.type === "agent" && s.type !== "command.rejected" && (!agent || owner.get(s.actor.principal) === agent));
  const reverted = new Set((steps ?? []).flatMap((s) => (s.payload?.op === "revert" && s.payload.reverts ? [s.payload.reverts] : [])));
  // 回答之后内核为这件事续接了运行：撤回只撤账本里的回答，已拉起的运行撤不回，行上直说。
  const resumedAfter = (s: Step) => (steps ?? []).some((e) => e.type === "resume.launched" && e.object === s.object && e.seq > s.seq);
  const shown = (showOlder || !since ? mine : mine.filter((s) => Date.parse(s.at) > Date.parse(since))).slice().reverse();

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      <div className="filters">
        <label>
          {t.agent}
          <select value={agent} onChange={(e) => setParams(e.target.value ? { agent: e.target.value } : {}, { replace: true })}>
            <option value="">{t.allAgents}</option>
            {agent && !agents.some((a) => a.id === agent) && <option value={agent}>{agent}</option>}
            {agents.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
          </select>
        </label>
      </div>
      {since && (
        <label>
          <input type="checkbox" checked={showOlder} onChange={(e) => setShowOlder(e.target.checked)} />
          {t.showOlder}
        </label>
      )}
      {error != null && (
        <div>
          <ErrorNote error={error} />
          <button
            onClick={() => {
              setError(null);
              load();
            }}
          >
            {t.retry}
          </button>
        </div>
      )}
      {steps === null ? (
        error == null && <p role="status">{t.loading}</p>
      ) : shown.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        shown.map((s) => <Row key={s.seq} s={s} reverted={reverted.has(s.seq)} resumed={resumedAfter(s)} onRevert={revert} />)
      )}
    </main>
  );
}
