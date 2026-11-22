import { RejectionNote, type Rejection } from "../decisions/RejectionNote";
import { LastRunNote } from "./LastRunNote";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, Route, Routes, useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { isMaterialId } from "../../app/relations";
import { AttachedMessages } from "../attached-messages";
import { Markdown } from "./Markdown";
import { BriefBody } from "../briefs/BriefBody";
import { ProgressForm } from "../today/ProgressForm";
import { locale, useT } from "../../app/i18n";
import { strings } from "./strings";
import { HANDOFF_TAG, WAITING } from "../../app/ids";
import { Pick, Val, useLabel } from "../../app/values";
import { ErrorNote } from "../../app/errors";
import { EntityPick, useItemTitle, useNames } from "../../app/names";
import { ViewTable, useViews } from "./TypeViews";
import { AgentBlock, tagName } from "./HandOff";
import { QuestionCard, useAnswer, type DecisionItem } from "../decisions/DecisionsPage";
import { TraceView } from "../trace/Trace";
import { ItemMaterials, MATERIAL_RELATIONS, type MaterialRel, type MaterialRow } from "./ItemMaterials";
import { linkTitle, readPrefill, registrable, withLink, type Vault } from "./prefill";
import { ScheduleBlock } from "./ScheduleBlock";
import { deadlineText, deadlineValue, hhmm, parseSlot, slotOf, useHostClock } from "./schedule";
import { ApiError } from "../../app/api";

// 形状对应 GET /v1/items、/v1/items/{id}、/v1/why/{id}（附录 B.2）。校验、授权、版本冲突、阶段进入约束都在服务端，
// 本页只搬运输入并显示服务端的错误原因。
export interface Item {
  preferred_agent_id?: string;
  id: string;
  phase: string;
  version: number;
  kind: string;
  title: string;
  status_label?: string;
  priority?: string;
  project?: string;
  tags?: string[];
  deadline?: string | null;
  planned_for?: string;
  planned_slot?: string;
  body?: string;
  spec?: { sections?: { project?: string; title?: string; body: string }[] };
  waiting_on?: { kind: string; ref: string };
  ask?: DecisionItem["ask"];
  origin?: { source: string; ref?: string };
}
interface Relation { type: string; from: string; to: string }
export interface Step {
  seq: number;
  type: string;
  actor: { type: string; principal: string; via: string };
  at: string;
  reason?: string;
}

const REL_TYPES = ["relates", "parent", "blocks", "derived_from", "supersedes"];


/** 每个变更的操作者徽章：谁、从哪个入口。 */
export function ActorBadge({ actor }: { actor: Step["actor"] }) {
  const t = useT(strings);
  const label = useLabel();
  const credential = useNames("credential");
  const key = useNames("key");
  // agent 显示它所属 Agent 的名字，钥匙名放在悬停里；本人与系统不再重复显示身份串。
  const who = actor.type === "agent" ? credential(actor.principal) : "";
  return (
    <span aria-label={t.actorLabel} data-actor={actor.type} className="badge plain" title={who ? key(actor.principal) : undefined}>
      {[label("actorType", actor.type), label("via", actor.via), who].filter(Boolean).join(" · ")}
    </span>
  );
}

export function ItemsPage() {
  return (
    <Routes>
      <Route index element={<ItemList />} />
      <Route path="new" element={<NewItem />} />
      <Route path=":id" element={<ItemDetail />} />
    </Routes>
  );
}

// 事项列表的筛选维度：全文、排序、计划日期在服务端做（与 CLI、HTTP 一致）；其余在取回的列表上筛和数。
// shortcut: 一次取回全部事项再分页，个人规模（数百条）够用；上万条时要服务端分页。
const PAGE_SIZE = 50;
const DIMS = ["project", "kind", "phase", "priority", "planned", "origin", "tag"] as const;
type Dim = (typeof DIMS)[number];
const NONE = "-";
// 来源按 origin.source 归组；定时安排建的（instance:<实例>）合成一组。
const originKey = (s?: string) => (!s ? NONE : s.startsWith("instance:") ? "instance:" : s);
const valuesOf = (i: Item, d: Dim): string[] =>
  d === "tag" ? i.tags ?? []
  : d === "planned" ? [i.planned_for ? "yes" : "no"]
  : d === "origin" ? [originKey(i.origin?.source)]
  : [(i[d] as string | undefined) || NONE];
const keep = (i: Item, f: Partial<Record<Dim, string>>, skip?: Dim) => DIMS.every((d) => d === skip || !f[d] || valuesOf(i, d).includes(f[d]!));

function ItemList() {
  const t = useT(strings);
  const { api } = useSession();
  const location = useLocation();
  const [params, setParams] = useSearchParams();
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const views = useViews(api);
  const label = useLabel();
  const project = useNames("project");
  const source = useNames("source");
  const { utc_offset: offset } = useHostClock(api);
  const get = (k: string) => params.get(k) ?? "";
  const q = get("q"), sort = get("sort"), plannedFor = get("date");
  const view = views.find((x) => x.key === get("view"));
  const f = Object.fromEntries(DIMS.map((d) => [d, get(d)])) as Record<Dim, string>;
  // 改任何筛选都回到第一页；值为空即去掉这一项。
  const set = (next: Record<string, string>) => {
    const p = new URLSearchParams(params);
    for (const [k, v] of Object.entries(next)) (v ? p.set(k, v) : p.delete(k));
    if (!("page" in next)) p.delete("page");
    setParams(p, { replace: true });
  };

  useEffect(() => {
    setError(null);
    const qs = new URLSearchParams();
    for (const [k, v] of [["q", q.trim()], ["sort", sort], ["planned_for", plannedFor]]) if (v) qs.set(k, v);
    api.get<{ items: Item[] }>("/items" + (qs.size ? "?" + qs : "")).then(
      (r) => setItems(r.items),
      (e) => setError(e),
    );
  }, [api, q, sort, plannedFor]);

  const all = items ?? [];
  const shown = all.filter((i) => keep(i, f));
  const name = (d: Dim, v: string): string =>
    v === NONE ? t.none[d] ?? t.notSet
    : d === "project" ? project(v)
    : d === "kind" ? label("itemKind", v)
    : d === "phase" ? label("phase", v)
    : d === "priority" ? label("priority", v)
    : d === "planned" ? t.plannedHas[v as "yes" | "no"]
    : d === "origin" ? (v === "instance:" ? t.fromSchedule : source(v))
    : tagName(v, t.handoffName);
  // 每个维度的选项按「其余筛选都生效时」的条数从多到少排；只剩一种取值且没被选中的维度不显示。
  const facets = DIMS.map((d) => {
    const n = new Map<string, number>();
    for (const i of all) if (keep(i, f, d)) for (const v of valuesOf(i, d)) n.set(v, (n.get(v) ?? 0) + 1);
    return { d, opts: [...n.entries()].sort((a, b) => b[1] - a[1] || name(d, a[0]).localeCompare(name(d, b[0]))) };
  }).filter((x) => x.opts.length > 1 || f[x.d]);
  const active = DIMS.filter((d) => f[d]);
  const filtered = active.length > 0 || !!q || !!plannedFor || !!view;
  const pages = Math.max(1, Math.ceil(shown.length / PAGE_SIZE));
  const page = Math.min(pages, Math.max(1, Number(get("page")) || 1));
  const rows = shown.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE);

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      <p><Link to="new"><button type="button">{t.create}</button></Link></p>
      {views.length > 0 && (
        <nav className="chips" aria-label={t.quick}>
          <button type="button" aria-pressed={!view} onClick={() => set({ view: "" })}>{t.allTypes}</button>
          {views.map((x) => (
            // 快捷视图按能力包声明的列与筛选显示（TypeViews），通用筛选随之清空，免得两套同名筛选叠在一起。
            <button key={x.key} type="button" aria-pressed={x === view} onClick={() => set({ view: x === view ? "" : x.key, ...Object.fromEntries(DIMS.map((d) => [d, ""])) })}>
              {label("itemKind", x.v.kind)}
            </button>
          ))}
        </nav>
      )}
      <div className="filters">
        {!view && facets.map(({ d, opts }) => (
          <label key={d} className={f[d] ? "on" : undefined}>
            {t.dims[d]}
            <select value={f[d]} onChange={(e) => set({ [d]: e.target.value })}>
              <option value="">{t.all}</option>
              {f[d] && !opts.some(([v]) => v === f[d]) && <option value={f[d]}>{name(d, f[d])} (0)</option>}
              {opts.map(([v, n]) => <option key={v} value={v}>{name(d, v)} ({n})</option>)}
            </select>
          </label>
        ))}
        <label className={q ? "on" : undefined}>
          {t.search}
          <input type="search" value={q} onChange={(e) => set({ q: e.target.value })} placeholder={t.searchPlaceholder} />
        </label>
        <label>
          {t.sort}
          <select value={sort} onChange={(e) => set({ sort: e.target.value })}>
            {Object.entries(t.sorts).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
          </select>
        </label>
        <label className={plannedFor ? "on" : undefined}>
          {t.plannedDate}
          <input type="date" value={plannedFor} onChange={(e) => set({ date: e.target.value })} />
        </label>
      </div>
      {filtered && (
        <p className="active-filters" aria-label={t.filtering}>
          <span className="sub">{t.filtering}</span>
          {view && <button type="button" className="badge" onClick={() => set({ view: "" })} aria-label={t.removeFilter(label("itemKind", view.v.kind))}>{label("itemKind", view.v.kind)} ✕</button>}
          {active.map((d) => (
            <button key={d} type="button" className="badge" onClick={() => set({ [d]: "" })} aria-label={t.removeFilter(t.dims[d])}>{t.dims[d]}：{name(d, f[d])} ✕</button>
          ))}
          {q && <button type="button" className="badge" onClick={() => set({ q: "" })} aria-label={t.removeFilter(t.search)}>{t.search}：{q} ✕</button>}
          {plannedFor && <button type="button" className="badge" onClick={() => set({ date: "" })} aria-label={t.removeFilter(t.plannedDate)}>{t.plannedDate}：{plannedFor} ✕</button>}
          <button type="button" className="link" onClick={() => setParams(sort ? { sort } : {}, { replace: true })}>{t.clearAll}</button>
        </p>
      )}
      {error != null && <ErrorNote error={error} />}
      {view && items && <ViewTable view={view} items={all} />}
      {!view && items && !shown.length && error == null && <p className="empty">{t.noMatch}</p>}
      {!view && <ul aria-label={t.list} className="rows">
        {rows.map((i) => (
          <li key={i.id}>
            <Link to={i.id} state={{ list: location.search }}>{i.title}</Link> <span className="badge plain"><Val k="phase" id={i.phase} /></span>
            <span className="sub">
              {[i.project && project(i.project), i.status_label, i.tags?.map((x) => (x === HANDOFF_TAG ? t.handoffName : "#" + x)).join(" "), i.planned_for && t.planAt(i.planned_for, i.planned_slot), i.deadline && t.due(deadlineText(i.deadline, offset))].filter(Boolean).join(" · ")}
            </span>
          </li>
        ))}
      </ul>}
      {!view && shown.length > PAGE_SIZE && (
        <nav className="pager" aria-label={t.pager}>
          <button type="button" disabled={page <= 1} onClick={() => set({ page: String(page - 1) })}>{t.prev}</button>
          <span className="sub">{t.range((page - 1) * PAGE_SIZE + 1, (page - 1) * PAGE_SIZE + rows.length, shown.length)}</span>
          <button type="button" disabled={page >= pages} onClick={() => set({ page: String(page + 1) })}>{t.next}</button>
        </nav>
      )}
    </main>
  );
}

// 导入的条目只知道日期时 precision 为 day：只显示到日，不显示约定的 12:00 或 18:00（§7.1）。
const when = (at: string, precision?: string) => (precision === "day" ? new Date(at).toLocaleDateString(locale()) : new Date(at).toLocaleString(locale()));
const grouped = (r: Relation, from: string) => r.from === from && isMaterialId(r.to) && (MATERIAL_RELATIONS as readonly string[]).includes(r.type);

interface Detail { rejection?: Rejection; item: Item; relations: Relation[]; keys: { key: string; domain?: string }[]; others: Item[]; progress: { id: string; text: string; on?: string; actor: string; at: string; precision?: string }[]; materials: MaterialRel[] }

function ItemDetail() {
  const t = useT(strings);
  const label = useLabel();
  const { id = "" } = useParams();
  const { api } = useSession();
  const [d, setD] = useState<Detail | null>(null);
  const [loadErr, setLoadErr] = useState<unknown>(null);
  const [actErr, setActErr] = useState<unknown>(null);
  const projectName = useNames("project");
  const credential = useNames("credential");
  const [editing, setEditing] = useState(false);
  const post = useAnswer(setActErr);
  const { utc_offset: offset } = useHostClock(api);
  const back = ".." + ((useLocation().state as { list?: string } | null)?.list ?? ""); // 回到列表时带回原来的筛选与页码

  const load = useCallback(async () => {
    const eid = encodeURIComponent(id);
    const [it, list] = await Promise.all([
      api.get<{ item: Item; relations: Relation[]; correlation_keys?: { key: string; domain?: string }[]; progress?: Detail["progress"]; last_rejection?: Rejection | null }>("/items/" + eid),
      api.get<{ items: Item[] }>("/items"),
    ]);
    // 材料区的关系：依据、产出、纪要三类指向材料的边；材料读不到时按 id 显示，不丢这条关系。
    const materials = await Promise.all(
      it.relations.filter((r) => grouped(r, it.item.id)).map(async (r): Promise<MaterialRel> => {
        const material = await api.get<{ material: MaterialRow }>("/materials/" + encodeURIComponent(r.to)).then((m) => m.material, (): MaterialRow => ({ id: r.to, title: r.to }));
        return { type: r.type, to: r.to, material };
      }),
    );
    // 进展按发生时刻排：导入的历史条目不按写入顺序。
    const progress = [...(it.progress ?? [])].sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
    setD({ rejection: it.last_rejection ?? undefined, item: it.item, relations: it.relations, keys: it.correlation_keys ?? [], progress, materials, others: list.items.filter((i) => i.id !== id) });
  }, [api, id]);

  useEffect(() => {
    setD(null);
    setLoadErr(null);
    load().catch((e) => setLoadErr(e));
  }, [load]);

  // 写入后整页重读：显示的永远是服务端状态，冲突/拒绝时不留下本地假状态。
  const act = async (fn: () => Promise<unknown>) => {
    try {
      await fn();
      setActErr(null);
      setEditing(false);
      await load();
    } catch (e) {
      setActErr(e);
    }
  };

  if (loadErr != null) return <main><ErrorNote error={loadErr} /></main>;
  if (!d) return <main><p role="status">{t.loading}</p></main>;
  const { item } = d;
  const path = "/items/" + encodeURIComponent(id);
  const ifMatch = { "If-Match": String(item.version) };

  const answer = async (iid: string, body: { choice: string } | { text: string }) => {
    const ok = await post(iid, body);
    await load().catch(() => undefined);
    return ok;
  };
  const ended = ["done", "dropped"].includes(item.phase);

  // 自上而下（界面设计 §3.2）：①标题、状态一句与随阶段变化的主操作（等你决定时直接是问题卡）②Agent 处理 ③正文
  // ④进展与经过 ⑤「更多操作」折叠：状态标签、截止、转阶段、关联、材料、关联键。
  return (
    <main>
      <p className="eyebrow"><Link to={back}>{t.back}</Link></p>
      <h1>{item.title}</h1>
      <p className="lede">
        {[t.phaseNow(label("phase", item.phase)), item.project && projectName(item.project), item.planned_for && t.planAt(item.planned_for, item.planned_slot), item.deadline && t.due(deadlineText(item.deadline, offset))].filter(Boolean).join(" · ")}
      </p>
      {actErr != null && <ErrorNote error={actErr} />}
      {item.phase === "needs_input" && item.ask && !item.ask.resolution ? (
        <QuestionCard item={item as DecisionItem} rejection={d.rejection} onAnswer={answer} inItem />
      ) : (
        <RejectionNote rejection={d.rejection} what="edit" item={item.id} />
      )}
      {!ended && <LastRunNote item={item.id} />}
      {item.phase === "proposed" && (
        <div className="actions">
          <button type="button" className="primary" onClick={() => act(() => api.post(path + "/accept", {}))}>{t.accept}</button>
          <button type="button" onClick={() => act(() => api.post(path + "/dismiss", {}))}>{t.dismiss}</button>
        </div>
      )}
      {item.phase !== "proposed" && <PhaseActions key={"p" + item.version} item={item} onSubmit={(b) => act(() => api.post(path + "/transition", b, ifMatch))} />}
      {item.kind === "task" && (
        <AgentBlock key={"h" + item.version} item={item} onAgent={(aid) => act(() => api.patch(path, { preferred_agent_id: aid, reason: t.handoff }, ifMatch))} onSave={(tags) => act(() => api.patch(path, { tags, reason: t.handoffReason(tags.includes(HANDOFF_TAG)) }, ifMatch))} />
      )}

      {item.kind !== "brief" && <ScheduleBlock item={item} path={path} onSaved={load} />}

      {item.body ? <Markdown source={item.body} /> : null}
      {item.kind === "brief" && <BriefBody spec={item.spec} />}
      {editing ? (
        <ContentForm
          initial={{ title: item.title, body: item.body ?? "" }}
          submitLabel={t.save}
          onCancel={() => setEditing(false)}
          onSubmit={(v) => act(() => api.patch(path, { title: v.title, body: v.body }, ifMatch))}
        />
      ) : (
        <p><button type="button" onClick={() => setEditing(true)}>{t.edit}</button></p>
      )}

      <AttachedMessages itemId={id} />

      <section aria-label={t.progress}>
        <h2>{t.progress}</h2>
        <ul aria-label={t.progress} className="rows">
          {d.progress.map((p) => (
            <li key={p.id}>{p.text} <span className="sub">{[p.on && t.pointsTo(p.on), credential(p.actor), when(p.at, p.precision)].filter(Boolean).join(" · ")}</span></li>
          ))}
        </ul>
        <ProgressForm onSubmit={(b) => act(() => api.post(path + "/progress", b))} />
      </section>

      <section aria-label={t.changes}>
        <h2>{t.timeline}</h2>
        <TraceView path={path + "/trace"} rev={item.version} />
      </section>

      <details className="panel" aria-label={t.moreActions}>
        <summary>{t.moreActions}</summary>
        <div className="panel facts">
          <p>{t.label(item.status_label || t.notSet)}</p>
          {item.tags?.some((x) => x !== HANDOFF_TAG) ? <p>{t.tags(item.tags.filter((x) => x !== HANDOFF_TAG).join(t.listSep))}</p> : null}
        </div>
        {!ended && <DropAction key={"d" + item.version} onSubmit={(b) => act(() => api.post(path + "/transition", b, ifMatch))} />}
        {item.kind === "delegation" && <p><Link to={"/delegation#d-" + encodeURIComponent(item.id)}>{t.reassignLink}</Link></p>}
        <LabelForm key={"l" + item.version} item={item} onSave={(v) => act(() => api.patch(path, { status_label: v }, ifMatch))} />

        <section aria-label={t.relationsLabel}>
          <h2>{t.relations}</h2>
          <ul aria-label={t.relations} className="rows">
            {d.relations.filter((r) => !grouped(r, item.id)).map((r, n) => {
              const out = r.from === item.id;
              const other = out ? r.to : r.from;
              const type = label("relation", r.type);
              const to = isMaterialId(other) ? "/materials?id=" + encodeURIComponent(other) : "../" + encodeURIComponent(other);
              return <li key={n}>{out ? `${type} →` : `← ${type}`} <Link to={to}>{isMaterialId(other) ? other : <ItemTitle id={other} />}</Link></li>;
            })}
          </ul>
          <RelateForm others={d.others} onSubmit={(b) => act(() => api.post(path + "/relations", b))} />
        </section>

        <ItemMaterials rels={d.materials} onRemove={(r) => act(() => api.post(path + "/relations", { to: r.to, type: r.type, remove: true }))} />

        {/* 关联键是来源线程或对象的标识（§9.4），平时用不到，收在高级里。 */}
        <details aria-label={t.sourcesLabel}>
          <summary>{t.advancedKeys}</summary>
          <ul aria-label={t.keys} className="rows">
            {d.keys.map((k, n) => <li key={n}>{k.key}</li>)}
          </ul>
          <KeyForm key={"k" + item.version} onSubmit={(key) => act(() => api.post(path + "/correlation-keys", { key }, ifMatch))} />
        </details>
      </details>
    </main>
  );
}

function LabelForm({ item, onSave }: { item: Item; onSave: (v: string) => void }) {
  const t = useT(strings);
  const [v, setV] = useState(item.status_label ?? "");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); onSave(v); }}>
      <label>{t.statusLabel} <input value={v} onChange={(e) => setV(e.target.value)} /></label>
      <button type="submit">{t.saveLabel}</button>
    </form>
  );
}

// 主操作区的阶段变更（界面设计 §9.5）：按当前阶段列出能去的阶段，一键改；理由写明是在网页上改的。
// 要补信息的就地展开再确认：等待要说等什么，委派完成要带证据（内核要求，§6.14）。「不做了」在「更多操作」里二次确认。
function PhaseActions({ item, onSubmit }: { item: Item; onSubmit: (b: Record<string, unknown>) => void }) {
  const t = useT(strings);
  const label = useLabel();
  const [to, setTo] = useState<string | null>(null);
  const [evidence, setEvidence] = useState("");
  const [wKind, setWKind] = useState("human");
  const [wRef, setWRef] = useState("");
  const ended = ["done", "dropped"].includes(item.phase);
  const targets = ended ? ["open"] : ["done", "open", "waiting"].filter((p) => p !== item.phase);
  const text = (p: string) => (p === "open" ? (ended ? t.reopen : t.backToOpen) : p === "waiting" ? t.toWaiting : t.done);
  const reason = (p: string) => (p === "done" ? t.doneReason : t.phaseReason(label("phase", p)));
  const needs = (p: string) => p === "waiting" || (p === "done" && item.kind === "delegation");
  // 等待对象随类别换控件：时间用日期时间（发 RFC 3339），执行者用名单，其余写一句话。
  const pickKind = (k: string) => { setWKind(k); setWRef(""); };
  const ref = wKind === "time" && wRef ? new Date(wRef).toISOString() : wRef.trim();
  const submit = (p: string) => onSubmit({ to: p, reason: reason(p), ...(p === "done" && evidence.trim() ? { evidence: evidence.trim() } : {}), ...(p === "waiting" ? { waiting_on: { kind: wKind, ref } } : {}) });
  return (
    <div className="stack" aria-label={t.phaseActions} role="group">
      <div className="actions">
        {targets.map((p, n) => (
          <button key={p} type="button" className={n === 0 ? "primary" : undefined} aria-expanded={needs(p) ? to === p : undefined} onClick={() => (needs(p) ? setTo(to === p ? null : p) : submit(p))}>{text(p)}</button>
        ))}
      </div>
      {to && (
        <form className="panel" onSubmit={(e) => { e.preventDefault(); submit(to); }}>
          {to === "done" && <label>{t.evidence} <input value={evidence} onChange={(e) => setEvidence(e.target.value)} required /></label>}
          {to === "waiting" && (
            <>
              <label>{t.waitKind} <Pick k="waiting" value={wKind} onChange={pickKind} ids={WAITING} /></label>
              {wKind === "time" ? (
                <label>{t.waitUntil} <input type="datetime-local" value={wRef} onChange={(e) => setWRef(e.target.value)} required /></label>
              ) : wKind === "executor" ? (
                <label>{t.waitRef} <EntityPick kind="executor" value={wRef} onChange={setWRef} empty={t.choose} /></label>
              ) : (
                <label>{t.waitRef} <input value={wRef} onChange={(e) => setWRef(e.target.value)} required /></label>
              )}
            </>
          )}
          <div className="actions">
            <button type="submit" className="primary">{t.confirmPhase(text(to))}</button>
            <button type="button" onClick={() => setTo(null)}>{t.cancel}</button>
          </div>
        </form>
      )}
    </div>
  );
}

/** 不做了（放弃）：低频且会让事项离开所有待办，所以收在「更多操作」里，先确认再改。 */
function DropAction({ onSubmit }: { onSubmit: (b: Record<string, unknown>) => void }) {
  const t = useT(strings);
  const [asking, setAsking] = useState(false);
  const [why, setWhy] = useState("");
  if (!asking) return <p><button type="button" className="danger" onClick={() => setAsking(true)}>{t.drop}</button></p>;
  return (
    <div className="panel actions" role="group" aria-label={t.drop}>
      <label>{t.dropWhy} <input value={why} onChange={(e) => setWhy(e.target.value)} /></label>
      <button type="button" className="danger" onClick={() => onSubmit({ to: "dropped", reason: why.trim() || t.dropReason })}>{t.confirmDrop}</button>
      <button type="button" onClick={() => setAsking(false)}>{t.cancel}</button>
    </div>
  );
}

function RelateForm({ others, onSubmit }: { others: Item[]; onSubmit: (b: { to: string; type: string }) => void }) {
  const t = useT(strings);
  const [to, setTo] = useState("");
  const [type, setType] = useState("relates");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); if (to) onSubmit({ to, type }); }}>
      <label>{t.relateTo} <select value={to} onChange={(e) => setTo(e.target.value)}><option value="">{t.choose}</option>{others.map((o) => <option key={o.id} value={o.id}>{o.title}</option>)}</select></label>
      <label>{t.relType} <Pick k="relation" value={type} onChange={setType} ids={REL_TYPES} /></label>
      <button type="submit" disabled={!to}>{t.relate}</button>
    </form>
  );
}

interface Content { title: string; body: string }

function ContentForm({ initial, submitLabel, onSubmit, onCancel, extra }: {
  initial: Content;
  submitLabel: string;
  onSubmit: (v: Content) => void;
  onCancel?: () => void;
  extra?: ReactNode;
}) {
  const t = useT(strings);
  const [v, setV] = useState(initial);
  const [err, setErr] = useState<string | null>(null);
  return (
    <form
      className="panel stack"
      onSubmit={(e) => {
        e.preventDefault();
        if (!v.title.trim()) return setErr(t.emptyTitle);
        setErr(null);
        onSubmit({ title: v.title.trim(), body: v.body });
      }}
    >
      <label>{t.titleField} <input value={v.title} onChange={(e) => setV({ ...v, title: e.target.value })} /></label>
      {extra}
      <label>{t.body} <textarea placeholder={t.markdown} rows={8} value={v.body} onChange={(e) => setV({ ...v, body: e.target.value })} /></label>
      {err && <p role="alert">{err}</p>}
      <div className="actions">
        <button type="submit">{submitLabel}</button>
        {onCancel && <button type="button" onClick={onCancel}>{t.cancel}</button>}
      </div>
    </form>
  );
}

// 新建直接进 open（本人创建，不走建议流）；校验与授权在服务端，这里只拦空标题。
// /new 与 /items/new 的查询参数只作初值（§4.1）：不提交、不带凭据，点"创建"才写入。
export function NewItemPage() {
  return <NewItem />;
}

function NewItem() {
  const t = useT(strings);
  const { api } = useSession();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const pre = useMemo(() => readPrefill(params), [params]);
  const [projects, setProjects] = useState<{ id: string; name?: string }[]>([]);
  const [vaults, setVaults] = useState<Vault[]>([]);
  // null：本人还没动过项目选择，显示预填认出的那个。
  const [picked, setPicked] = useState<string | null>(null);
  const [deadline, setDeadline] = useState(pre.deadline);
  const [plannedFor, setPlannedFor] = useState(pre.plannedFor);
  const preSlot = parseSlot(pre.plannedSlot);
  const [timed, setTimed] = useState(!!preSlot);
  const [slotStart, setSlotStart] = useState(preSlot ? hhmm(preSlot[0]) : "09:00");
  const [slotEnd, setSlotEnd] = useState(preSlot ? hhmm(preSlot[1] % 1440) : "10:00");
  const [err, setErr] = useState<unknown>(null);
  const [created, setCreated] = useState<string | null>(null);
  const { utc_offset: offset } = useHostClock(api);
  const from = (useLocation().state as { from?: string } | null)?.from;
  // 每次打开新建页一个幂等键：连点两次、网络重试都只建出一件（§3.2）；被服务端拒绝（4xx）后换新键，改过的输入不算重放。
  const idem = useRef(crypto.randomUUID());
  const slotPair = parseSlot(`${slotStart} - ${slotEnd === "00:00" ? "24:00" : slotEnd}`);
  const badSlot = !!plannedFor && timed && !slotPair;
  useEffect(() => void api.get<{ projects: { id: string; name?: string }[] }>("/projects").then((r) => setProjects(r.projects), () => {}), [api]);
  useEffect(() => void api.get<{ vaults: Vault[] }>("/note-vaults").then((r) => setVaults(r.vaults ?? []), () => {}), [api]);

  // project 可以是 id 或名字，认不出就留空。
  const known = projects.find((p) => p.id === pre.project) ?? projects.find((p) => p.name === pre.project);
  const project = picked ?? known?.id ?? "";

  const create = async (c: Content) => {
    if (badSlot) return;
    setErr(null);
    setCreated(null);
    const asMaterial = !!pre.link && !!project && registrable(pre.link, vaults);
    let id: string;
    try {
      const r = await api.post<{ id: string }>("/items", {
        kind: "task",
        phase: "open",
        title: c.title,
        ...(asMaterial ? (c.body ? { body: c.body } : {}) : withLink(c.body, pre.link) ? { body: withLink(c.body, pre.link) } : {}),
        ...(project ? { project } : {}),
        ...(deadline ? { deadline: deadlineValue(deadline.slice(0, 10), deadline.slice(11) === "23:59" ? null : deadline.slice(11), offset) } : {}),
        ...(plannedFor ? { planned_for: plannedFor } : {}),
        ...(plannedFor && timed && slotPair ? { planned_slot: slotOf(slotPair[0], slotPair[1]) } : {}),
      }, { "Idempotency-Key": idem.current });
      id = r.id;
    } catch (e) {
      if (e instanceof ApiError && e.status >= 400 && e.status < 500) idem.current = crypto.randomUUID();
      return setErr(e);
    }
    if (asMaterial) {
      try {
        const m = await api.post<{ material: string }>("/materials", { link: pre.link, title: linkTitle(pre.link), project });
        await api.post("/items/" + encodeURIComponent(id) + "/relations", { to: m.material, type: "basis" });
      } catch (e) {
        // 事项已经写入：不重复创建，说明链接没登记上并给出新事项的入口。
        setErr(e);
        return setCreated(id);
      }
    }
    // 新事项页顶部的"返回刚才的页面"回到打开新建页之前的地方（如日历）。
    nav("/items/" + encodeURIComponent(id), from ? { state: { from } } : undefined);
  };

  return (
    <main>
      <p className="eyebrow"><Link to="/items">{t.back}</Link></p>
      <PageHead>{t.newItem}</PageHead>
      {pre.any && <p className="badge plain" role="status">{t.prefilled}</p>}
      {pre.cutTitle && <p role="status">{t.cutTitle}</p>}
      {pre.cutBody && <p role="status">{t.cutBody}</p>}
      {err != null && <ErrorNote error={err} />}
      {created && <p role="alert">{t.linkNotSaved} · <Link to={"/items/" + encodeURIComponent(created)}>{t.openCreated}</Link></p>}
      <ContentForm
        initial={{ title: pre.title, body: pre.body }}
        submitLabel={t.createSubmit}
        onSubmit={create}
        extra={
          <>
            {pre.link && <p>{t.linkField}: <code>{pre.link}</code></p>}
            <label>{t.projectField} <select value={project} onChange={(e) => setPicked(e.target.value)}><option value="">{t.noProject}</option>{projects.map((p) => <option key={p.id} value={p.id}>{p.name ?? p.id}</option>)}</select></label>
            <label>{t.deadlineField} <input type="datetime-local" value={deadline} onChange={(e) => setDeadline(e.target.value)} /></label>
            <label>{t.plannedDate} <input type="date" value={plannedFor} onChange={(e) => setPlannedFor(e.target.value)} /></label>
            <fieldset disabled={!plannedFor}>
              <legend>{t.slot}</legend>
              <label><input type="checkbox" checked={!timed} onChange={(e) => setTimed(!e.target.checked)} /> {t.untimed}</label>
              {timed && (
                <div className="actions">
                  <label>{t.slotStart} <input type="time" step={60} value={slotStart} onChange={(e) => setSlotStart(e.target.value)} /></label>
                  <label>{t.slotEnd} <input type="time" step={60} value={slotEnd} onChange={(e) => setSlotEnd(e.target.value)} /></label>
                </div>
              )}
              {badSlot && <p role="alert">{t.badSlot}</p>}
            </fieldset>
          </>
        }
      />
    </main>
  );
}

function ItemTitle({ id }: { id: string }) {
  return <>{useItemTitle(id)}</>;
}

function KeyForm({ onSubmit }: { onSubmit: (key: string) => void }) {
  const t = useT(strings);
  const [key, setKey] = useState("");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); if (key.trim()) onSubmit(key.trim()); }}>
      <label>{t.sourceThreads} <input value={key} onChange={(e) => setKey(e.target.value)} /></label>
      <button type="submit" disabled={!key.trim()}>{t.registerKey}</button>
    </form>
  );
}
