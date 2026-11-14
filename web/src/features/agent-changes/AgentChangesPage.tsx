import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";
import { Val, useLabel } from "../../app/values";

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


function Row({ s, reverted, onRevert }: { s: Step; reverted: boolean; onRevert: (s: Step) => Promise<void> }) {
  const t = useT(strings);
  const label = useLabel();
  const [busy, setBusy] = useState(false);
  const title = s.payload?.item?.title;
  const who = [s.actor.principal, s.actor.on_behalf_of && t.onBehalfOf(s.actor.on_behalf_of), s.resolved_via === "chat" && t.viaChat, s.grant && t.viaGrant(s.grant)].filter(Boolean).join(" · ");
  const canRevert = REVERTIBLE.has(s.type) && !reverted;

  return (
    <article aria-label={`${label("event", s.type)} #${s.seq}`}>
      <h3>
        <Val k="event" id={s.type} />
        {t.colon}
        {title ?? s.object}
      </h3>
      <p className="meta">
        {who} · {new Date(s.at).toLocaleString(locale())}
        {reverted && t.reverted}
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
          {t.revert}
        </button>
      )}
    </article>
  );
}

export function AgentChangesPage() {
  const { api } = useSession();
  const t = useT(strings);
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
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {since && (
        <label>
          <input type="checkbox" checked={showOlder} onChange={(e) => setShowOlder(e.target.checked)} />
          {t.showOlder}
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
            {t.retry}
          </button>
        </div>
      )}
      {steps === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : shown.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        shown.map((s) => <Row key={s.seq} s={s} reverted={reverted.has(s.seq)} onRevert={revert} />)
      )}
    </main>
  );
}
