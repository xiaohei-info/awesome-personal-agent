import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

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
  const t = useT(strings);
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
    <section aria-label={t.title}>
      <h2>{t.title}</h2>
      {error && <p role="alert">{error}</p>}
      {!list && !error && <p role="status">{t.loading}</p>}
      {list && !list.length && <p className="sub">{t.none}</p>}
      {list && list.length > 0 && (
        <ul aria-label={t.list} className="rows">
          {list.map((m) => (
            <li key={m.id} data-forward-allowed={m.forward_allowed}>
              <strong>{m.subject || m.summary || m.id}</strong>
              {m.subject && m.summary ? ` — ${m.summary}` : ""}
              {" · "}
              <span aria-label={t.sender}>{m.sender || t.unknownSender}</span>
              {" · "}
              <span aria-label={t.verifiedLabel}>{m.sender_verified ? t.verified : t.unverified}</span>
              {" · "}
              <span aria-label={t.allowLabel}>{m.forward_allowed ? t.allowed : t.notAllowed}</span>
              {" · "}
              <span aria-label={t.stateLabel}>{t.states[m.attach_state] ?? m.attach_state}</span>
              {!m.forward_allowed && t.ownerOnly}
              {safeHref(m.link) ? <> · <a href={safeHref(m.link)!} rel="noopener noreferrer">{t.source}</a></> : null}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
