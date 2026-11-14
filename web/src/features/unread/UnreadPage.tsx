import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";
import { Val } from "../../app/values";

// 形状对应 GET /v1/deliveries/unread 返回的 domain.Delivery。未读 = 退回 Web 且本人没点过；是否未读由服务端判定。
export interface UnreadDelivery {
  id: string;
  item: string;
  class: string;
  version: number;
  state: string;
  content: { title: string; summary?: string; options?: string[]; web_url?: string };
  injections?: { injected_to_session: string; at: string }[];
}


export function UnreadPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [items, setItems] = useState<UnreadDelivery[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const load = useCallback(
    () =>
      api.get<{ deliveries: UnreadDelivery[] }>("/deliveries/unread").then(
        (r) => setItems(r.deliveries),
        (e) => setError(msg(e)),
      ),
    [api],
  );

  useEffect(() => {
    load();
  }, [load]);

  // 只有本人点击才发 read；注入记录只展示，不触发任何确认。失败后重新读取，以服务端为准。
  const markRead = async (id: string) => {
    setError(null);
    setBusy(id);
    try {
      await api.post("/deliveries/" + encodeURIComponent(id) + "/read");
    } catch (e) {
      setError(msg(e));
    }
    await load();
    setBusy(null);
  };

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
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
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        items.map((d) => (
          <article key={d.id} aria-label={d.content.title}>
            <h3>{d.content.title}</h3>
            <p className="meta"><span className="badge plain"><Val k="deliveryClass" id={d.class} /></span></p>
            {d.content.summary && <p>{d.content.summary}</p>}
            {d.content.options && d.content.options.length > 0 && <p>{t.options(d.content.options.join(" / "))}</p>}
            {d.content.web_url && <a href={d.content.web_url}>{t.open}</a>}
            {d.injections && d.injections.length > 0 && (
              <p>{t.injected(d.injections.length)}</p>
            )}
            <div className="actions">
              <button disabled={busy === d.id} onClick={() => markRead(d.id)}>
                {t.markRead}
              </button>
            </div>
          </article>
        ))
      )}
    </main>
  );
}
