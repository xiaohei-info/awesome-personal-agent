import { locale, useT } from "../../app/i18n";
import { REJECT_REASON } from "../../app/ids";
import { Go } from "../../app/page";
import { useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { useLabel } from "../../app/values";
import { strings } from "./strings";
// 形状对应 GET /v1/items 的 last_rejections 与 GET /v1/items/{id} 的 last_rejection（api/items.Rejection，设计 §14.3）：
// 事项上最近一次、尚未被处理的退出码 23 拒绝（档位没放开）。是否仍显示由服务端判定。被拒的写入从不生效，所以总写"事项未被修改"。
export interface Rejection {
  seq: number;
  at: string;
  actor: { type: string; principal: string; via: string };
  action?: string;
  code: string;
  /** 拒绝的口径（kernel/domain Reason*：hard_line / tier_closed / out_of_scope）；旧记录可能是授权单的 GrantReason*。 */
  reason?: string;
  why: string;
}

const isReason = (r?: string): r is (typeof REJECT_REASON)[number] => !!r && (REJECT_REASON as readonly string[]).includes(r);

/** 谁的什么操作被拒、按三种口径说为什么、事项未被修改，以及下一步：你来处理，或去看它的权限卡 / 它的钥匙。英文原因只在显示原始值里。 */
export function RejectionNote({ rejection, what, item }: { rejection?: Rejection; what: "answer" | "handle" | "edit"; item?: string }) {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const credential = useNames("credential");
  if (!rejection) return null;
  const w = t.rejectedWhat[what];
  const act = (rejection.action && t.rejectedAction[rejection.action]) || w;
  const who = rejection.actor.type === "agent" ? credential(rejection.actor.principal) : rejection.actor.principal;
  const reason = rejection.reason;
  const why = isReason(reason) ? label("rejectReason", reason) : reason ? label("grantReason", reason) : label("error", rejection.code);
  const self = item ? "/items/" + encodeURIComponent(item) : undefined;
  return (
    <p role="note" aria-label={t.rejected(w)}>
      <strong>{t.rejected(w)}</strong>
      {t.colon}
      {t.rejectedBody(who, act)} · {why} · {t.notApplied}
      {" · "}
      {new Date(rejection.at).toLocaleString(locale())}
      {" "}
      <details className="inline">
        <summary>{t.details}</summary>
        <code className={raw ? "raw-id" : undefined}>{rejection.why}</code>
      </details>{" "}
      {self && <Go className="quiet" to={self}>{t.goSelf}</Go>}{" "}
      {reason === "out_of_scope" ? <Go className="quiet" to="/settings/agents">{t.goKeys}</Go> : reason !== "hard_line" && <Go className="quiet" to="/settings/agents#permission-overview">{t.goTier}</Go>}
    </p>
  );
}
