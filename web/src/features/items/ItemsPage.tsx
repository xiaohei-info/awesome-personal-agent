import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, Route, Routes, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { isMaterialId } from "../../app/relations";
import { AttachedMessages } from "../attached-messages";
import { Markdown } from "./Markdown";
import { BriefBody } from "../briefs/BriefBody";
import { ProgressForm } from "../today/ProgressForm";
import { locale, useT } from "../../app/i18n";
import { strings } from "./strings";
import { HANDOFF_TAG, PHASE, WAITING } from "../../app/ids";
import { Pick, Val, useLabel } from "../../app/values";
import { ErrorNote } from "../../app/errors";
import { EntityPick, useItemTitle, useNames } from "../../app/names";
import { ViewTable, useViews } from "./TypeViews";
import { AgentBlock, tagName } from "./HandOff";
import { QuestionCard, useAnswer, type DecisionItem } from "../decisions/DecisionsPage";
import { TraceView } from "../trace/Trace";
import { ItemMaterials, MATERIAL_RELATIONS, type MaterialRel, type MaterialRow } from "./ItemMaterials";
import { linkTitle, readPrefill, registrable, withLink, type Vault } from "./prefill";

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
  body?: string;
  spec?: { sections?: { project?: string; title?: string; body: string }[] };
  waiting_on?: { kind: string; ref: string };
  ask?: DecisionItem["ask"];
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
  // agent 显示凭据的标签；本人与系统不再重复显示身份串。
  const who = actor.type === "agent" ? credential(actor.principal) : "";
  return (
    <span aria-label={t.actorLabel} data-actor={actor.type} className="badge plain">
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

function ItemList() {
  const t = useT(strings);
  const { api } = useSession();
  const [phase, setPhase] = useState("");
  const [tag, setTag] = useState("");
  const [q, setQ] = useState("");
  const [sort, setSort] = useState("");
  const [plannedFor, setPlannedFor] = useState("");
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const views = useViews(api);
  const label = useLabel();
  const [params, setParams] = useSearchParams();
  const view = views.find((x) => x.key === params.get("view"));
  const setView = (key: string) => setParams(key ? { view: key } : {}, { replace: true });

  useEffect(() => {
    setError(null);
    // 全文、排序与计划日期都在服务端做，Web 与 CLI、HTTP 的结果因此一致。
    const qs = new URLSearchParams();
    for (const [k, v] of [["phase", phase], ["q", q.trim()], ["sort", sort], ["planned_for", plannedFor]]) if (v) qs.set(k, v);
    api.get<{ items: Item[] }>("/items" + (qs.size ? "?" + qs : "")).then(
      (r) => setItems(r.items),
      (e) => setError(e),
    );
  }, [api, phase, q, sort, plannedFor]);

  const tags = [...new Set((items ?? []).flatMap((i) => i.tags ?? []))].sort();
  const shown = (items ?? []).filter((i) => !tag || i.tags?.includes(tag));

  return (
    <main>
      <PageHead>{t.title}</PageHead>
      <p><Link to="new"><button type="button">{t.create}</button></Link></p>
      {views.length > 0 && (
        <nav className="chips" aria-label={t.typeFilter}>
          <button type="button" className={view ? undefined : "on"} aria-pressed={!view} onClick={() => setView("")}>{t.allTypes}</button>
          {views.map((x) => (
            <button key={x.key} type="button" className={x === view ? "on" : undefined} aria-pressed={x === view} onClick={() => setView(x.key)}>
              {label("itemKind", x.v.kind)}
            </button>
          ))}
        </nav>
      )}
      <div className="filters">
      <label>
        {t.phaseFilter}
        <Pick k="phase" value={phase} onChange={setPhase} ids={PHASE} empty={t.all} />
      </label>
      <label>
        {t.tagFilter}
        <select value={tag} onChange={(e) => setTag(e.target.value)}>
          <option value="">{t.all}</option>
          {tags.map((x) => <option key={x} value={x}>{tagName(x, t.handoffName)}</option>)}
        </select>
      </label>
      <label>
        {t.search}
        <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t.searchPlaceholder} />
      </label>
      <label>
        {t.sort}
        <select value={sort} onChange={(e) => setSort(e.target.value)}>
          {Object.entries(t.sorts).map(([k, v]) => (
            <option key={k} value={k}>
              {v}
            </option>
          ))}
        </select>
      </label>
      <label>
        {t.plannedDate}
        <input type="date" value={plannedFor} onChange={(e) => setPlannedFor(e.target.value)} />
      </label>
      </div>
      {error != null && <ErrorNote error={error} />}
      {view && items && <ViewTable view={view} items={shown} />}
      {!view && items && !shown.length && error == null && <p className="empty">{t.noMatch}</p>}
      {!view && <ul aria-label={t.list} className="rows">
        {shown.map((i) => (
          <li key={i.id}>
            <Link to={i.id}>{i.title}</Link> <span className="badge plain"><Val k="phase" id={i.phase} /></span>
            <span className="sub">
              {[i.status_label, i.tags?.map((x) => (x === HANDOFF_TAG ? t.handoffName : "#" + x)).join(" "), i.deadline && t.due(new Date(i.deadline).toLocaleString(locale()))].filter(Boolean).join(" · ")}
            </span>
          </li>
        ))}
      </ul>}
    </main>
  );
}

// 导入的条目只知道日期时 precision 为 day：只显示到日，不显示约定的 12:00 或 18:00（§7.1）。
const when = (at: string, precision?: string) => (precision === "day" ? new Date(at).toLocaleDateString(locale()) : new Date(at).toLocaleString(locale()));
const grouped = (r: Relation, from: string) => r.from === from && isMaterialId(r.to) && (MATERIAL_RELATIONS as readonly string[]).includes(r.type);

interface Detail { item: Item; relations: Relation[]; keys: { key: string; domain?: string }[]; others: Item[]; progress: { id: string; text: string; on?: string; actor: string; at: string; precision?: string }[]; materials: MaterialRel[] }

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

  const load = useCallback(async () => {
    const eid = encodeURIComponent(id);
    const [it, list] = await Promise.all([
      api.get<{ item: Item; relations: Relation[]; correlation_keys?: { key: string; domain?: string }[]; progress?: Detail["progress"] }>("/items/" + eid),
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
    setD({ item: it.item, relations: it.relations, keys: it.correlation_keys ?? [], progress, materials, others: list.items.filter((i) => i.id !== id) });
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
      <p className="eyebrow"><Link to="..">{t.back}</Link></p>
      <h1>{item.title}</h1>
      <p className="lede">
        {[t.phaseNow(label("phase", item.phase)), item.project && projectName(item.project), item.deadline && t.due(new Date(item.deadline).toLocaleString(locale())), item.planned_for && t.planned(item.planned_for)].filter(Boolean).join(" · ")}
      </p>
      {actErr != null && <ErrorNote error={actErr} />}
      {item.phase === "needs_input" && item.ask && !item.ask.resolution && (
        <QuestionCard item={item as DecisionItem} onAnswer={answer} inItem />
      )}
      {item.phase === "proposed" && (
        <div className="actions">
          <button type="button" className="primary" onClick={() => act(() => api.post(path + "/accept", {}))}>{t.accept}</button>
          <button type="button" onClick={() => act(() => api.post(path + "/dismiss", {}))}>{t.dismiss}</button>
        </div>
      )}
      {item.phase === "open" && (
        <div className="actions">
          <button type="button" className="primary" onClick={() => act(() => api.post(path + "/transition", { to: "done", reason: t.doneReason }, ifMatch))}>{t.done}</button>
        </div>
      )}
      {item.kind === "task" && (
        <AgentBlock key={"h" + item.version} item={item} onAgent={(aid) => act(() => api.patch(path, { preferred_agent_id: aid, reason: t.handoff }, ifMatch))} onSave={(tags) => act(() => api.patch(path, { tags, reason: t.handoffReason(tags.includes(HANDOFF_TAG)) }, ifMatch))} />
      )}

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
          <p>{t.deadline(item.deadline ? new Date(item.deadline).toLocaleString(locale()) : t.notSet)}</p>
          {item.tags?.some((x) => x !== HANDOFF_TAG) ? <p>{t.tags(item.tags.filter((x) => x !== HANDOFF_TAG).join(t.listSep))}</p> : null}
        </div>
        <LabelForm key={"l" + item.version} item={item} onSave={(v) => act(() => api.patch(path, { status_label: v }, ifMatch))} />
        <DeadlineForm key={"d" + item.version} item={item} onSave={(v) => act(() => api.patch(path, { deadline: v }, ifMatch))} />
        <TransitionForm key={"t" + item.version} onSubmit={(b) => act(() => api.post(path + "/transition", b, ifMatch))} />

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

// datetime-local 是本地时间；发给服务端的是 RFC 3339（UTC）。
function DeadlineForm({ item, onSave }: { item: Item; onSave: (v: string | null) => void }) {
  const t = useT(strings);
  const [v, setV] = useState("");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); if (v) onSave(new Date(v).toISOString()); }}>
      <label>{t.deadlineField} <input type="datetime-local" value={v} onChange={(e) => setV(e.target.value)} /></label>
      <div className="actions">
        <button type="submit" disabled={!v}>{t.setDeadline}</button>
        <button type="button" disabled={!item.deadline} onClick={() => onSave(null)}>{t.clearDeadline}</button>
      </div>
    </form>
  );
}

function TransitionForm({ onSubmit }: { onSubmit: (b: Record<string, unknown>) => void }) {
  const t = useT(strings);
  const [to, setTo] = useState("open");
  const [reason, setReason] = useState("");
  const [evidence, setEvidence] = useState("");
  const [wKind, setWKind] = useState("human");
  const [wRef, setWRef] = useState("");
  // 等待对象随类别换控件：时间用日期时间（发 RFC 3339），执行者用名单，其余写一句话。
  const pickKind = (k: string) => { setWKind(k); setWRef(""); };
  const ref = wKind === "time" && wRef ? new Date(wRef).toISOString() : wRef.trim();
  return (
    <form
      className="panel"
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit({ to, reason, ...(evidence ? { evidence } : {}), ...(to === "waiting" && ref ? { waiting_on: { kind: wKind, ref } } : {}) });
      }}
    >
      <label>{t.targetPhase} <Pick k="phase" value={to} onChange={setTo} ids={PHASE} /></label>
      <label>{t.reason} <input value={reason} onChange={(e) => setReason(e.target.value)} /></label>
      <label>{t.evidence} <input value={evidence} onChange={(e) => setEvidence(e.target.value)} /></label>
      {to === "waiting" && (
        <>
          <label>{t.waitKind} <Pick k="waiting" value={wKind} onChange={pickKind} ids={WAITING} /></label>
          {wKind === "time" ? (
            <label>{t.waitUntil} <input type="datetime-local" value={wRef} onChange={(e) => setWRef(e.target.value)} /></label>
          ) : wKind === "executor" ? (
            <label>{t.waitRef} <EntityPick kind="executor" value={wRef} onChange={setWRef} empty={t.choose} /></label>
          ) : (
            <label>{t.waitRef} <input value={wRef} onChange={(e) => setWRef(e.target.value)} /></label>
          )}
        </>
      )}
      <button type="submit">{t.transition}</button>
    </form>
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
  const [err, setErr] = useState<unknown>(null);
  const [created, setCreated] = useState<string | null>(null);
  useEffect(() => void api.get<{ projects: { id: string; name?: string }[] }>("/projects").then((r) => setProjects(r.projects), () => {}), [api]);
  useEffect(() => void api.get<{ vaults: Vault[] }>("/note-vaults").then((r) => setVaults(r.vaults ?? []), () => {}), [api]);

  // project 可以是 id 或名字，认不出就留空。
  const known = projects.find((p) => p.id === pre.project) ?? projects.find((p) => p.name === pre.project);
  const project = picked ?? known?.id ?? "";

  const create = async (c: Content) => {
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
        ...(deadline ? { deadline: new Date(deadline).toISOString() } : {}),
        ...(plannedFor ? { planned_for: plannedFor } : {}),
      });
      id = r.id;
    } catch (e) {
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
    nav("/items/" + encodeURIComponent(id));
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
