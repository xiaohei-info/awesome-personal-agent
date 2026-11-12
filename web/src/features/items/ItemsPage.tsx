import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, Route, Routes, useNavigate, useParams } from "react-router-dom";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { MATERIAL_RELATION_LABELS, isMaterialId } from "../../app/relations";
import { AttachedMessages } from "../attached-messages";
import { Markdown } from "./Markdown";

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

const PHASES = ["proposed", "open", "waiting", "needs_input", "done", "dropped"];
const REL_TYPES = ["relates", "parent", "blocks", "derived_from", "supersedes"];
const ACTOR = { human: "本人", agent: "agent", system: "系统" } as Record<string, string>;

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

/** 每个变更的操作者徽章：谁、从哪个入口。 */
export function ActorBadge({ actor }: { actor: Step["actor"] }) {
  return (
    <span aria-label="操作者" data-actor={actor.type} className="badge plain">
      {[ACTOR[actor.type] ?? actor.type, actor.via, actor.principal].join(" · ")}
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
  const { api } = useSession();
  const [phase, setPhase] = useState("");
  const [tag, setTag] = useState("");
  const [q, setQ] = useState("");
  const [sort, setSort] = useState("");
  const [plannedFor, setPlannedFor] = useState("");
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setError(null);
    // 全文、排序与计划日期都在服务端做，Web 与 CLI、HTTP 的结果因此一致。
    const qs = new URLSearchParams();
    for (const [k, v] of [["phase", phase], ["q", q.trim()], ["sort", sort], ["planned_for", plannedFor]]) if (v) qs.set(k, v);
    api.get<{ items: Item[] }>("/items" + (qs.size ? "?" + qs : "")).then(
      (r) => setItems(r.items),
      (e) => setError(msg(e)),
    );
  }, [api, phase, q, sort, plannedFor]);

  const tags = [...new Set((items ?? []).flatMap((i) => i.tags ?? []))].sort();
  const shown = (items ?? []).filter((i) => !tag || i.tags?.includes(tag));

  return (
    <main>
      <PageHead>事项</PageHead>
      <p><Link to="new"><button type="button">新建</button></Link></p>
      <div className="filters">
      <label>
        阶段筛选
        <select value={phase} onChange={(e) => setPhase(e.target.value)}>
          <option value="">全部</option>
          {PHASES.map((p) => <option key={p}>{p}</option>)}
        </select>
      </label>
      <label>
        标签筛选
        <select value={tag} onChange={(e) => setTag(e.target.value)}>
          <option value="">全部</option>
          {tags.map((t) => <option key={t}>{t}</option>)}
        </select>
      </label>
      <label>
        搜索
        <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder="标题、摘要、正文、进展" />
      </label>
      <label>
        排序
        <select value={sort} onChange={(e) => setSort(e.target.value)}>
          <option value="">默认</option>
          <option value="updated">更新时间</option>
          <option value="deadline">截止</option>
          <option value="planned_for">计划日期</option>
          <option value="priority">优先级</option>
        </select>
      </label>
      <label>
        计划日期
        <input type="date" value={plannedFor} onChange={(e) => setPlannedFor(e.target.value)} />
      </label>
      </div>
      {error && <p role="alert">{error}</p>}
      {items && !shown.length && !error && <p className="empty">没有符合条件的事项</p>}
      <ul aria-label="事项列表" className="rows">
        {shown.map((i) => (
          <li key={i.id}>
            <Link to={i.id}>{i.title}</Link> <span className="badge plain">{i.phase}</span>
            <span className="sub">
              {[i.status_label, i.tags?.map((t) => "#" + t).join(" "), i.deadline && `截止 ${new Date(i.deadline).toLocaleString()}`].filter(Boolean).join(" · ")}
            </span>
          </li>
        ))}
      </ul>
    </main>
  );
}

interface Detail { item: Item; relations: Relation[]; keys: { key: string; domain?: string }[]; steps: Step[]; others: Item[] }

function ItemDetail() {
  const { id = "" } = useParams();
  const { api } = useSession();
  const [d, setD] = useState<Detail | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [actErr, setActErr] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);

  const load = useCallback(async () => {
    const eid = encodeURIComponent(id);
    const [it, why, list] = await Promise.all([
      api.get<{ item: Item; relations: Relation[]; correlation_keys?: { key: string; domain?: string }[] }>("/items/" + eid),
      api.get<{ steps: Step[] }>("/why/" + eid),
      api.get<{ items: Item[] }>("/items"),
    ]);
    setD({ item: it.item, relations: it.relations, keys: it.correlation_keys ?? [], steps: why.steps, others: list.items.filter((i) => i.id !== id) });
  }, [api, id]);

  useEffect(() => {
    setD(null);
    setLoadErr(null);
    load().catch((e) => setLoadErr(msg(e)));
  }, [load]);

  // 写入后整页重读：显示的永远是服务端状态，冲突/拒绝时不留下本地假状态。
  const act = async (fn: () => Promise<unknown>) => {
    try {
      await fn();
      setActErr(null);
      setEditing(false);
      await load();
    } catch (e) {
      setActErr(msg(e));
    }
  };

  if (loadErr) return <main><p role="alert">{loadErr}</p></main>;
  if (!d) return <main><p role="status">加载中…</p></main>;
  const { item } = d;
  const path = "/items/" + encodeURIComponent(id);
  const ifMatch = { "If-Match": String(item.version) };

  return (
    <main>
      <p className="eyebrow"><Link to="..">← 事项列表</Link></p>
      <h1>{item.title}</h1>
      <div className="panel facts">
        <p>{`阶段：${item.phase}`}</p>
        <p>{`版本：${item.version}`}</p>
        <p>{`状态标签：${item.status_label || "未设置"}`}</p>
        <p>{item.deadline ? `截止：${new Date(item.deadline).toLocaleString()}` : "截止：未设置"}</p>
        {item.project ? <p>{`项目：${item.project}`}</p> : null}
        {item.planned_for ? <p>{`计划日期：${item.planned_for}`}</p> : null}
        {item.tags?.length ? <p>标签：{item.tags.join("、")}</p> : null}
      </div>
      {item.body ? <Markdown source={item.body} /> : null}
      {actErr && <p role="alert">{actErr}</p>}

      <h2>编辑</h2>
      {editing ? (
        <ContentForm
          initial={{ title: item.title, body: item.body ?? "" }}
          submitLabel="保存"
          onCancel={() => setEditing(false)}
          onSubmit={(v) => act(() => api.patch(path, { title: v.title, body: v.body }, ifMatch))}
        />
      ) : (
        <p><button type="button" onClick={() => setEditing(true)}>编辑</button></p>
      )}
      <LabelForm key={"l" + item.version} item={item} onSave={(v) => act(() => api.patch(path, { status_label: v }, ifMatch))} />
      <DeadlineForm key={"d" + item.version} item={item} onSave={(v) => act(() => api.patch(path, { deadline: v }, ifMatch))} />
      <TransitionForm key={"t" + item.version} onSubmit={(b) => act(() => api.post(path + "/transition", b, ifMatch))} />

      <section aria-label="关联">
        <h2>关系</h2>
        <ul aria-label="关系" className="rows">
          {d.relations.map((r, n) => {
            const out = r.from === item.id;
            const other = out ? r.to : r.from;
            const type = MATERIAL_RELATION_LABELS[r.type] ?? r.type;
            const to = isMaterialId(other) ? "/materials?id=" + encodeURIComponent(other) : "../" + encodeURIComponent(other);
            return <li key={n}>{out ? `${type} →` : `← ${type}`} <Link to={to}>{other}</Link></li>;
          })}
        </ul>
        <RelateForm others={d.others} onSubmit={(b) => act(() => api.post(path + "/relations", b))} />
      </section>

      <section aria-label="关联来源">
        <h2>关联来源线程</h2>
        <ul aria-label="关联键" className="rows">
          {d.keys.map((k, n) => <li key={n}>{k.key}</li>)}
        </ul>
        <KeyForm key={"k" + item.version} onSubmit={(key) => act(() => api.post(path + "/correlation-keys", { key }, ifMatch))} />
      </section>

      <AttachedMessages itemId={id} />

      <section aria-label="变更">
        <h2>时间线</h2>
        <ol aria-label="时间线" className="rows">
          {d.steps.map((s) => (
            <li key={s.seq}>
              <ActorBadge actor={s.actor} /> <strong>{s.type}</strong> <time dateTime={s.at}>{new Date(s.at).toLocaleString()}</time>
              {s.reason ? ` — ${s.reason}` : ""}
            </li>
          ))}
        </ol>
      </section>
    </main>
  );
}

function LabelForm({ item, onSave }: { item: Item; onSave: (v: string) => void }) {
  const [v, setV] = useState(item.status_label ?? "");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); onSave(v); }}>
      <label>状态标签 <input value={v} onChange={(e) => setV(e.target.value)} /></label>
      <button type="submit">保存状态标签</button>
    </form>
  );
}

// datetime-local 是本地时间；发给服务端的是 RFC 3339（UTC）。
function DeadlineForm({ item, onSave }: { item: Item; onSave: (v: string | null) => void }) {
  const [v, setV] = useState("");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); if (v) onSave(new Date(v).toISOString()); }}>
      <label>截止 <input type="datetime-local" value={v} onChange={(e) => setV(e.target.value)} /></label>
      <div className="actions">
        <button type="submit" disabled={!v}>设置截止</button>
        <button type="button" disabled={!item.deadline} onClick={() => onSave(null)}>清除截止</button>
      </div>
    </form>
  );
}

function TransitionForm({ onSubmit }: { onSubmit: (b: Record<string, unknown>) => void }) {
  const [to, setTo] = useState("open");
  const [reason, setReason] = useState("");
  const [evidence, setEvidence] = useState("");
  const [wKind, setWKind] = useState("human");
  const [wRef, setWRef] = useState("");
  return (
    <form
      className="panel"
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit({ to, reason, ...(evidence ? { evidence } : {}), ...(to === "waiting" && wRef ? { waiting_on: { kind: wKind, ref: wRef } } : {}) });
      }}
    >
      <label>目标阶段 <select value={to} onChange={(e) => setTo(e.target.value)}>{PHASES.map((p) => <option key={p}>{p}</option>)}</select></label>
      <label>原因 <input value={reason} onChange={(e) => setReason(e.target.value)} /></label>
      <label>证据 <input value={evidence} onChange={(e) => setEvidence(e.target.value)} /></label>
      {to === "waiting" && (
        <>
          <label>等待对象类别 <select value={wKind} onChange={(e) => setWKind(e.target.value)}>{["human", "executor", "external", "time"].map((k) => <option key={k}>{k}</option>)}</select></label>
          <label>等待对象 <input value={wRef} onChange={(e) => setWRef(e.target.value)} /></label>
        </>
      )}
      <button type="submit">转阶段</button>
    </form>
  );
}

function RelateForm({ others, onSubmit }: { others: Item[]; onSubmit: (b: { to: string; type: string }) => void }) {
  const [to, setTo] = useState("");
  const [type, setType] = useState("relates");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); if (to) onSubmit({ to, type }); }}>
      <label>关联事项 <select value={to} onChange={(e) => setTo(e.target.value)}><option value="">选择…</option>{others.map((o) => <option key={o.id} value={o.id}>{o.id}</option>)}</select></label>
      <label>关系类型 <select value={type} onChange={(e) => setType(e.target.value)}>{REL_TYPES.map((t) => <option key={t}>{t}</option>)}</select></label>
      <button type="submit" disabled={!to}>建立关联</button>
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
  const [v, setV] = useState(initial);
  const [err, setErr] = useState<string | null>(null);
  return (
    <form
      className="panel stack"
      onSubmit={(e) => {
        e.preventDefault();
        if (!v.title.trim()) return setErr("标题不能为空");
        setErr(null);
        onSubmit({ title: v.title.trim(), body: v.body });
      }}
    >
      <label>标题 <input value={v.title} onChange={(e) => setV({ ...v, title: e.target.value })} /></label>
      {extra}
      <label>正文 <textarea placeholder="支持 Markdown" rows={8} value={v.body} onChange={(e) => setV({ ...v, body: e.target.value })} /></label>
      {err && <p role="alert">{err}</p>}
      <div className="actions">
        <button type="submit">{submitLabel}</button>
        {onCancel && <button type="button" onClick={onCancel}>取消</button>}
      </div>
    </form>
  );
}

// 新建直接进 open（本人创建，不走建议流）；校验与授权在服务端，这里只拦空标题。
function NewItem() {
  const { api } = useSession();
  const nav = useNavigate();
  const [projects, setProjects] = useState<{ id: string; name?: string }[]>([]);
  const [project, setProject] = useState("");
  const [deadline, setDeadline] = useState("");
  const [plannedFor, setPlannedFor] = useState("");
  const [err, setErr] = useState<string | null>(null);
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
      setErr(msg(e));
    }
  };

  return (
    <main>
      <p className="eyebrow"><Link to="..">← 事项列表</Link></p>
      <PageHead>新建事项</PageHead>
      {err && <p role="alert">{err}</p>}
      <ContentForm
        initial={{ title: "", body: "" }}
        submitLabel="创建"
        onSubmit={create}
        extra={
          <>
            <label>项目 <select value={project} onChange={(e) => setProject(e.target.value)}><option value="">无</option>{projects.map((p) => <option key={p.id} value={p.id}>{p.name ?? p.id}</option>)}</select></label>
            <label>截止 <input type="datetime-local" value={deadline} onChange={(e) => setDeadline(e.target.value)} /></label>
            <label>计划日期 <input type="date" value={plannedFor} onChange={(e) => setPlannedFor(e.target.value)} /></label>
          </>
        }
      />
    </main>
  );
}

function KeyForm({ onSubmit }: { onSubmit: (key: string) => void }) {
  const [key, setKey] = useState("");
  return (
    <form className="panel" onSubmit={(e) => { e.preventDefault(); if (key.trim()) onSubmit(key.trim()); }}>
      <label>关联来源线程 <input value={key} onChange={(e) => setKey(e.target.value)} /></label>
      <button type="submit" disabled={!key.trim()}>登记关联</button>
    </form>
  );
}
