import { locale, useT } from "../../app/i18n";
import { Go } from "../../app/page";
import { useNames } from "../../app/names";
import { useRaw } from "../../app/raw";
import { useLabel } from "../../app/values";
import { strings } from "./strings";
// 形状对应 GET /v1/items 的 last_rejections 与 GET /v1/items/{id} 的 last_rejection（api/items.Rejection，§14.5）：
// 事项上最近一次、尚未被处理的退出码 23 拒绝。是否仍显示由服务端判定。被拒的写入从不生效，所以总写"事项未被修改"。
export interface Rejection {
  seq: number;
  at: string;
  actor: { type: string; principal: string; via: string };
  action?: string;
  code: string;
  /** 更细的稳定原因（kernel/domain GrantReason*），有就按它说人话，比 code 准。 */
  reason?: string;
  why: string;
  grant?: string;
}

/** 谁的什么操作被拒、为什么（按原因码说人话）、事项未被修改，以及下一步：去签发授权。英文原因只在显示原始值里。 */
export function RejectionNote({ rejection, what, item }: { rejection?: Rejection; what: "answer" | "handle" | "edit"; item?: string }) {
  const t = useT(strings);
  const label = useLabel();
  const raw = useRaw();
  const credential = useNames("credential");
  if (!rejection) return null;
  const w = t.rejectedWhat[what];
  const act = (rejection.action && t.rejectedAction[rejection.action]) || w;
  const who = rejection.actor.type === "agent" ? credential(rejection.actor.principal) : rejection.actor.principal;
  // 授权单因初筛待定暂不可用：第一步是处理事项上那张卡，而不是去签授权（授权本身没问题）。
  const screening = rejection.reason === "grant_screening_pending";
  const why = rejection.reason ? label("grantReason", rejection.reason) : label("error", rejection.code);
  return (
    <p role="note" aria-label={t.rejected(w)}>
      <strong>{t.rejected(w)}</strong>
      {t.colon}
      {t.rejectedBody(who, act)} · {why} · {t.notApplied}
      {rejection.grant ? t.rejectedGrant(rejection.grant) : ""}
      {" · "}
      {new Date(rejection.at).toLocaleString(locale())}
      {" "}
      <details className="inline">
        <summary>{t.details}</summary>
        <code className={raw ? "raw-id" : undefined}>{rejection.why}</code>
      </details>{" "}
      {screening ? (item && <Go className="quiet" to={"/items/" + encodeURIComponent(item)}>{t.goCard}</Go>) : <Go className="quiet" to="/settings/agents">{t.goGrant}</Go>}
    </p>
  );
}
