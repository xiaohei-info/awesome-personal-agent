import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, Route, Routes, useNavigate, useParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { isMaterialId } from "../../app/relations";
import { AttachedMessages } from "../attached-messages";
import { Markdown } from "./Markdown";
import { ProgressForm } from "../today/ProgressForm";
import { locale, useT } from "../../app/i18n";
import { strings } from "./strings";
import { PHASE, WAITING } from "../../app/ids";
import { Pick, Val, useLabel } from "../../app/values";
import { ErrorNote } from "../../app/errors";
import { EntityPick, useItemTitle, useNames } from "../../app/names";

// 形状对应 GET /v1/items、/v1/items/{id}、/v1/why/{id}（附录 B.2）。校验、授权、版本冲突、阶段进入约束都在服务端，
// 本页只搬运输入并显示服务端的错误原因。
export interface Item {
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
  waiting_on?: { kind: string; ref: string };
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
      <div className="filters">
      <label>
        {t.phaseFilter}
        <Pick k="phase" value={phase} onChange={setPhase} ids={PHASE} empty={t.all} />
      </label>
      <label>
        {t.tagFilter}
        <select value={tag} onChange={(e) => setTag(e.target.value)}>
          <option value="">{t.all}</option>
          {tags.map((t) => <option key={t}>{t}</option>)}
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
      {items && !shown.length && error == null && <p className="empty">{t.noMatch}</p>}
      <ul aria-label={t.list} className="rows">
        {shown.map((i) => (
          <li key={i.id}>
            <Link to={i.id}>{i.title}</Link> <span className="badge plain"><Val k="phase" id={i.phase} /></span>
            <span className="sub">
              {[i.status_label, i.tags?.map((t) => "#" + t).join(" "), i.deadline && t.due(new Date(i.deadline).toLocaleString(locale()))].filter(Boolean).join(" · ")}
            </span>
          </li>
        ))}
      </ul>
    </main>
  );
}

interface Detail { item: Item; relations: Relation[]; keys: { key: string; domain?: string }[]; steps: Step[]; others: Item[]; progress: { id: string; text: string; on?: string; actor: string; at: string }[] }

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

  const load = useCallback(async () => {
    const eid = encodeURIComponent(id);
    const [it, why, list] = await Promise.all([
      api.get<{ item: Item; relations: Relation[]; correlation_keys?: { key: string; domain?: string }[]; progress?: Detail["progress"] }>("/items/" + eid),
      api.get<{ steps: Step[] }>("/why/" + eid),
      api.get<{ items: Item[] }>("/items"),
    ]);
    setD({ item: it.item, relations: it.relations, keys: it.correlation_keys ?? [], steps: why.steps, progress: it.progress ?? [], others: list.items.filter((i) => i.id !== id) });
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

  return (
    <main>
      <p className="eyebrow"><Link to="..">{t.back}</Link></p>
      <h1>{item.title}</h1>
      <div className="panel facts">
        <p>{t.phase(label("phase", item.phase))}</p>
        <p>{t.label(item.status_label || t.notSet)}</p>
        <p>{t.deadline(item.deadline ? new Date(item.deadline).toLocaleString(locale()) : t.notSet)}</p>
        {item.project ? <p>{t.project(projectName(item.project))}</p> : null}
        {item.planned_for ? <p>{t.planned(item.planned_for)}</p> : null}
        {item.tags?.length ? <p>{t.tags(item.tags.join(t.listSep))}</p> : null}
      </div>
      {item.body ? <Markdown source={item.body} /> : null}
      {actErr != null && <ErrorNote error={actErr} />}

      <h2>{t.edit}</h2>
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
      <LabelForm key={"l" + item.version} item={item} onSave={(v) => act(() => api.patch(path, { status_label: v }, ifMatch))} />
      <DeadlineForm key={"d" + item.version} item={item} onSave={(v) => act(() => api.patch(path, { deadline: v }, ifMatch))} />
      <TransitionForm key={"t" + item.version} onSubmit={(b) => act(() => api.post(path + "/transition", b, ifMatch))} />

      <section aria-label={t.relationsLabel}>
        <h2>{t.relations}</h2>
        <ul aria-label={t.relations} className="rows">
          {d.relations.map((r, n) => {
            const out = r.from === item.id;
            const other = out ? r.to : r.from;
            const type = t.relation[r.type] ?? r.type;
            const to = isMaterialId(other) ? "/materials?id=" + encodeURIComponent(other) : "../" + encodeURIComponent(other);
            return <li key={n}>{out ? `${type} →` : `← ${type}`} <Link to={to}>{isMaterialId(other) ? other : <ItemTitle id={other} />}</Link></li>;
          })}
        </ul>
        <RelateForm others={d.others} onSubmit={(b) => act(() => api.post(path + "/relations", b))} />
      </section>

      {/* 关联键是来源线程或对象的标识（§9.4），平时用不到，收在高级里。 */}
      <details aria-label={t.sourcesLabel}>
        <summary>{t.advancedKeys}</summary>
        <ul aria-label={t.keys} className="rows">
          {d.keys.map((k, n) => <li key={n}>{k.key}</li>)}
        </ul>
        <KeyForm key={"k" + item.version} onSubmit={(key) => act(() => api.post(path + "/correlation-keys", { key }, ifMatch))} />
      </details>

      <AttachedMessages itemId={id} />

      <section aria-label={t.progress}>
        <h2>{t.progress}</h2>
        <ul aria-label={t.progress} className="rows">
          {d.progress.map((p) => (
            <li key={p.id}>{p.text} <span className="sub">{[p.on && t.pointsTo(p.on), credential(p.actor), new Date(p.at).toLocaleString(locale())].filter(Boolean).join(" · ")}</span></li>
          ))}
        </ul>
        <ProgressForm onSubmit={(b) => act(() => api.post(path + "/progress", b))} />
      </section>

      <section aria-label={t.changes}>
        <h2>{t.timeline}</h2>
        <ol aria-label={t.timeline} className="rows">
          {d.steps.map((s) => (
            <li key={s.seq}>
              <ActorBadge actor={s.actor} /> <strong><Val k="event" id={s.type} /></strong> <time dateTime={s.at}>{new Date(s.at).toLocaleString(locale())}</time>
              {s.reason ? ` — ${s.reason}` : ""}
            </li>
          ))}
        </ol>
      </section>
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
function NewItem() {
  const t = useT(strings);
  const { api } = useSession();
  const nav = useNavigate();
  const [projects, setProjects] = useState<{ id: string; name?: string }[]>([]);
  const [project, setProject] = useState("");
  const [deadline, setDeadline] = useState("");
  const [plannedFor, setPlannedFor] = useState("");
  const [err, setErr] = useState<unknown>(null);
  useEffect(() => void api.get<{ projects: { id: string; name?: string }[] }>("/projects").then((r) => setProjects(r.projects), () => {}), [api]);

  const create = async (c: Content) => {
    try {
      const r = await api.post<{ id: string }>("/items", {
        kind: "task",
        phase: "open",
        title: c.title,
        ...(c.body ? { body: c.body } : {}),
        ...(project ? { project } : {}),
        ...(deadline ? { deadline: new Date(deadline).toISOString() } : {}),
        ...(plannedFor ? { planned_for: plannedFor } : {}),
      });
      nav("../" + encodeURIComponent(r.id));
    } catch (e) {
      setErr(e);
    }
  };

  return (
    <main>
      <p className="eyebrow"><Link to="..">{t.back}</Link></p>
      <PageHead>{t.newItem}</PageHead>
      {err != null && <ErrorNote error={err} />}
      <ContentForm
        initial={{ title: "", body: "" }}
        submitLabel={t.createSubmit}
        onSubmit={create}
        extra={
          <>
            <label>{t.projectField} <select value={project} onChange={(e) => setProject(e.target.value)}><option value="">{t.noProject}</option>{projects.map((p) => <option key={p.id} value={p.id}>{p.name ?? p.id}</option>)}</select></label>
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
