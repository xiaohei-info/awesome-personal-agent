import { useState } from "react";
import { useSession } from "../../app/session";
import { ErrorNote } from "../../app/errors";
import { useT } from "../../app/i18n";
import { strings } from "./strings";

// 来源原文页上的物理删除入口：确认后 POST /v1/settings/purge {object: 来源事件 id}（只有 Web 登录能调；授权与墓碑都在服务端）。
export function PurgeSourceEvent({ id, title, onDeleted }: { id: string; title: string; onDeleted: () => void }) {
  const { api } = useSession();
  const t = useT(strings);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const purge = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.post("/settings/purge", { object: id });
      onDeleted();
    } catch (e) {
      setError(e);
    } finally {
      setConfirming(false);
      setBusy(false);
    }
  };
  return (
    <div>
      {error != null && <ErrorNote error={error} />}
      <button type="button" className="danger" onClick={() => setConfirming(true)} disabled={confirming}>{t.eventDelete}</button>
      {confirming && (
        <div role="alertdialog" aria-labelledby={"purge-event-" + id}>
          <p id={"purge-event-" + id}>{t.eventWarn1}<strong>{title}</strong>{t.eventWarn2}</p>
          <div className="actions">
            <button className="danger" onClick={purge} disabled={busy}>{t.confirm}</button> <button onClick={() => setConfirming(false)} disabled={busy}>{t.cancel}</button>
          </div>
        </div>
      )}
    </div>
  );
}
