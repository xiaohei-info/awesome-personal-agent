import { useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 POST /v1/settings/purge（kernel/purge.Result）。授权（仅 Web 本人）、对象校验、墓碑与 blob 引用判断都在服务端；
// 本页让本人按标题找到对象（也可直接填编号），在不可逆操作前再确认一次，并如实展示服务端的回答。
interface Result {
  object: string;
  kind: string;
  events: number;
  blobs_removed?: string[];
  blobs_retained?: string[];
}
interface Hit { id: string; title: string; kind: "item" | "material" }

export function PurgePage() {
  const { api } = useSession();
  const t = useT(strings);
  const [text, setText] = useState("");
  const [picked, setPicked] = useState<Hit | null>(null);
  const [hits, setHits] = useState<Hit[] | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<{ r: Result; name: string } | null>(null);
  const object = picked?.id ?? text.trim();
  const name = picked?.title ?? object;

  const find = async () => {
    const q = text.trim();
    if (!q) return;
    setError(null);
    try {
      const [items, mats] = await Promise.all([
        api.get<{ items: { id: string; title: string }[] }>("/items?" + new URLSearchParams({ q })),
        api.get<{ hits: { id: string; title: string }[] }>("/materials/search?" + new URLSearchParams({ q })),
      ]);
      setHits([...items.items.map((i): Hit => ({ id: i.id, title: i.title, kind: "item" })), ...mats.hits.map((m): Hit => ({ id: m.id, title: m.title, kind: "material" }))]);
    } catch (e) {
      setError(msg(e));
    }
  };

  const purge = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult({ r: await api.post<Result>("/settings/purge", { object }), name });
      setText("");
      setPicked(null);
      setHits(null);
    } catch (e) {
      setError(msg(e));
    } finally {
      setConfirming(false);
      setBusy(false);
    }
  };

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      {error && <div role="alert">{error}</div>}
      {result && (
        <div role="status">
          {t.done(result.name)}
          <details>
            <summary>{t.details}</summary>
            <p className="sub">
              <code>{result.r.object}</code> · {t.detailEvents(result.r.events)}
            </p>
            {result.r.blobs_removed?.length ? <p className="sub">{t.blobsRemoved(result.r.blobs_removed.join(", "))}</p> : null}
            {result.r.blobs_retained?.length ? <p className="sub">{t.blobsRetained(result.r.blobs_retained.join(", "))}</p> : null}
          </details>
        </div>
      )}
      <form
        className="panel stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (object) setConfirming(true);
        }}
      >
        <div>
          <span className="sub">{t.objectId}</span>
          <span className="sentence">
            <input
              aria-label={t.objectId}
              value={text}
              onChange={(e) => {
                setText(e.target.value);
                setPicked(null);
              }}
              disabled={confirming}
            />
            <button type="button" onClick={() => void find()} disabled={!text.trim() || confirming}>{t.find}</button>
          </span>
        </div>
        {hits && (hits.length === 0 ? (
          <p className="sub">{t.noHits}</p>
        ) : (
          <fieldset className="choice">
            <legend>{t.candidates}</legend>
            {hits.map((h) => (
              <label key={h.kind + h.id}>
                <input type="radio" name="purge-target" checked={picked?.id === h.id} onChange={() => setPicked(h)} disabled={confirming} />
                <span>
                  <b>{h.title}</b>
                  <small>{h.kind === "item" ? t.item : t.material}</small>
                </span>
              </label>
            ))}
          </fieldset>
        ))}
        <div>
          <button type="submit" className="danger" disabled={!object || confirming}>{t.delete}</button>
        </div>
      </form>
      {confirming && (
        <div role="alertdialog" aria-labelledby="purge-confirm">
          <p id="purge-confirm">
            {t.warn1}
            <strong>{name}</strong>
            {t.warn2}
          </p>
          <div className="actions">
            <button className="danger" onClick={purge} disabled={busy}>{t.confirm}</button> <button onClick={() => setConfirming(false)} disabled={busy}>{t.cancel}</button>
          </div>
        </div>
      )}
    </main>
  );
}
