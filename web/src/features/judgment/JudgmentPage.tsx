import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import "./judgment.css";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { EntityPick, useNames } from "../../app/names";
import { useLabel } from "../../app/values";
import { JUDGE_TEMPLATE, PACK_BAND } from "../../app/ids";
import { strings } from "./strings";

// 筛选门槛（原「判断与校准」去掉抽检后的部分，设计 §5.16）：形状对应 GET /v1/judgments/stats 与 /v1/settings/judgment（附录 B.2，主文档 §8.10）。
// 不确定带与影子模式的写入语义不变：PUT 只带 bands（整份列表）或 shadow（整份列表），校验在服务端；本页只换了编辑方式。
interface Row { use: string; template: string; source?: string; backend: string; calls: number; decided: number; uncertain: number; errors: number; reused: number }
interface Rate { use: string; template: string; source?: string; escalation: number; undecided: number; acceptance: number; false_drop: number; reviewed: number }
interface Bucket { use: string; template: string; source?: string; low: number; high: number; count: number }
interface Stats { rows: Row[]; rates: Rate[]; histogram: Bucket[] }
export interface Band { template: string; source?: string; low: number; high: number }
interface Settings { bands: Band[]; shadow: string[]; spot_check: { per_brief: number } }

const WINDOWS = [7, 30, 90];
const pct = (v: number) => `${(v * 100).toFixed(1)}%`;
const same = (b: { template: string; source?: string }, template: string, source: string) => b.template === template && (b.source ?? "") === source;

/** 拖线时的估算：按桶中点落在哪一段计数，结果是近似（标「约」）。 */
export function estimate(buckets: Bucket[], low: number, high: number): [number, number, number] {
  let a = 0, b = 0, c = 0;
  for (const k of buckets) {
    const mid = (k.low + k.high) / 2;
    if (mid < low) a += k.count;
    else if (mid > high) c += k.count;
    else b += k.count;
  }
  return [a, b, c];
}

/** 保存一条线后的整份不确定带：替换（或新增）这个模板在这个来源上的那一条，其余原样保留。 */
export function withBand(bands: Band[], next: Band): Band[] {
  const rest = bands.filter((b) => !same(b, next.template, next.source ?? ""));
  return [...rest, next];
}

function Card({ template, rate, buckets, band, override, days, shadow, onSave, onReset, onShadow }: {
  template: string;
  rate?: Rate;
  buckets: Bucket[];
  band: { low: number; high: number };
  override: boolean;
  days: number;
  shadow: boolean;
  onSave: (low: number, high: number) => Promise<void>;
  onReset: () => Promise<void>;
  onShadow: (on: boolean) => Promise<void>;
}) {
  const t = useT(strings);
  const label = useLabel();
  const [low, setLow] = useState(band.low);
  const [high, setHigh] = useState(band.high);
  useEffect(() => { setLow(band.low); setHigh(band.high); }, [band.low, band.high]);
  const question = label("judgeTemplate", template);
  const max = Math.max(1, ...buckets.map((b) => b.count));
  const [a, b, c] = estimate(buckets, low, high);
  const changed = low !== band.low || high !== band.high;
  return (
    <section aria-label={question} className="panel threshold">
      <h2>{question}</h2>
      {rate ? (
        <div className="figures">
          <p><span className="big">{pct(rate.false_drop)}</span><span className="sub">{t.missed} · {t.checked(rate.reviewed)}</span></p>
          <p><span className="big">{pct(rate.acceptance)}</span><span className="sub">{t.accepted}</span></p>
        </div>
      ) : (
        <p className="sub">{t.noData}</p>
      )}
      {buckets.length > 0 && (
        <div className="histogram" role="img" aria-label={t.chart(question)}>
          {buckets.map((k) => (
            <span key={k.low} className={(k.low + k.high) / 2 < low ? "below" : (k.low + k.high) / 2 > high ? "above" : "between"} style={{ left: `${k.low * 100}%`, width: `calc(${(k.high - k.low) * 100}% - 2px)`, height: `${Math.max(4, (k.count / max) * 100)}%` }} title={`${k.low.toFixed(1)}–${k.high.toFixed(1)}: ${k.count}`} />
          ))}
          <i className="line" style={{ left: `${low * 100}%` }} />
          <i className="line" style={{ left: `${high * 100}%` }} />
        </div>
      )}
      <div className="sliders">
        <label>{t.low}<input type="range" min={0} max={1} step={0.01} value={low} onChange={(e) => setLow(Number(e.target.value))} /></label>
        <label>{t.high}<input type="range" min={0} max={1} step={0.01} value={high} onChange={(e) => setHigh(Number(e.target.value))} /></label>
      </div>
      <p className="sub">
        {t.lines(low.toFixed(2), high.toFixed(2))}
        {!override && !changed ? t.packDefault : ""}
      </p>
      {buckets.length > 0 && <p role="status">{template === "triage-screen" ? t.estimateTriage(days, a, b, c) : t.estimate(days, a, b, c)}</p>}
      <div className="actions">
        <button className="primary" disabled={!changed || low > high} onClick={() => void onSave(low, high)}>{t.saveLines}</button>
        {override && <button onClick={() => void onReset()}>{t.useDefault}</button>}
        {low > high && <span role="alert">{t.invalid}</span>}
      </div>
      <label className="check">
        <input type="checkbox" checked={shadow} onChange={() => void onShadow(!shadow)} /> {t.trial}
      </label>
    </section>
  );
}

export function JudgmentPage() {
  const t = useT(strings);
  const label = useLabel();
  const sourceName = useNames("source");
  const { api } = useSession();
  const [days, setDays] = useState(30);
  const [source, setSource] = useState("");
  const [stats, setStats] = useState<Stats | null>(null);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const guard = useCallback(async (f: () => Promise<void>) => {
    try {
      await f();
      setError(null);
    } catch (e) {
      setError(e);
    }
  }, []);
  const loadStats = useCallback(async () => {
    const q = new URLSearchParams({ since: `${days}d` });
    if (source) q.set("source", source);
    setStats(await api.get<Stats>(`/judgments/stats?${q}`));
  }, [api, days, source]);
  const loadSettings = useCallback(async () => setSettings(await api.get<Settings>("/settings/judgment")), [api]);
  useEffect(() => { void guard(loadStats); }, [guard, loadStats]);
  useEffect(() => { void guard(loadSettings); }, [guard, loadSettings]);

  const putBands = (bands: Band[]) =>
    guard(async () => {
      setNotice(null);
      await api.put("/settings/judgment", { bands });
      await loadSettings();
      setNotice(t.saved);
    });
  const putShadow = (shadow: string[]) =>
    guard(async () => {
      setNotice(null);
      await api.put("/settings/judgment", { shadow });
      await loadSettings();
    });

  const rates = stats?.rates ?? [];
  const templates = [...new Set([...JUDGE_TEMPLATE, ...rates.map((r) => r.template), ...(settings?.bands ?? []).map((b) => b.template), ...(settings?.shadow ?? [])])];
  const bandOf = (template: string) => {
    const own = settings?.bands.find((b) => same(b, template, source));
    const any = settings?.bands.find((b) => same(b, template, ""));
    const pack = PACK_BAND[template] ?? {};
    const b = own ?? any;
    return { band: b ? { low: b.low, high: b.high } : { low: pack.low ?? 0, high: pack.high ?? 1 }, override: !!own };
  };

  return (
    <main className="judgment">
      <PageHead>{t.title}</PageHead>
      <div className="filters">
        <label>
          {t.window}
          <select value={days} onChange={(e) => setDays(Number(e.target.value))}>
            {WINDOWS.map((w) => <option key={w} value={w}>{t.days(w)}</option>)}
          </select>
        </label>
        <label>
          {t.source}
          <EntityPick kind="source" value={source} onChange={setSource} empty={t.allSources} />
        </label>
      </div>
      {error != null && <ErrorNote error={error} />}
      {notice && <p role="status">{notice}</p>}
      {(!stats || !settings) && error == null && <p role="status">{t.loading}</p>}
      {stats && settings &&
        templates.map((tpl) => {
          const rate = rates.find((r) => r.template === tpl);
          const buckets = (stats.histogram ?? []).filter((k) => k.template === tpl && (k.source ?? "") === (rate?.source ?? source));
          const { band, override } = bandOf(tpl);
          return (
            <Card
              key={tpl + "|" + source}
              template={tpl}
              rate={rate}
              buckets={buckets}
              band={band}
              override={override}
              days={days}
              shadow={settings.shadow.includes(tpl)}
              onSave={(low, high) => putBands(withBand(settings.bands, { template: tpl, ...(source ? { source } : {}), low, high }))}
              onReset={() => putBands(settings.bands.filter((b) => !same(b, tpl, source)))}
              onShadow={(on) => putShadow(on ? [...settings.shadow, tpl] : settings.shadow.filter((x) => x !== tpl))}
            />
          );
        })}
      {stats && (stats.rates.length > 0 || stats.rows.length > 0) && (
        <details className="panel">
          <summary>{t.details}</summary>
          {stats.rates.length > 0 && (
            <table aria-label={t.rates}>
              <thead><tr>{t.rateCols.map((c) => <th key={c}>{c}</th>)}</tr></thead>
              <tbody>
                {stats.rates.map((r) => (
                  <tr key={r.use + r.template + (r.source ?? "")}>
                    <td>{label("judgeTemplate", r.template)}</td><td>{r.source ? sourceName(r.source) : t.allSources}</td>
                    <td>{pct(r.escalation)}</td><td>{pct(r.undecided)}</td><td>{pct(r.acceptance)}</td><td>{pct(r.false_drop)}</td><td>{r.reviewed}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {stats.rows.length > 0 && (
            <table aria-label={t.calls}>
              <thead><tr>{t.callCols.map((c) => <th key={c}>{c}</th>)}</tr></thead>
              <tbody>
                {stats.rows.map((r) => (
                  <tr key={r.use + r.template + (r.source ?? "") + r.backend}>
                    <td>{label("judgeTemplate", r.template)}</td><td>{r.source ? sourceName(r.source) : t.allSources}</td><td>{label("judgeBackend", r.backend)}</td>
                    <td>{r.calls}</td><td>{r.decided}</td><td>{r.uncertain}</td><td>{r.errors}</td><td>{r.reused}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </details>
      )}
    </main>
  );
}
