import { useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";

// 形状对应 GET /v1/items/{id}/messages（附录 B.2）。Web 登录拿到全部挂靠消息；
// 白名单判定在服务端（forward_allowed），本组件只显示，不判定、不修改白名单。
export interface AttachedMessage {
  id: string;
  source: string;
  sender?: string;
  sender_verified?: boolean;
  subject?: string;
  summary?: string;
  link?: string;
  occurred_at: string;
  attach_state: string;
  forward_allowed: boolean;
}

const STATE: Record<string, string> = {
  pending: "待转发",
  forwarded: "已转发",
  processed: "已处理",
  handed_back: "已交回",
  released: "已退回判断流",
};

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

// link 来自外部来源，内核不校验协议；只放行 http(s)，防止 javascript: 等在已登录的 Web 源内执行。
function safeHref(link?: string): string | null {
  if (!link) return null;
  try {
    const u = new URL(link);
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

export function AttachedMessages({ itemId }: { itemId: string }) {
  const { api } = useSession();
  const [list, setList] = useState<AttachedMessage[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setList(null);
    setError(null);
    api.get<{ messages: AttachedMessage[] }>("/items/" + encodeURIComponent(itemId) + "/messages").then(
      (r) => setList(r.messages),
      (e) => setError(msg(e)),
    );
  }, [api, itemId]);

  return (
    <section aria-label="挂靠消息">
      <h2>挂靠消息</h2>
      {error && <p role="alert">{error}</p>}
      {!list && !error && <p role="status">加载中…</p>}
      {list && !list.length && <p className="sub">没有挂靠消息</p>}
      {list && list.length > 0 && (
        <ul aria-label="挂靠消息列表" className="rows">
          {list.map((m) => (
            <li key={m.id} data-forward-allowed={m.forward_allowed}>
              <strong>{m.subject || m.summary || m.id}</strong>
              {m.subject && m.summary ? ` — ${m.summary}` : ""}
              {" · "}
              <span aria-label="发送者">{m.sender || "未知发送者"}</span>
              {" · "}
              <span aria-label="核实状态">{m.sender_verified ? "已核实" : "未核实"}</span>
              {" · "}
              <span aria-label="白名单">{m.forward_allowed ? "已进白名单" : "未进白名单"}</span>
              {" · "}
              <span aria-label="处理状态">{STATE[m.attach_state] ?? m.attach_state}</span>
              {!m.forward_allowed && " · 仅本人可见，不进入无人值守会话，不算已处理"}
              {safeHref(m.link) ? <> · <a href={safeHref(m.link)!} rel="noopener noreferrer">来源</a></> : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
