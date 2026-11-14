import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import "./judgment.css";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/judgments/stats、/judgments/spot-check、/settings/judgment（附录 B.2、B.4；主文档 §8.10）。
// 指标、分布、抽检与不确定带的校验都在服务端；本页只调用并如实展示服务端的回答。
interface Row { use: string; template: string; source?: string; backend: string; calls: number; decided: number; uncertain: number; errors: number; reused: number }
interface Rate { use: string; template: string; source?: string; escalation: number; undecided: number; acceptance: number; false_drop: number; reviewed: number }
interface Bucket { use: string; template: string; source?: string; low: number; high: number; count: number }
interface Stats { rows: Row[]; rates: Rate[]; histogram: Bucket[] }
interface Band { template: string; source?: string; low: number; high: number }
interface Settings { bands: Band[]; shadow: string[]; spot_check: { per_brief: number } }
interface Entry { judgment: string; event: string; source: string; subject?: string; template: string; judged_at: string; results: string[] }
interface Source { id: string }

const WINDOWS = ["7d", "30d", "90d"];
const pct = (v: number) => `${(v * 100).toFixed(1)}%`;
const key = (g: { use: string; template: string; source?: string }) => `${g.use}|${g.template}|${g.source ?? ""}`;

interface BandDraft { template: string; source: string; low: string; high: string }
const toDraft = (b: Band): BandDraft => ({ template: b.template, source: b.source ?? "", low: String(b.low), high: String(b.high) });

export function JudgmentPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [since, setSince] = useState("30d");
  const [template, setTemplate] = useState("");
  const [source, setSource] = useState("");
  const [sources, setSources] = useState<Source[]>([]);
  const [stats, setStats] = useState<Stats | null>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [drafts, setDrafts] = useState<BandDraft[]>([]);
  const [newShadow, setNewShadow] = useState("");
  const [pool, setPool] = useState<Entry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const loadStats = useCallback(async () => {
    const q = new URLSearchParams({ since });
    if (template) q.set("template", template);
    if (source) q.set("source", source);
    setStats(await api.get<Stats>(`/judgments/stats?${q}`));
  }, [api, since, template, source]);
  const loadSettings = useCallback(async () => {
    const s = await api.get<Settings>("/settings/judgment");
    setSettings(s);
    setDrafts(s.bands.map(toDraft));
  }, [api]);
  const loadPool = useCallback(async () => setPool((await api.get<{ spot_check: Entry[] }>("/judgments/spot-check")).spot_check), [api]);
  const guard = useCallback(async (f: () => Promise<void>) => {
    try {
      await f();
      setError(null);
    } catch (e) {
      setError(msg(e));
    }
  }, []);

  useEffect(() => { void guard(loadStats); }, [guard, loadStats]);
  useEffect(() => {
    void guard(async () => {
      await Promise.all([loadSettings(), loadPool(), api.get<{ sources: Source[] }>("/sources").then((r) => setSources(r.sources))]);
    });
  }, [api, guard, loadSettings, loadPool]);

  const saveBands = () =>
    guard(async () => {
      setNotice(null);
      const bands = drafts.map((d) => ({ template: d.template.trim(), ...(d.source ? { source: d.source } : {}), low: Number(d.low), high: Number(d.high) }));
      await api.put("/settings/judgment", { bands });
      await loadSettings();
      setNotice(t.bandsSaved);
    });
  const saveShadow = (shadow: string[]) =>
    guard(async () => {
      setNotice(null);
      await api.put("/settings/judgment", { shadow });
      await loadSettings();
    });
  const review = (e: Entry, result: string) =>
    guard(async () => {
      setNotice(null);
      await api.post(`/judgments/${encodeURIComponent(e.judgment)}/review`, { result });
      await Promise.all([loadPool(), loadStats()]);
      setNotice(t.reviewed(t.results[result] ?? result));
    });
  const setDraft = (i: number, patch: Partial<BandDraft>) => setDrafts((ds) => ds.map((d, j) => (j === i ? { ...d, ...patch } : d)));

  const templates = [...new Set([...(stats?.rates ?? []).map((r) => r.template), ...(settings?.bands ?? []).map((b) => b.template), ...(settings?.shadow ?? [])])].sort();
  const bandFor = (g: { template: string; source?: string }) =>
    settings?.bands.find((b) => b.template === g.template && (b.source ?? "") === (g.source ?? "")) ?? settings?.bands.find((b) => b.template === g.template && !b.source);

  return (
    <main className="judgment">
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error && <p role="alert" className="err">{error}</p>}
      {notice && <p role="status">{notice}</p>}

      <h2>{t.metrics}</h2>
      <p>
        <label>{t.window} <select aria-label={t.window} value={since} onChange={(e) => setSince(e.target.value)}>{WINDOWS.map((w) => <option key={w} value={w}>{t.days(w)}</option>)}</select></label>{" "}
        <label>{t.template} <select aria-label={t.template} value={template} onChange={(e) => setTemplate(e.target.value)}><option value="">{t.all}</option>{templates.map((x) => <option key={x}>{x}</option>)}</select></label>{" "}
        <label>{t.source} <select aria-label={t.source} value={source} onChange={(e) => setSource(e.target.value)}><option value="">{t.all}</option>{sources.map((s) => <option key={s.id}>{s.id}</option>)}</select></label>
      </p>
      {!stats ? <p>{t.loading}</p> : stats.rates.length === 0 ? <p>{t.noRecords}</p> : (
        <table aria-label={t.metrics}>
          <thead><tr>{t.rateCols.map((c) => <th key={c}>{c}</th>)}</tr></thead>
          <tbody>
            {stats.rates.map((r) => (
              <tr key={key(r)}>
                <td>{r.use}</td><td>{r.template}</td><td>{r.source ?? t.all}</td>
                <td>{pct(r.escalation)}</td><td>{pct(r.undecided)}</td><td>{pct(r.acceptance)}</td><td>{pct(r.false_drop)}</td><td>{r.reviewed}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {stats && stats.rows.length > 0 && (
        <table aria-label={t.calls}>
          <thead><tr>{t.callCols.map((c) => <th key={c}>{c}</th>)}</tr></thead>
          <tbody>
            {stats.rows.map((r) => (
              <tr key={key(r) + "|" + r.backend}>
                <td>{r.use}</td><td>{r.template}</td><td>{r.source ?? t.all}</td><td>{r.backend}</td><td>{r.calls}</td><td>{r.decided}</td><td>{r.uncertain}</td><td>{r.errors}</td><td>{r.reused}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2>{t.distribution}</h2>
      {(stats?.rates ?? []).map((g) => {
        const buckets = (stats?.histogram ?? []).filter((b) => key(b) === key(g));
        if (buckets.length === 0) return null;
        const max = Math.max(...buckets.map((b) => b.count));
        const band = bandFor(g);
        return (
          <figure key={key(g)} aria-label={t.dist(g.use, g.template, g.source ?? t.all)}>
            <figcaption>
              {g.use} · {g.template} · {g.source ?? t.allSources}
              {t.currentBand}
              {band ? `${band.low}–${band.high}` : t.packBand}
              {t.lowEnd(pct(g.false_drop), g.reviewed)}
              {t.highEnd(pct(g.acceptance))}
            </figcaption>
            <table>
              <tbody>
                {buckets.map((b) => (
                  <tr key={b.low}>
                    <td>{b.low.toFixed(1)}–{b.high.toFixed(1)}</td>
                    <td><progress aria-label={`${b.low.toFixed(1)}–${b.high.toFixed(1)}`} max={max} value={b.count} /></td>
                    <td>{b.count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </figure>
        );
      })}

      <h2>{t.bands}</h2>
      <p>{t.bandsHint}</p>
      <table aria-label={t.bands}>
        <thead><tr>{t.bandCols.map((c) => <th key={c}>{c}</th>)}<th /></tr></thead>
        <tbody>
          {drafts.map((d, i) => (
            <tr key={i}>
              <td><input aria-label={t.bandTemplate} value={d.template} onChange={(e) => setDraft(i, { template: e.target.value })} /></td>
              <td>
                <select aria-label={t.bandSource} value={d.source} onChange={(e) => setDraft(i, { source: e.target.value })}>
                  <option value="">{t.allSources}</option>{sources.map((s) => <option key={s.id}>{s.id}</option>)}
                </select>
              </td>
              <td><input aria-label={t.low} type="number" step="0.01" value={d.low} onChange={(e) => setDraft(i, { low: e.target.value })} /></td>
              <td><input aria-label={t.high} type="number" step="0.01" value={d.high} onChange={(e) => setDraft(i, { high: e.target.value })} /></td>
              <td><button className="quiet" onClick={() => setDrafts((ds) => ds.filter((_, j) => j !== i))}>{t.del}</button></td>
            </tr>
          ))}
        </tbody>
      </table>
      <p>
        <button onClick={() => setDrafts((ds) => [...ds, { template: template || "", source: "", low: "", high: "" }])}>{t.addBand}</button>{" "}
        <button onClick={saveBands}>{t.saveBands}</button>
      </p>

      <h2>{t.shadow}</h2>
      <p>{t.shadowHint}</p>
      <ul>
        {[...new Set([...templates, ...(settings?.shadow ?? [])])].map((name) => {
          const on = settings?.shadow.includes(name) ?? false;
          return (
            <li key={name}>
              <label>
                <input type="checkbox" checked={on} onChange={() => saveShadow(on ? settings!.shadow.filter((x) => x !== name) : [...(settings?.shadow ?? []), name])} /> {name}
              </label>
            </li>
          );
        })}
      </ul>
      <p>
        <input aria-label={t.shadowTemplate} placeholder={t.templateName} value={newShadow} onChange={(e) => setNewShadow(e.target.value)} />{" "}
        <button disabled={!newShadow.trim()} onClick={() => { const name = newShadow.trim(); setNewShadow(""); void saveShadow([...new Set([...(settings?.shadow ?? []), name])]); }}>{t.enableShadow}</button>
      </p>

      <h2>{t.spotCheck}</h2>
      <p>{t.spotHint}</p>
      {pool && pool.length === 0 && <p>{t.noPool}</p>}
      <ul aria-label={t.pool}>
        {(pool ?? []).map((e) => (
          <li key={e.judgment}>
            <strong>{e.subject || e.event}</strong>
            {t.entryMeta(e.source, e.template, e.judged_at)}{" "}
            {e.results.map((r) => <button key={r} onClick={() => review(e, r)}>{t.results[r] ?? r}</button>)}
          </li>
        ))}
      </ul>
    </main>
  );
}
