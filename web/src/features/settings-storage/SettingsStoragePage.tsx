import { useCallback, useEffect, useState } from "react";
import { PageHead } from "../../app/page";
import { errorText, locale, useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { strings } from "./strings";

// 设置 › 存储：GET /storage 的数字原样显示；保留天数经 PUT /settings/sources/{id}/content-retention（附录 B.2）。
// 取值范围由服务端判定，页面只挡掉明显不是整数的输入；被拒绝时原因原样显示，输入保留。
interface Usage { id: string; name: string; events: number; content_bytes: number; oldest?: string; retention_days?: number }
interface Storage { ledger_bytes: number; blobs_bytes: number; content_bytes: number; sources: Usage[] }

export function fmtBytes(n: number): string {
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${i ? v.toFixed(1) : v} ${u[i]}`;
}

export function SettingsStoragePage() {
  const t = useT(strings);
  const { api } = useSession();
  const [data, setData] = useState<Storage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    setError(null);
    try {
      setData(await api.get<Storage>("/storage"));
    } catch (e) {
      setData(null);
      setError(errorText(e));
    }
  }, [api]);
  useEffect(() => void load(), [load]);

  if (error)
    return (
      <main>
        <PageHead>{t.title}</PageHead>
        <p role="alert">{error}</p>
        <button onClick={() => void load()}>{t.retry}</button>
      </main>
    );
  if (!data) return <main><p role="status">{t.loading}</p></main>;
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <section aria-label={t.totals} className="panel stack">
        <h2>{t.totals}</h2>
        <dl>
          <dt>{t.ledger}</dt><dd>{fmtBytes(data.ledger_bytes)}</dd>
          <dt>{t.blobs}</dt><dd>{fmtBytes(data.blobs_bytes)}</dd>
          <dt>{t.content}</dt><dd>{fmtBytes(data.content_bytes)}</dd>
        </dl>
      </section>
      <section aria-label={t.sources} className="panel stack">
        <h2>{t.sources}</h2>
        <p className="sub">{t.warn}</p>
        {data.sources.length === 0 && <p className="sub">{t.empty}</p>}
        {data.sources.map((s) => <SourceRow key={s.id} s={s} />)}
      </section>
    </main>
  );
}

function SourceRow({ s }: { s: Usage }) {
  const t = useT(strings);
  const { api } = useSession();
  const saved = s.retention_days == null ? "" : String(s.retention_days);
  const [text, setText] = useState(saved);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  async function save() {
    const v = text.trim();
    if (v && !/^\d+$/.test(v)) return setStatus({ ok: false, text: t.bad });
    setBusy(true);
    setStatus(null);
    try {
      const r = await api.put<{ days: number | null }>(`/settings/sources/${encodeURIComponent(s.id)}/content-retention`, { days: v ? Number(v) : null });
      setText(r.days == null ? "" : String(r.days));
      setStatus({ ok: true, text: t.savedRow });
    } catch (e) {
      setStatus({ ok: false, text: errorText(e) });
    } finally {
      setBusy(false);
    }
  }
  return (
    <div role="group" aria-label={s.name} className="stack">
      <div className="line">
        <strong>{s.name}</strong>
        <span>{t.events}: {s.events}</span>
        <span>{t.size}: {fmtBytes(s.content_bytes)}</span>
        <span>{t.oldest}: {s.oldest ? new Date(s.oldest).toLocaleString(locale()) : "—"}</span>
      </div>
      <div className="line">
        <input type="number" min={1} max={36500} step={1} aria-label={t.days(s.name)} placeholder={t.placeholder} value={text} onChange={(e) => setText(e.target.value)} />
        <button type="button" className="primary" disabled={busy} aria-label={t.saveRow(s.name)} onClick={() => void save()}>{t.save}</button>
      </div>
      {status && <p role={status.ok ? "status" : "alert"}>{status.text}</p>}
    </div>
  );
}
