import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";

// 形状对应 GET /v1/audit/events 返回的 Step。哪些能撤销、是否最新、授权都由服务端的 item.revert 判定。
interface Step {
  seq: number;
  type: string;
  object: string;
  actor: { type: string; principal: string; via: string; on_behalf_of?: string };
  at: string;
  reason?: string;
  evidence?: string;
  resolved_via?: string;
  grant?: string;
  payload?: { op?: string; reverts?: number; before?: { title?: string }; item?: { title?: string } };
}

const VISIT_KEY = "apa.agentChanges.lastVisit";
const PAGE = 500;
const REVERTIBLE = new Set(["item.updated", "item.phase_changed", "decision.resolved"]);
const TYPE_LABEL: Record<string, string> = { "item.updated": "修改事项", "item.phase_changed": "变更阶段", "item.created": "新建事项", "decision.resolved": "决定" };

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

function Row({ s, reverted, onRevert }: { s: Step; reverted: boolean; onRevert: (s: Step) => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const title = s.payload?.item?.title;
  const who = [s.actor.principal, s.actor.on_behalf_of && `代 ${s.actor.on_behalf_of}`, s.resolved_via === "chat" && "聊天代答", s.grant && `授权单代答 · 授权单 ${s.grant}`].filter(Boolean).join(" · ");
  const canRevert = REVERTIBLE.has(s.type) && !reverted;

  return (
    <article aria-label={`${s.type} #${s.seq}`}>
      <h3>
        {TYPE_LABEL[s.type] ?? s.type}：{title ?? s.object}
      </h3>
      <p>
        {who} · {new Date(s.at).toLocaleString()}
        {reverted && " · 已撤销"}
      </p>
      {s.reason && <p>{s.reason}</p>}
      {s.evidence && <blockquote>{s.evidence}</blockquote>}
      {canRevert && (
        <button
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            try {
              await onRevert(s);
            } finally {
              setBusy(false);
            }
          }}
        >
          撤销
        </button>
      )}
    </article>
  );
}

export function AgentChangesPage() {
  const { api } = useSession();
  const [steps, setSteps] = useState<Step[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [showOlder, setShowOlder] = useState(false);
  // 基线只在挂载时取一次；离开页面才推进，刷新不会让"新变更"凭空消失。
  const [since] = useState(() => localStorage.getItem(VISIT_KEY));

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
      setError(msg(e));
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
      setError(msg(e)); // 含 403/409：以服务端判定为准，不在前端猜
    }
  };

  const mine = (steps ?? []).filter((s) => s.actor.type === "agent" && s.type !== "command.rejected");
  const reverted = new Set((steps ?? []).flatMap((s) => (s.payload?.op === "revert" && s.payload.reverts ? [s.payload.reverts] : [])));
  const shown = (showOlder || !since ? mine : mine.filter((s) => Date.parse(s.at) > Date.parse(since))).slice().reverse();

  return (
    <main>
      <h2>agent 变更</h2>
      <p>撤销只会写一条反向命令恢复账本状态，原记录保留；外部已发生的副作用不会被撤销。</p>
      {since && (
        <label>
          <input type="checkbox" checked={showOlder} onChange={(e) => setShowOlder(e.target.checked)} />
          显示更早的变更
        </label>
      )}
      {error && (
        <div role="alert">
          {error}{" "}
          <button
            onClick={() => {
              setError(null);
              load();
            }}
          >
            重试
          </button>
        </div>
      )}
      {steps === null ? (
        !error && <p>加载中…</p>
      ) : shown.length === 0 ? (
        <p>没有 agent 变更</p>
      ) : (
        shown.map((s) => <Row key={s.seq} s={s} reverted={reverted.has(s.seq)} onRevert={revert} />)
      )}
    </main>
  );
}
