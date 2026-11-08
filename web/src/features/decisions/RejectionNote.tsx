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

export function RejectionNote({ rejection, what }: { rejection?: Rejection; what: "代答" | "处理" }) {
  if (!rejection) return null;
  return (
    <p role="note" aria-label={`被拒绝的${what}`}>
      <strong>被拒绝的{what}</strong>：{rejection.actor.principal} 的{what}未生效 · {rejection.why}
      {rejection.grant ? ` · 授权单 ${rejection.grant}` : ""}
      {" · "}
      {new Date(rejection.at).toLocaleString()}
    </p>
  );
}
