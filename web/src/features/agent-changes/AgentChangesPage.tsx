import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ErrorNote } from "../../app/errors";
import { useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
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
  const [asking, setAsking] = useState(false);
  const credential = useNames("credential");
  const raw = useRaw();
  const title = s.payload?.item?.title;
  // 操作者显示凭据的标签而不是 id；临时授权给出链接，id 只在「显示原始值」里。
  const who = [credential(s.actor.principal), s.actor.on_behalf_of && t.onBehalfOf(s.actor.on_behalf_of), s.resolved_via === "chat" && t.viaChat].filter(Boolean).join(" · ");
  const canRevert = REVERTIBLE.has(s.type) && !reverted;

  return (
    <article aria-label={`${label("event", s.type)} #${s.seq}`}>
      <h3>
        <Val k="event" id={s.type} />
        {t.colon}
        {title ?? s.object}
      </h3>
      <p className="meta">
        {who}
        {s.grant && (
          <>
            {" · "}
            <Link to={"/settings/grants#" + encodeURIComponent(s.grant)}>{t.viaGrant}</Link>
            {raw && <code className="raw-id">{s.grant}</code>}
          </>
        )}
        {" · "}{new Date(s.at).toLocaleString(locale())}
        {reverted && t.reverted}
      </p>
      {s.reason && <p>{s.reason}</p>}
      {s.evidence && <blockquote>{s.evidence}</blockquote>}
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

  const mine = (steps ?? []).filter((s) => s.actor.type === "agent" && s.type !== "command.rejected");
  const reverted = new Set((steps ?? []).flatMap((s) => (s.payload?.op === "revert" && s.payload.reverts ? [s.payload.reverts] : [])));
  const shown = (showOlder || !since ? mine : mine.filter((s) => Date.parse(s.at) > Date.parse(since))).slice().reverse();

  return (
    <main>
      <PageHead>{t.title}</PageHead>
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
        shown.map((s) => <Row key={s.seq} s={s} reverted={reverted.has(s.seq)} onRevert={revert} />)
      )}
    </main>
  );
}
