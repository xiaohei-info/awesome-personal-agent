import { useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 形状对应 GET /v1/packs 的 views 与 GET /v1/items（附录 B.2、C.8）。
// 列、筛选、卡片字段全部来自能力包声明；本页只按字段路径取值，不含任何包专属逻辑，新增包不改这里。
interface View { kind: string; list: { columns: string[]; filters: string[] }; card: { fields: string[] } }
interface Pack { name: string; views?: Record<string, View> | null }
type Item = { id: string; kind: string; title: string } & Record<string, unknown>

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

/** 按点路径取值：`spec.period`。缺失返回 undefined。 */
export function pick(o: unknown, path: string): unknown {
  // 附录 C.8 的筛选名 `tag` 指事项的 `tags`：单数名缺失时取复数字段。
  return path.split(".").reduce<unknown>((v, k) => {
    if (!v || typeof v !== "object") return undefined;
    const r = v as Record<string, unknown>;
    return r[k] ?? r[k + "s"];
  }, o);
}

const ISO = /^\d{4}-\d{2}-\d{2}T/;
function show(v: unknown): string {
  if (v === undefined || v === null || v === "") return "—";
  if (Array.isArray(v)) return v.map(show).join("、");
  if (typeof v === "string") return ISO.test(v) ? new Date(v).toLocaleString() : v;
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
const values = (v: unknown): string[] => (v === undefined || v === null || v === "" ? [] : Array.isArray(v) ? v.map(String) : [String(v)]);

export function PackViewsPage() {
  const { api } = useSession();
  const [packs, setPacks] = useState<Pack[] | null>(null);
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [sel, setSel] = useState("");
  const [filters, setFilters] = useState<Record<string, string>>({});

  useEffect(() => {
    Promise.all([api.get<{ packs: Pack[] }>("/packs"), api.get<{ items: Item[] }>("/items")]).then(
      ([p, i]) => { setPacks(p.packs); setItems(i.items); },
      (e) => setError(msg(e)),
    );
  }, [api]);

  if (error) return <main><p role="alert">{error}</p></main>;
  if (!packs || !items) return <main><p role="status">加载中…</p></main>;

  const views = packs.flatMap((p) => Object.entries(p.views ?? {}).map(([name, v]) => ({ key: `${p.name}/${name}`, v })));
  if (!views.length) return <main><PageHead>能力包视图</PageHead><p className="empty">没有能力包声明视图</p></main>;
  const cur = views.find((x) => x.key === sel) ?? views[0];
  const { v } = cur;
  const rows = items.filter((i) => i.kind === v.kind);
  const shown = rows.filter((i) => v.list.filters.every((f) => !filters[f] || values(pick(i, f)).includes(filters[f])));

  return (
    <main>
      <PageHead>能力包视图</PageHead>
      <div className="filters">
      <label>
        视图
        <select value={cur.key} onChange={(e) => { setSel(e.target.value); setFilters({}); }}>
          {views.map((x) => <option key={x.key}>{x.key}</option>)}
        </select>
      </label>
      {v.list.filters.map((f) => {
        const opts = [...new Set(rows.flatMap((i) => values(pick(i, f))))].sort();
        return (
          <label key={f}>
            {`筛选 ${f}`}
            <select value={filters[f] ?? ""} onChange={(e) => setFilters({ ...filters, [f]: e.target.value })}>
              <option value="">全部</option>
              {opts.map((o) => <option key={o}>{o}</option>)}
            </select>
          </label>
        );
      })}
      </div>
      {!shown.length && <p className="empty">没有符合条件的事项</p>}
      {!!shown.length && (
        <div className="panel flush">
        <table aria-label="视图列表">
          <thead><tr>{v.list.columns.map((c) => <th key={c}>{c}</th>)}</tr></thead>
          <tbody>
            {shown.map((i) => <tr key={i.id}>{v.list.columns.map((c) => <td key={c}>{show(pick(i, c))}</td>)}</tr>)}
          </tbody>
        </table>
        </div>
      )}
      <section aria-label="视图卡片">
        {shown.map((i) => (
          <article key={i.id} aria-label={`卡片 ${i.title}`}>
            <h3>{i.title}</h3>
            <dl>
              {v.card.fields.map((f) => <div key={f}><dt>{f}</dt><dd>{show(pick(i, f))}</dd></div>)}
            </dl>
          </article>
        ))}
      </section>
    </main>
  );
}
