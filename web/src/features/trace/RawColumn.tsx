import { useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { strings } from "./strings";

// 来源原文栏（设计 §6.10、§15.1）：GET /v1/source-events/{id} 的展示。Web 登录读到全文。
// 所有内容只经 React 文本节点进入页面——不渲染 HTML，也就不会加载其中的任何外部资源。
interface Manifest { state: string; bytes: number; missing?: { part: string; reason?: string }[] }
interface Attachment { name: string; media_type?: string; size: number }
interface ChatMessage { sender: string; text: string; at: string }
interface RawRead {
  content: Manifest;
  body?: string;
  original?: { media_type: string; text: string };
  headers?: Record<string, string | string[]>;
  attachments?: Attachment[];
  messages?: ChatMessage[];
}

export function bytesText(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) (v /= 1024), i++;
  return `${v.toFixed(1)} ${units[i]}`;
}

export function RawColumn({ id }: { id: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const [data, setData] = useState<RawRead | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [asRaw, setAsRaw] = useState(false);
  useEffect(() => {
    let live = true;
    setData(null);
    setError(null);
    api.get<RawRead>(`/source-events/${encodeURIComponent(id)}`).then((d) => live && setData(d), (e) => live && setError(e));
    return () => {
      live = false;
    };
  }, [api, id]);

  const stateText = (m: Manifest) => {
    switch (m.state) {
      case "partial": return t.rawPartial((m.missing ?? []).map((x) => (t.rawPartNames[x.part] ?? x.part) + (x.reason ? `（${x.reason}）` : "")).join(t.listSep));
      case "legacy": return t.rawLegacy;
      case "purged": return t.rawPurged;
    }
    return t.rawFull;
  };

  return (
    <section role="region" aria-label={t.rawTitle}>
      <h2>{t.rawTitle}</h2>
      {error != null && <ErrorNote error={error} />}
      {!data && error == null && <p role="status">{t.loading}</p>}
      {data && (
        <>
          <p className="sub">{t.rawHead(bytesText(data.content.bytes), stateText(data.content))}</p>
          {data.content.state !== "purged" && (
            <>
              {data.original && (
                <p>
                  <button type="button" className="link" aria-pressed={asRaw} onClick={() => setAsRaw(!asRaw)}>{asRaw ? t.rawPlain : t.rawFormat}</button>
                  {asRaw && <> <code>{data.original.media_type}</code></>}
                </p>
              )}
              {asRaw && data.original ? (
                <pre className="raw-text">{data.original.text}</pre>
              ) : (
                data.body !== undefined && <pre className="raw-text">{data.body}</pre>
              )}
              {data.headers && Object.keys(data.headers).length > 0 && (
                <table aria-label={t.rawHeaders}>
                  <tbody>
                    {Object.entries(data.headers).map(([k, v]) => (
                      <tr key={k}>
                        <th scope="row">{k}</th>
                        <td>{[v].flat().map((x, i) => <div key={i}>{x}</div>)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {data.attachments && data.attachments.length > 0 && (
                <table aria-label={t.rawAttachments}>
                  <thead><tr><th>{t.rawName}</th><th>{t.rawType}</th><th>{t.rawSize}</th></tr></thead>
                  <tbody>
                    {data.attachments.map((a, i) => (
                      <tr key={i}><td>{a.name}</td><td>{a.media_type ?? ""}</td><td>{bytesText(a.size)}</td></tr>
                    ))}
                  </tbody>
                </table>
              )}
              {data.messages && data.messages.length > 0 && (
                <ol aria-label={t.rawMessages} className="rows">
                  {data.messages.map((m, i) => (
                    <li key={i}>
                      <strong>{m.sender}</strong> · <time dateTime={m.at}>{new Date(m.at).toLocaleString(locale())}</time>
                      <pre className="raw-text">{m.text}</pre>
                    </li>
                  ))}
                </ol>
              )}
            </>
          )}
        </>
      )}
    </section>
  );
}
