import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import type { ApiClient } from "../../app/api";
import { lang, locale, useT } from "../../app/i18n";
import { common } from "../../app/strings";
import { useLabel } from "../../app/values";
import { useNames } from "../../app/names";
import { strings } from "./strings";

// 事项列表的类型筛选（原「能力包视图」，设计 §3.2）：形状对应 GET /v1/packs 的 views（附录 B.2、C.8）。
// 列、筛选、卡片字段全部来自能力包声明；这里只按字段路径取值，不含任何包专属逻辑，新增包不改这里。
export interface View { kind: string; list: { columns: string[]; filters: string[] }; card: { fields: string[] } }
interface Pack { name: string; views?: Record<string, View> | null }
export interface TypeView { key: string; v: View }
type Row = { id: string; kind: string; title: string } & Record<string, unknown>;

/** 全部能力包声明的视图，键是 `<包>/<视图名>`；取不到时为空列表（类型筛选不显示）。 */
export function useViews(api: ApiClient): TypeView[] {
  const [views, setViews] = useState<TypeView[]>([]);
  useEffect(() => {
    api.get<{ packs: Pack[] }>("/packs").then(
      (r) => setViews(r.packs.flatMap((p) => Object.entries(p.views ?? {}).map(([name, v]) => ({ key: `${p.name}/${name}`, v })))),
      () => setViews([]),
    );
  }, [api]);
  return views;
}

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
  if (Array.isArray(v)) return v.map(show).join(common[lang()].listSep);
  if (typeof v === "string") return ISO.test(v) ? new Date(v).toLocaleString(locale()) : v;
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
const values = (v: unknown): string[] => (v === undefined || v === null || v === "" ? [] : Array.isArray(v) ? v.map(String) : [String(v)]);

/** 一种类型的事项：视图声明的筛选、列表列与卡片字段；字段名走词表，查不到显示原名。 */
export function ViewTable({ view, items: list }: { view: TypeView; items: { id: string; kind: string; title: string }[] }) {
  const items = list as Row[]; // 视图按字段路径取值，事项其余字段原样来自 GET /items
  const t = useT(strings);
  const label = useLabel();
  const project = useNames("project");
  const [filters, setFilters] = useState<Record<string, string>>({});
  useEffect(() => setFilters({}), [view.key]);
  const { v } = view;
  const rows = items.filter((i) => i.kind === v.kind);
  const shown = rows.filter((i) => v.list.filters.every((f) => !filters[f] || values(pick(i, f)).includes(filters[f])));
  const text = (f: string, x: unknown) => (f === "phase" && typeof x === "string" ? label("phase", x) : f === "project" && typeof x === "string" ? project(x) : show(x));
  const cell = (i: Row, f: string) => (f === "title" ? <Link to={i.id}>{i.title}</Link> : text(f, pick(i, f)));
  return (
    <>
      {v.list.filters.length > 0 && (
        <div className="filters">
          {v.list.filters.map((f) => {
            const opts = [...new Set(rows.flatMap((i) => values(pick(i, f))))].sort();
            return (
              <label key={f}>
                {label("viewField", f)}
                <select value={filters[f] ?? ""} onChange={(e) => setFilters({ ...filters, [f]: e.target.value })}>
                  <option value="">{t.all}</option>
                  {opts.map((o) => <option key={o} value={o}>{text(f, o)}</option>)}
                </select>
              </label>
            );
          })}
        </div>
      )}
      {!shown.length && <p className="empty">{t.noMatch}</p>}
      {!!shown.length && (
        <div className="panel flush">
          <table aria-label={t.viewTable(label("itemKind", v.kind))}>
            <thead><tr>{v.list.columns.map((c) => <th key={c}>{label("viewField", c)}</th>)}</tr></thead>
            <tbody>
              {shown.map((i) => <tr key={i.id}>{v.list.columns.map((c) => <td key={c}>{cell(i, c)}</td>)}</tr>)}
            </tbody>
          </table>
        </div>
      )}
      {v.card.fields.length > 0 && (
        <section aria-label={t.viewCards}>
          {shown.map((i) => (
            <article key={i.id} aria-label={i.title}>
              <h3><Link to={i.id}>{i.title}</Link></h3>
              <dl>
                {v.card.fields.map((f) => <div key={f}><dt>{label("viewField", f)}</dt><dd>{text(f, pick(i, f))}</dd></div>)}
              </dl>
            </article>
          ))}
        </section>
      )}
    </>
  );
}
