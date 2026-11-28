import { useCallback, useEffect, useRef, useState } from "react";
import { locale, useT } from "../../app/i18n";
import { useNames } from "../../app/names";
import { useSession } from "../../app/session";
import { ErrorNote } from "../../app/errors";
import { Skeleton } from "../../app/ui";
import { useLabel } from "../../app/values";
import { near, What, type TraceData, type TraceEntry } from "../trace/Trace";
import type { Item } from "./ItemsPage";
import { strings } from "./strings";

// 事项的讨论（主文档 §6.3、界面设计 §9.4）：按时间的一条流。发言（经过里 kind 为 progress 的行）是评论，作者分本人与各 Agent，
// 显示名取 Agent 的名字、不露凭据 id；回复按 reply_to 挂在被回复的那条下面（楼中楼）；决定、派活与交回、后续消息作为流里的小字行，
// 派出去的子事项旁边写它现在的阶段。发言取 GET /items/{id} 的 progress[]（全文、reply_to、只到日的导入条目），
// 流里的其余行取 GET /items/{id}/trace（与讨论流同一份合成、不分页截断），写入走 POST /items/{id}/comments。

/** GET /items/{id} 的 progress[] 的一条：actor 是 owner 或凭据 id。 */
export interface Said { id: string; text: string; on?: string; reply_to?: string; actor: string; at: string; precision?: string }

const EVENTS = new Set(["decision", "delegated", "dispatched", "reported", "follow"]);

interface Comment { p: Said; kids: Comment[] }

/** 发言与流里其余行：顶层按时间排，回复挂到被回复的那条下（找不到被回复的那条时当顶层）。 */
export function thread(said: Said[], entries: TraceEntry[]): (Comment | TraceEntry)[] {
  const byId = new Map(said.map((p): [string, Comment] => [p.id, { p, kids: [] }]));
  const top: (Comment | TraceEntry)[] = entries.filter((e) => EVENTS.has(e.kind) && !e.mech);
  for (const c of [...byId.values()].sort((a, b) => Date.parse(a.p.at) - Date.parse(b.p.at))) {
    const parent = c.p.reply_to ? byId.get(c.p.reply_to) : undefined;
    if (parent && parent !== c) parent.kids.push(c);
    else top.push(c);
  }
  return top.sort((a, b) => Date.parse(isComment(a) ? a.p.at : a.at) - Date.parse(isComment(b) ? b.p.at : b.at));
}
const isComment = (x: Comment | TraceEntry): x is Comment => "kids" in x;
const OWNER = "owner";

export function Discussion({ item, said, rev, onPosted }: { item: Item; said: Said[]; rev?: unknown; onPosted?: () => void }) {
  const t = useT(strings);
  const { api } = useSession();
  const path = "/items/" + encodeURIComponent(item.id);
  const [data, setData] = useState<TraceData | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [phases, setPhases] = useState<Record<string, string>>({});
  const [replying, setReplying] = useState<string | null>(null);

  const load = useCallback(() => api.get<TraceData>(path + "/trace").then((d) => { setData(d); setError(null); }, setError), [api, path]);
  useEffect(() => { void load(); }, [load, rev]);
  // 派出去的子事项现在的阶段：每个子事项取一次。
  useEffect(() => {
    const ids = [...new Set((data?.entries ?? []).filter((e) => ["delegated", "reported", "dispatched"].includes(e.kind) && e.item && !e.item.gone && e.item.id !== item.id).map((e) => e.item!.id))];
    if (!ids.length) return;
    let live = true;
    Promise.all(ids.map((id) => api.get<{ item: Item }>("/items/" + encodeURIComponent(id)).then((r) => [id, r.item.phase] as const, () => null)))
      .then((rs) => live && setPhases(Object.fromEntries(rs.filter((r): r is readonly [string, string] => !!r))));
    return () => { live = false; };
  }, [api, data, item.id]);

  /** 发一条（回复时带 reply_to）；成功后重读讨论并告诉详情（版本号变了）。返回是否写成。 */
  const post = async (text: string, replyTo?: string): Promise<boolean> => {
    try {
      await api.post(path + "/comments", { text, ...(replyTo ? { reply_to: replyTo } : {}) });
    } catch (e) {
      setError(e);
      return false;
    }
    setReplying(null);
    await load();
    onPosted?.();
    return true;
  };

  if (!data) return error != null ? <ErrorNote error={error} /> : <Skeleton label={t.loading} n={3} />;
  const stream = thread(said, data.entries);
  const n = said.length;
  return (
    <section aria-label={t.discussion} className="discussion">
      <h2>{t.discussion}{n > 0 && <span className="count-n"> {n}</span>}</h2>
      {error != null && <ErrorNote error={error} />}
      {stream.length === 0 && <p className="sub">{t.noDiscussion}</p>}
      <ol className="stream" aria-label={t.discussion}>
        {stream.map((x, i) => isComment(x)
          ? <CommentView key={x.p.id} c={x} replying={replying} setReplying={setReplying} post={post} />
          : <EventLine key={"e" + i} e={x} phase={x.item && phases[x.item.id]} />)}
      </ol>
      <Composer onSubmit={(text) => post(text)} />
    </section>
  );
}

/** 作者的显示名：本人写「你」，Agent 写它的名字（凭据归到所属 Agent，不露 id）。 */
function useAuthor() {
  const t = useT(strings);
  const credential = useNames("credential");
  return (actor?: string) => (actor === OWNER ? t.you : actor ? credential(actor) : t.agentFallback);
}

function Author({ actor }: { actor?: string }) {
  const t = useT(strings);
  const name = useAuthor()(actor);
  const owner = actor === OWNER;
  return (
    <span className="author">
      <span className="avatar" data-kind={owner ? "owner" : "agent"} aria-hidden="true">{[...name][0] ?? "?"}</span>
      <b>{name}</b>
      {!owner && <span className="badge plain">{t.agentTag}</span>}
    </span>
  );
}

// 只知道日期的导入条目只显示到日（§7.1）。
const stamp = (p: Said) => (p.precision === "day" ? new Date(p.at).toLocaleDateString(locale()) : near(p.at));

function CommentView({ c, replying, setReplying, post, depth = 0 }: {
  c: Comment;
  replying: string | null;
  setReplying: (id: string | null) => void;
  post: (text: string, replyTo?: string) => Promise<boolean>;
  depth?: number;
}) {
  const t = useT(strings);
  const { p } = c;
  const author = useAuthor()(p.actor);
  return (
    <li className="comment" data-comment={p.id} data-depth={Math.min(depth, 2)}>
      <div className="comment-head">
        <Author actor={p.actor} />
        <time dateTime={p.at} title={p.precision === "day" ? undefined : new Date(p.at).toLocaleString(locale())}>{stamp(p)}</time>
        {p.on && <span className="sub">{t.pointsTo(p.on)}</span>}
      </div>
      <p className="comment-text">{p.text}</p>
      <div className="comment-acts">
        <button type="button" className="quiet" aria-expanded={replying === p.id} onClick={() => setReplying(replying === p.id ? null : p.id)}>{t.reply}</button>
      </div>
      {replying === p.id && <Composer autoFocus label={t.replyTo(author)} onSubmit={(text) => post(text, p.id)} onCancel={() => setReplying(null)} />}
      {c.kids.length > 0 && (
        <ol className="replies" aria-label={t.repliesOf(author)}>
          {c.kids.map((k) => <CommentView key={k.p.id} c={k} replying={replying} setReplying={setReplying} post={post} depth={depth + 1} />)}
        </ol>
      )}
    </li>
  );
}

/** 流里的小字行：决定、派活、交回、后续消息；派出去的子事项写它现在的阶段。 */
function EventLine({ e, phase }: { e: TraceEntry; phase?: string }) {
  const label = useLabel();
  const who = e.who.kind === "agent" ? <Author actor={e.who.name} /> : null;
  return (
    <li className="stream-event" data-kind={e.kind}>
      <span className="dot paused" aria-hidden="true" />
      {who}
      <span className="what"><What e={e} /></span>
      {phase && <span className={"badge" + (phase === "done" ? " ok" : phase === "needs_input" ? " bad" : "")}>{label("phase", phase)}</span>}
      <time dateTime={e.at} title={new Date(e.at).toLocaleString(locale())}>{near(e.at)}</time>
    </li>
  );
}

/** 写一条：多行输入，⌘/Ctrl+Enter 发送；发送失败保留输入。 */
function Composer({ onSubmit, onCancel, label, autoFocus }: { onSubmit: (text: string) => Promise<boolean>; onCancel?: () => void; label?: string; autoFocus?: boolean }) {
  const t = useT(strings);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const box = useRef<HTMLTextAreaElement>(null);
  const send = async () => {
    if (!text.trim() || busy) return;
    setBusy(true);
    if (await onSubmit(text.trim())) setText("");
    setBusy(false);
  };
  return (
    <form className="composer" aria-label={label ?? t.comment} onSubmit={(e) => { e.preventDefault(); void send(); }}>
      <textarea ref={box} rows={2} aria-label={label ?? t.comment} placeholder={t.commentPlaceholder} value={text} autoFocus={autoFocus}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); void send(); }
          if (e.key === "Escape" && onCancel) { e.preventDefault(); onCancel(); }
        }} />
      <div className="actions">
        {onCancel && <button type="button" onClick={onCancel}>{t.cancel}</button>}
        <button type="submit" className="primary" disabled={!text.trim() || busy}>{t.send}</button>
      </div>
    </form>
  );
}
