import { useCallback, useEffect, useState } from "react";
import { Link, Route, Routes, useParams } from "react-router-dom";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { MATERIAL_RELATION_LABELS, isMaterialId } from "../../app/relations";
import { AttachedMessages } from "../attached-messages";

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
      <Route path=":id" element={<ItemDetail />} />
    </Routes>
  );
}

function ItemList() {
  const { api } = useSession();
  const [phase, setPhase] = useState("");
  const [tag, setTag] = useState("");
  const [items, setItems] = useState<Item[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setError(null);
    api.get<{ items: Item[] }>("/items" + (phase ? "?phase=" + encodeURIComponent(phase) : "")).then(
      (r) => setItems(r.items),
      (e) => setError(msg(e)),
    );
  }, [api, phase]);

  const tags = [...new Set((items ?? []).flatMap((i) => i.tags ?? []))].sort();
  const shown = (items ?? []).filter((i) => !tag || i.tags?.includes(tag));

  return (
    <main>
      <PageHead>事项</PageHead>
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

interface Detail { item: Item; relations: Relation[]; steps: Step[]; others: Item[] }

function ItemDetail() {
  const { id = "" } = useParams();
  const { api } = useSession();
  const [d, setD] = useState<Detail | null>(null);
  const [loadErr, setLoadErr] = useState<string | null>(null);
  const [actErr, setActErr] = useState<string | null>(null);

  const load = useCallback(async () => {
    const eid = encodeURIComponent(id);
    const [it, why, list] = await Promise.all([
      api.get<{ item: Item; relations: Relation[] }>("/items/" + eid),
      api.get<{ steps: Step[] }>("/why/" + eid),
      api.get<{ items: Item[] }>("/items"),
    ]);
    setD({ item: it.item, relations: it.relations, steps: why.steps, others: list.items.filter((i) => i.id !== id) });
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
        {item.tags?.length ? <p>标签：{item.tags.join("、")}</p> : null}
      </div>
      {actErr && <p role="alert">{actErr}</p>}

      <h2>编辑</h2>
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
