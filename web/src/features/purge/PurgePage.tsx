import { useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 POST /v1/settings/purge（kernel/purge.Result）。授权（仅 Web 本人）、对象校验、墓碑与 blob 引用判断都在服务端；
// 本页只负责在不可逆操作前让本人确认，并如实展示服务端的回答。
interface Result {
  object: string;
  kind: string;
  events: number;
  blobs_removed?: string[];
  blobs_retained?: string[];
}


export function PurgePage() {
  const { api } = useSession();
  const t = useT(strings);
  const [id, setId] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Result | null>(null);
  const object = id.trim();

  const purge = async () => {
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await api.post<Result>("/settings/purge", { object }));
      setId("");
    } catch (e) {
      setError(msg(e));
    } finally {
      setConfirming(false);
      setBusy(false);
    }
  };

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error && <div role="alert">{error}</div>}
      {result && (
        <div role="status">
          {t.done(result.kind, result.object, result.events)}
          {result.blobs_removed?.length ? t.blobsRemoved(result.blobs_removed.join(", ")) : ""}
          {result.blobs_retained?.length ? t.blobsRetained(result.blobs_retained.join(", ")) : ""}
        </div>
      )}
      <form
        className="panel"
        onSubmit={(e) => {
          e.preventDefault();
          if (object) setConfirming(true);
        }}
      >
        <label>
          {t.objectId}
          <input value={id} onChange={(e) => setId(e.target.value)} disabled={confirming} />
        </label>
        <div>
          <button type="submit" disabled={!object || confirming}>{t.delete}</button>
        </div>
      </form>
      {confirming && (
        <div role="alertdialog" aria-labelledby="purge-confirm">
          <p id="purge-confirm">
            {t.warn1}
            <strong>{object}</strong>
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
