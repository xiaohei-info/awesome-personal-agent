import { useCallback, useEffect, useState } from "react";
import { PageHead } from "../../app/page";
import { errorText, locale, useLang, useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { entryOf } from "../../app/values";
import { strings } from "./strings";

// 诊断 › 用量：GET /usage?days= 的数字只做合计与分组展示，没回报用量的只计次数、不估算。
export interface Counts { requests: number; reported: number; input_tokens: number; output_tokens: number; cache_read_tokens: number }
export interface Row extends Counts { part: Part; purpose: string; agent?: string; model?: string }
export interface Day extends Counts { part: Part; date: string }
export interface Summary { since?: string; rows: Row[]; days: Day[] }
type Part = "judge" | "model" | "agent";
const PARTS: Part[] = ["judge", "model", "agent"];
const KEYS = ["requests", "reported", "input_tokens", "output_tokens", "cache_read_tokens"] as const;

/** 按 key 合并计数，保持首次出现的顺序。 */
export function groupBy<T extends Counts>(rows: T[], key: (r: T) => string): T[] {
  const out = new Map<string, T>();
  for (const r of rows) {
    const g = out.get(key(r));
    if (!g) out.set(key(r), { ...r });
    else for (const k of KEYS) g[k] += r[k];
  }
  return [...out.values()];
}

export function UsagePage() {
  const t = useT(strings);
  const { api } = useSession();
  const [days, setDays] = useState<1 | 7 | 30>(1);
  const [data, setData] = useState<Summary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(
    (live: () => boolean) => {
      setData(null);
      setError(null);
      api.get<Summary>(`/usage?days=${days}`).then(
        (d) => live() && setData(d),
        (e) => live() && setError(errorText(e)),
      );
    },
    [api, days],
  );
  useEffect(() => {
    let on = true; // 快速换时间段时丢掉过时的响应
    load(() => on);
    return () => void (on = false);
  }, [load]);

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <fieldset className="seg-radio">
        <legend>{t.range}</legend>
        {([1, 7, 30] as const).map((n) => (
          <label key={n}>
            <input type="radio" name="usage-range" checked={days === n} onChange={() => setDays(n)} />
            {t.ranges[n]}
          </label>
        ))}
      </fieldset>
      {error ? (
        <>
          <p role="alert">{error}</p>
          <button onClick={() => load(() => true)}>{t.retry}</button>
        </>
      ) : !data ? (
        <p role="status">{t.loading}</p>
      ) : (
        <>
          <p className="sub">{data.since ? t.since(new Date(data.since).toLocaleDateString(locale())) : t.none}</p>
          {PARTS.map((p) => (
            <PartPanel key={p} part={p} rows={data.rows.filter((r) => r.part === p)} days={days === 1 ? [] : data.days.filter((d) => d.part === p)} />
          ))}
        </>
      )}
    </main>
  );
}

function PartPanel({ part, rows, days }: { part: Part; rows: Row[]; days: Day[] }) {
  const t = useT(strings);
  const lang = useLang();
  const agent = part === "agent";
  const count = agent ? t.runs : t.requests;
  const purpose = (id: string) => t.purposes[id] ?? entryOf(lang, "judgeUse", id)?.label ?? t.other;
  const name = (r: Row) => (agent ? `${r.agent || t.unnamed} · ${purpose(r.purpose)}` : purpose(r.purpose));
  const [total] = groupBy(rows, () => "");
  const max = Math.max(1, ...days.map((d) => d.input_tokens + d.output_tokens));
  const none = agent ? t.noAgentUsage : t.noUsage;
  return (
    <section aria-label={t.parts[part]} className="panel stack">
      <h2>{t.parts[part]}</h2>
      {!total ? (
        <p className="sub">{t.none}</p>
      ) : (
        <>
          <dl>
            <dt>{count}</dt><dd>{total.requests.toLocaleString(locale())}</dd>
            <dt>{t.input}</dt><dd>{tokens(total, total.input_tokens, none)}</dd>
            <dt>{t.output}</dt><dd>{tokens(total, total.output_tokens, none)}</dd>
            {total.cache_read_tokens > 0 && (<><dt>{t.cached}</dt><dd>{total.cache_read_tokens.toLocaleString(locale())}</dd></>)}
          </dl>
          {total.reported > 0 && total.reported < total.requests && <p className="sub">{t.unreported(total.requests - total.reported)}</p>}
          {/* 一个分组里可能有多个用途名落到"其他"，按显示名合并，不出现内核取值。 */}
          <Table label={`${t.parts[part]} ${agent ? t.agent : t.purpose}`} head={agent ? t.agent : t.purpose} count={count} none={none}
            rows={groupBy(rows, name).map((r) => [name(r), r])} />
          <details>
            <summary>{t.byModel}</summary>
            <Table label={`${t.parts[part]} ${t.byModel}`} head={t.model} count={count} none={none}
              rows={groupBy(rows, (r) => r.model ?? "").map((r) => [r.model || t.noModel, r])} />
          </details>
          {days.length > 0 && (
              <table aria-label={`${t.parts[part]} ${t.byDay}`}>
                <thead>
                  <tr><th>{t.date}</th><th>{count}</th><th>{t.input}</th><th>{t.output}</th><th>{t.trend}</th></tr>
                </thead>
                <tbody>
                  {days.map((d) => (
                    <tr key={d.date}>
                      <td>{d.date}</td>
                      <td>{d.requests.toLocaleString(locale())}</td>
                      <td>{tokens(d, d.input_tokens, none)}</td>
                      <td>{tokens(d, d.output_tokens, none)}</td>
                      <td><meter min={0} max={max} value={d.input_tokens + d.output_tokens} aria-label={`${d.date} ${t.trend}`} /></td>
                    </tr>
                  ))}
                </tbody>
              </table>
          )}
        </>
      )}
    </section>
  );
}

/** 一次都没回报时不写 0：那会读成没用量。 */
const tokens = (c: Counts, n: number, none: string) => (c.reported ? n.toLocaleString(locale()) : none);

function Table({ label, head, count, none, rows }: { label: string; head: string; count: string; none: string; rows: [string, Counts][] }) {
  const t = useT(strings);
  const cached = rows.some(([, r]) => r.cache_read_tokens > 0);
  return (
      <table aria-label={label}>
        <thead>
          <tr><th>{head}</th><th>{count}</th><th>{t.input}</th><th>{t.output}</th>{cached && <th>{t.cached}</th>}</tr>
        </thead>
        <tbody>
          {rows.map(([name, r]) => (
            <tr key={name}>
              <td>{name}{r.reported > 0 && r.reported < r.requests && <small> {t.unreported(r.requests - r.reported)}</small>}</td>
              <td>{r.requests.toLocaleString(locale())}</td>
              <td>{tokens(r, r.input_tokens, none)}</td>
              <td>{tokens(r, r.output_tokens, none)}</td>
              {cached && <td>{r.cache_read_tokens.toLocaleString(locale())}</td>}
            </tr>
          ))}
        </tbody>
      </table>
  );
}
