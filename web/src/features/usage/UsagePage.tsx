import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Skeleton } from "../../app/ui";
import { PageHead } from "../../app/page";
import { errorText, locale, useLang, useT } from "../../app/i18n";
import { useRaw } from "../../app/raw";
import { useSession } from "../../app/session";
import { entryOf } from "../../app/values";
import { Donut, MODEL_COLORS, PART_COLORS, SEGMENT_COLORS, Trend, type Bucket, type Series } from "./charts";
import { allUnprovided, cacheRate, PARTS, rank, SEGMENTS, split, sum, total, unprovided, unreported, type Counts, type Group, type Part, type Row, type Summary } from "./data";
import { strings } from "./strings";

type T = (typeof strings)["zh"];
/** 一次都没有用量时怎么写：全是未提供写"未提供"，否则"未回报"。 */
const noneOf = (t: T, c: Counts, agent: boolean) => (allUnprovided(c) ? (agent ? t.agentNotProvided : t.notProvided) : agent ? t.noAgentUsage : t.noUsage);
/** 部分没有用量时的提示，未回报与未提供分开写。 */
function Gaps({ c, hideUnreported }: { c: Counts; hideUnreported?: boolean }) {
  const t = useT(strings);
  const parts = [!hideUnreported && unreported(c) > 0 && t.unreported(unreported(c)), unprovided(c) > 0 && t.unprovided(unprovided(c))].filter(Boolean);
  return parts.length ? <span className="usage-warn"> {parts.join(" · ")}</span> : null;
}

type Days = 1 | 7 | 30;
type Use = "all" | Part;
const USES: Use[] = ["all", ...PARTS];
const TOP_USES = 6;
const TOP_MODELS = 5;

const num = (n: number) => n.toLocaleString(locale());
const short = (n: number) => new Intl.NumberFormat(locale(), { notation: "compact", maximumFractionDigits: 1 }).format(n);
const percent = (n: number) => new Intl.NumberFormat(locale(), { style: "percent", maximumFractionDigits: 1 }).format(n);

export function UsagePage() {
  const t = useT(strings);
  const { api } = useSession();
  const [days, setDays] = useState<Days>(7);
  const [use, setUse] = useState<Use>("all");
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
      <div className="usage-controls">
        <Choice legend={t.range} name="usage-range" values={[1, 7, 30]} value={days} label={(n) => t.ranges[n]} onChange={setDays} />
        <Choice legend={t.use} name="usage-use" values={USES} value={use} label={(u) => t.uses[u]} onChange={setUse} />
      </div>
      {error ? (
        <>
          <p role="alert">{error}</p>
          <button onClick={() => load(() => true)}>{t.retry}</button>
        </>
      ) : !data ? (
        <Skeleton shape="chart" label={t.loading} />
      ) : (
        <>
          {data.since && <p className="sub">{t.since(new Date(data.since).toLocaleDateString(locale()))}</p>}
          <Report data={data} days={days} use={use} week={() => setDays(7)} />
        </>
      )}
    </main>
  );
}

function Choice<T extends string | number>({ legend, name, values, value, label, onChange }: {
  legend: string; name: string; values: T[]; value: T; label: (v: T) => string; onChange: (v: T) => void;
}) {
  return (
    <fieldset className="seg-radio">
      <legend>{legend}</legend>
      {values.map((v) => (
        <label key={v}>
          <input type="radio" name={name} checked={value === v} onChange={() => onChange(v)} />
          {label(v)}
        </label>
      ))}
    </fieldset>
  );
}

/** 用途名按词表称呼，认不出的归"其他"，不出现内核取值。 */
function useNames() {
  const t = useT(strings);
  const lang = useLang();
  return (r: Row) => {
    const p = t.purposes[r.purpose] ?? entryOf(lang, "judgeUse", r.purpose)?.label ?? t.other;
    return r.part === "agent" ? `${r.agent || t.unnamed} · ${p}` : p;
  };
}

function Report({ data, days, use, week }: { data: Summary; days: Days; use: Use; week: () => void }) {
  const t = useT(strings);
  const name = useNames();
  const rows = data.rows.filter((r) => use === "all" || r.part === use);
  const all = sum(rows);
  if (!all.requests)
    return (
      <div className="usage-empty">
        <p>{t.none}</p>
        {days === 1 && <button onClick={week}>{t.seeWeek}</button>}
      </div>
    );
  // 一次都没回报时按次数画：画 0 会读成没用量。
  const byCount = !all.reported;
  const none = noneOf(t, all, use === "agent");
  return (
    <>
      <Cards rows={rows} all={all} use={use} none={none} />
      <TrendPanel data={data} days={days} use={use} byCount={byCount} />
      <div className="split usage-split">
        <Uses rows={rows} all={all} byCount={byCount} />
        <Models rows={rows} all={all} byCount={byCount} />
      </div>
      <Details data={data} rows={rows} days={days} use={use} name={name} />
    </>
  );
}

function Cards({ rows, all, use, none }: { rows: Row[]; all: Counts; use: Use; none: string }) {
  const t = useT(strings);
  const name = useNames();
  const rate = cacheRate(all);
  const byCount = !all.reported;
  const top = rank(rows, (r) => (use === "all" ? t.uses[r.part] : name(r)), byCount)[0];
  const size = (c: Counts) => (byCount ? c.requests : total(c));
  const runs = sum(rows.filter((r) => r.part === "agent")).requests;
  return (
    <div className="usage-cards">
      <Card label={t.totalTokens} value={byCount ? none : total(all) >= 1e6 ? short(total(all)) : num(total(all))} title={byCount ? undefined : num(total(all))}>
        {!byCount && t.inOut(short(all.input_tokens), short(all.output_tokens))}
      </Card>
      <Card label={t.calls} value={t.times(num(all.requests))}>
        {use === "all" && t.callSplit(num(all.requests - runs), num(runs))}
        <Gaps c={all} />
      </Card>
      <Card label={t.cacheRate} value={rate === undefined ? "—" : percent(rate)}>
        {rate === undefined ? t.noCache : t.readTokens(short(all.cache_read_tokens))}
        {rate !== undefined && all.cache_write_tokens > 0 && ` · ${t.reuse((all.cache_read_tokens / all.cache_write_tokens).toFixed(1))}`}
      </Card>
      <Card label={t.top} value={top.key}>
        {size(top) > 0 && `${t.share(percent(size(top) / size(all)))} · ${byCount ? t.times(num(top.requests)) : short(total(top))}`}
      </Card>
    </div>
  );
}

function Card({ label, value, title, children }: { label: string; value: string; title?: string; children?: ReactNode }) {
  return (
    <section className="panel usage-card" aria-label={label}>
      <h2>{label}</h2>
      <b title={title}>{value}</b>
      <p>{children}</p>
    </section>
  );
}

function Legend({ series, marker }: { series: Series[]; marker?: boolean }) {
  const t = useT(strings);
  return (
    <ul className="usage-legend">
      {series.map((s) => (
        <li key={s.key}><i style={{ background: s.color }} />{s.label}</li>
      ))}
      {marker && <li><i className="hollow" />{t.hasUnreported}</li>}
    </ul>
  );
}

function TrendPanel({ data, days, use, byCount }: { data: Summary; days: Days; use: Use; byCount: boolean }) {
  const t = useT(strings);
  const [stack, setStack] = useState<"tokens" | "use">("tokens");
  const [picked, setPicked] = useState<string>();
  const byUse = byCount || (use === "all" && stack === "use");
  const parts = use === "all" ? PARTS : [use];
  const series: Series[] = byUse
    ? parts.map((p) => ({ key: p, label: t.uses[p], color: PART_COLORS[p] }))
    : SEGMENTS.map((k) => ({ key: k, label: t.segments[k], color: SEGMENT_COLORS[k] }));
  const buckets = data.slots.map((slot) => {
    const points = data.series.filter((p) => p.slot === slot && (use === "all" || p.part === use));
    const c = sum(points);
    const values: Record<string, number> = byUse
      ? Object.fromEntries(parts.map((p) => { const s = sum(points.filter((x) => x.part === p)); return [p, byCount ? s.requests : total(s)]; }))
      : split(c);
    return { slot, counts: c, values, bar: { ...values, slot, unreported: byCount ? 0 : c.requests - c.reported } as Bucket };
  });
  const height = (b: Bucket) => series.reduce((n, s) => n + Number(b[s.key]), 0);
  const tick = (slot: string) => (days === 1 ? `${slot}:00` : slot.slice(5));
  const title = byCount ? (days === 1 ? t.hourlyCalls : t.dailyCalls) : days === 1 ? t.hourly : t.daily;
  const sel = buckets.find((b) => b.slot === picked);
  const missing = sel ? sel.counts.requests - sel.counts.reported : 0;
  return (
    <section className="panel usage-trend" aria-label={title}>
      <header>
        <h2>{title}</h2>
        {use === "all" && !byCount && (
          <div className="seg" role="group" aria-label={t.stack}>
            {(["tokens", "use"] as const).map((s) => (
              <button key={s} aria-pressed={stack === s} onClick={() => setStack(s)}>{t.stacks[s]}</button>
            ))}
          </div>
        )}
      </header>
      {byCount && <p role="note">{t.countNote}</p>}
      <Trend data={buckets.map((b) => b.bar)} series={series} height={height} tick={tick} format={short} onPick={setPicked} />
      <Legend series={series} marker={buckets.some((b) => b.bar.unreported > 0)} />
      <p className="usage-picked" aria-live="polite">
        {!sel ? t.pick : (
          <>
            <b>{tick(sel.slot)}</b>
            {!sel.counts.requests ? ` ${t.noneShort}` : !sel.counts.reported ? ` ${t.times(num(sel.counts.requests))} · ${noneOf(t, sel.counts, false)}` : (
              <>
                {` ${t.totalShort(num(total(sel.counts)))}`}
                {series.map((s) => ` · ${s.label} ${num(sel.values[s.key])}`)}
                {missing > 0 && <span className="usage-warn"> {t.maybeMore(missing)}</span>}
              </>
            )}
          </>
        )}
      </p>
    </section>
  );
}

function Uses({ rows, all, byCount }: { rows: Row[]; all: Counts; byCount: boolean }) {
  const t = useT(strings);
  const name = useNames();
  const raw = useRaw();
  const groups = rank(rows, name, byCount);
  const size = (c: Counts) => (byCount ? c.requests : total(c));
  const max = Math.max(1, ...groups.map(size));
  const item = (g: Group<Row>) => {
    const silent = !g.reported;
    const s = split(g);
    return (
      <li key={g.key}>
        <div>
          <span>{g.key}{raw && <small className="raw-id">{[...new Set(g.items.map((r) => r.purpose))].join(" ")}</small>}</span>
          <span>
            {silent ? `${t.times(num(g.requests))} · ${noneOf(t, g, g.items[0].part === "agent")}` : `${short(total(g))} · ${percent(total(g) / (total(all) || 1))}`}
          </span>
        </div>
        <div className={silent && !byCount ? "usage-bar silent" : "usage-bar"} aria-hidden="true">
          {byCount ? <i style={{ width: `${(g.requests / max) * 100}%`, background: PART_COLORS[g.items[0].part] }} />
            : !silent && SEGMENTS.map((k) => <i key={k} style={{ width: `${(s[k] / max) * 100}%`, background: SEGMENT_COLORS[k] }} />)}
        </div>
        {!silent && <small><Gaps c={g} /></small>}
      </li>
    );
  };
  const rest = groups.slice(TOP_USES);
  return (
    <section className="panel" aria-label={t.byUse}>
      <h2>{t.byUse}</h2>
      <ul className="usage-list">{groups.slice(0, TOP_USES).map(item)}</ul>
      {rest.length > 0 && (
        <details>
          <summary>{t.more(rest.length)}</summary>
          <ul className="usage-list">{rest.map(item)}</ul>
        </details>
      )}
    </section>
  );
}

function Models({ rows, all, byCount }: { rows: Row[]; all: Counts; byCount: boolean }) {
  const t = useT(strings);
  const ranked = rank(rows, (r) => r.model || t.noModel, byCount);
  const rest = ranked.slice(TOP_MODELS);
  // 只剩一个时照常列名字，不藏进"其他"。
  const groups = rest.length > 1 ? [...ranked.slice(0, TOP_MODELS), { key: t.otherModels(rest.length), ...sum(rest) }] : ranked;
  const size = (c: Counts) => (byCount ? c.requests : total(c));
  const colored = groups.map((g, i) => ({ ...g, fill: MODEL_COLORS[i] ?? MODEL_COLORS[MODEL_COLORS.length - 1] }));
  return (
    <section className="panel" aria-label={t.byModel}>
      <h2>{t.byModel}</h2>
      <Donut data={colored.filter((g) => size(g) > 0).map((g) => ({ name: g.key, value: size(g), fill: g.fill }))}
        center={byCount ? t.times(short(all.requests)) : short(total(all))} />
      <ul className="usage-models">
        {colored.map((g) => {
          const rate = cacheRate(g);
          return (
            <li key={g.key}>
              <i style={{ background: g.fill }} />
              <span>{g.key}</span>
              <small>
                {byCount ? `${t.times(num(g.requests))} · ${noneOf(t, g, false)}` : !g.reported ? noneOf(t, g, false)
                  : `${percent(total(g) / (total(all) || 1))} · ${rate === undefined ? t.noCacheShort : t.hit(percent(rate))}`}
                {g.reported > 0 && <Gaps c={g} />}
              </small>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function Details({ data, rows, days, use, name }: { data: Summary; rows: Row[]; days: Days; use: Use; name: (r: Row) => string }) {
  const t = useT(strings);
  const raw = useRaw();
  const lines = rank(rows, (r) => JSON.stringify([r.part, name(r), r.model ?? ""]), false);
  const heads = [t.calls, t.missing, t.notProvidedShort, t.input, t.segments.read, t.segments.write, t.output, t.cacheRate];
  /** 一次都没回报时不写 0：那会读成没用量。 */
  const cells = (c: Counts) => {
    const rate = cacheRate(c);
    const silent = allUnprovided(c) ? t.notProvidedShort : t.silent;
    const token = (n: number) => (c.reported ? num(n) : silent);
    return [num(c.requests), num(unreported(c)), num(unprovided(c)), token(c.input_tokens), token(c.cache_read_tokens), token(c.cache_write_tokens),
      token(c.output_tokens), !c.reported ? silent : rate === undefined ? "—" : percent(rate)].map((v, i) => <td key={i}>{v}</td>);
  };
  const slots = data.slots
    .map((slot) => ({ slot, c: sum(data.series.filter((p) => p.slot === slot && (use === "all" || p.part === use))) }))
    .filter((s) => s.c.requests > 0);
  const when = days === 1 ? t.hour : t.date;
  return (
    <details className="panel usage-details">
      <summary>{t.details}</summary>
      <table aria-label={t.byUse}>
        <thead><tr>{[t.use, t.purpose, t.model, ...heads].map((h) => <th key={h}>{h}</th>)}</tr></thead>
        <tbody>
          {lines.map((g) => {
            const r = g.items[0];
            const ids = (list: (string | undefined)[]) => [...new Set(list)].join(" ");
            return (
              <tr key={g.key}>
                <td>{t.uses[r.part]}</td>
                <td>{name(r)}{raw && <small className="raw-id">{ids(g.items.map((x) => x.purpose))}</small>}</td>
                <td>{r.model || t.noModel}</td>
                {cells(g)}
              </tr>
            );
          })}
        </tbody>
      </table>
      <table aria-label={when}>
        <thead><tr>{[when, ...heads].map((h) => <th key={h}>{h}</th>)}</tr></thead>
        <tbody>
          {slots.map((s) => (
            <tr key={s.slot}><td>{days === 1 ? `${s.slot}:00` : s.slot}</td>{cells(s.c)}</tr>
          ))}
        </tbody>
      </table>
    </details>
  );
}
