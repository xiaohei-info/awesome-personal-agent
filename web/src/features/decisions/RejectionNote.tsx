import { locale, useT } from "../../app/i18n";
import { strings } from "./strings";
// 形状对应 GET /v1/items 的 last_rejections（api/items.Rejection，§14.5）：事项上最近一次、尚未被处理的退出码 23 拒绝。
// 决策待办与建议流共用；是否仍显示由服务端判定（本人处理后不再返回）。
export interface Rejection {
  seq: number;
  at: string;
  actor: { type: string; principal: string; via: string };
  code: string;
  why: string;
  grant?: string;
}

export function RejectionNote({ rejection, what }: { rejection?: Rejection; what: "answer" | "handle" }) {
  const t = useT(strings);
  if (!rejection) return null;
  const w = t.rejectedWhat[what];
  return (
    <p role="note" aria-label={t.rejected(w)}>
      <strong>{t.rejected(w)}</strong>
      {t.colon}
      {t.rejectedBody(rejection.actor.principal, w)} · {rejection.why}
      {rejection.grant ? t.rejectedGrant(rejection.grant) : ""}
      {" · "}
      {new Date(rejection.at).toLocaleString(locale())}
    </p>
  );
}
