import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from "react";
import { Link } from "react-router-dom";
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

/** 讨论里 @ 的一个对象（GET /items/{id}/mentionable）：主会话与名录里的执行者，含不在 APA 主机上的；ok 为假时 reason 是类别码。 */
export interface Candidate { id: string; kind: "main" | "executor"; label?: string; host?: string; remote?: boolean; ok: boolean; reason?: string }
/** 一条发言 @ 的对象现在怎样（讨论流发言行的 args.mentions）。 */
export interface MentionState { target: string; kind: "executor" | "main"; state: string; item?: string; run?: string }
const MAX_MENTIONS = 3;

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

  /** 发一条（回复时带 reply_to，@ 了谁带 mentions）；成功后重读讨论并告诉详情（版本号变了）。返回是否写成；失败的原因交给输入框。 */
  const post = async (text: string, replyTo?: string, mentions: string[] = []): Promise<boolean> => {
    await api.post(path + "/comments", { text, ...(replyTo ? { reply_to: replyTo } : {}), ...(mentions.length ? { mentions } : {}) });
    setReplying(null);
    await load();
    onPosted?.();
    return true;
  };

  if (!data) return error != null ? <ErrorNote error={error} /> : <Skeleton label={t.loading} n={3} />;
  const stream = thread(said, data.entries);
  // 每条发言 @ 的对象现在怎样：取经过里同一条发言（args.id）的 mentions。
  const states = new Map(data.entries.filter((e) => e.kind === "progress" && Array.isArray(e.args?.mentions)).map((e) => [String(e.args!.id), e.args!.mentions as MentionState[]]));
  const n = said.length;
  return (
    <section aria-label={t.discussion} className="discussion">
      <h2>{t.discussion}{n > 0 && <span className="count-n"> {n}</span>}</h2>
      {error != null && <ErrorNote error={error} />}
      {stream.length === 0 && <p className="sub">{t.noDiscussion}</p>}
      <ol className="stream" aria-label={t.discussion}>
        {stream.map((x, i) => isComment(x)
          ? <CommentView key={x.p.id} c={x} itemId={item.id} states={states} replying={replying} setReplying={setReplying} post={post} />
          : <EventLine key={"e" + i} e={x} phase={x.item && phases[x.item.id]} />)}
      </ol>
      <Composer itemId={item.id} onSubmit={(text, mentions) => post(text, undefined, mentions)} />
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

function CommentView({ c, itemId, states, replying, setReplying, post, depth = 0 }: {
  c: Comment;
  itemId: string;
  states: Map<string, MentionState[]>;
  replying: string | null;
  setReplying: (id: string | null) => void;
  post: (text: string, replyTo?: string, mentions?: string[]) => Promise<boolean>;
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
      {states.get(p.id)?.length ? <Mentions list={states.get(p.id)!} /> : null}
      <div className="comment-acts">
        <button type="button" className="quiet" aria-expanded={replying === p.id} onClick={() => setReplying(replying === p.id ? null : p.id)}>{t.reply}</button>
      </div>
      {replying === p.id && <Composer itemId={itemId} autoFocus label={t.replyTo(author)} onSubmit={(text, mentions) => post(text, p.id, mentions)} onCancel={() => setReplying(null)} />}
      {c.kids.length > 0 && (
        <ol className="replies" aria-label={t.repliesOf(author)}>
          {c.kids.map((k) => <CommentView key={k.p.id} c={k} itemId={itemId} states={states} replying={replying} setReplying={setReplying} post={post} depth={depth + 1} />)}
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

/** @ 对象的显示名：主会话写「主 Agent」，执行者写它的名字；没起名的写「未命名执行者」，全 id 只在悬停里。 */
function useTargetName() {
  const t = useT(strings);
  const executor = useNames("executor");
  return (id: string, label?: string) => (id === "session:main" ? t.mainSession : label || (executor(id) !== id ? executor(id) : t.unnamedExecutor));
}

/** 一条发言 @ 了谁、各自现在怎样；建出了子事项的链过去。 */
function Mentions({ list }: { list: MentionState[] }) {
  const t = useT(strings);
  const name = useTargetName();
  return (
    <ul className="mention-states" aria-label={t.mentionStates}>
      {list.map((m) => {
        const text = <>@{name(m.target)} · {t.mentionState[m.state] ?? m.state}</>;
        return (
          <li key={m.target} className="badge" data-state={m.state} title={m.target}>
            {m.item ? <Link to={"/items/" + encodeURIComponent(m.item)}>{text}</Link> : text}
          </li>
        );
      })}
    </ul>
  );
}

/**
 * 写一条：多行输入，⌘/Ctrl+Enter 发送；发送失败保留输入并写原因。输入 @ 弹出候选（主会话与执行者，含别的机器上的），
 * ↑↓ 选、回车或 Tab 选中、Esc 关；选中的成为输入框上方的小标签，至多 3 个；叫不动的（在跑、不在名单……）置灰并写原因。
 */
function Composer({ itemId, onSubmit, onCancel, label, autoFocus }: {
  itemId: string;
  onSubmit: (text: string, mentions: string[]) => Promise<boolean>;
  onCancel?: () => void;
  label?: string;
  autoFocus?: boolean;
}) {
  const t = useT(strings);
  const { api } = useSession();
  const name = useTargetName();
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<unknown>(null);
  const [picked, setPicked] = useState<Candidate[]>([]);
  const [cands, setCands] = useState<Candidate[] | null>(null);
  const [query, setQuery] = useState<{ at: number; q: string } | null>(null);
  const [sel, setSel] = useState(0);
  const box = useRef<HTMLTextAreaElement>(null);
  // 候选在第一次输入 @ 时取一次。
  useEffect(() => {
    if (!query || cands) return;
    api.get<{ candidates: Candidate[] }>("/items/" + encodeURIComponent(itemId) + "/mentionable").then((r) => setCands(r.candidates ?? []), () => setCands([]));
  }, [api, itemId, query, cands]);
  const shown = query && cands ? cands.filter((c) => !picked.some((p) => p.id === c.id) && (name(c.id, c.label) + " " + (c.host ?? "")).toLowerCase().includes(query.q.toLowerCase())) : [];
  const full = picked.length >= MAX_MENTIONS;

  /** 光标前是「@词」时进入候选；否则收起。 */
  const track = (v: string, caret: number) => {
    const m = /(^|\s)@([^\s@]*)$/.exec(v.slice(0, caret));
    setQuery(m ? { at: caret - m[2].length - 1, q: m[2] } : null);
    setSel(0);
  };
  const choose = (c: Candidate) => {
    if (!c.ok || full || !query) return;
    const caret = query.at + 1 + query.q.length;
    setText(text.slice(0, query.at) + text.slice(caret));
    setPicked([...picked, c]);
    setQuery(null);
    box.current?.focus();
  };
  const send = async () => {
    if (!text.trim() || busy) return;
    setBusy(true);
    setErr(null);
    try {
      if (await onSubmit(text.trim(), picked.map((p) => p.id))) {
        setText("");
        setPicked([]);
      }
    } catch (e) {
      setErr(e);
    }
    setBusy(false);
  };
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (query && shown.length) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        setSel((sel + (e.key === "ArrowDown" ? 1 : shown.length - 1)) % shown.length);
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        choose(shown[Math.min(sel, shown.length - 1)]);
        return;
      }
    }
    if (e.key === "Escape") {
      if (query) { e.preventDefault(); setQuery(null); return; }
      if (onCancel) { e.preventDefault(); onCancel(); }
    }
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); void send(); }
  };
  const listId = "mention-" + itemId + (label ? "-r" : "");
  return (
    <form className="composer" aria-label={label ?? t.comment} onSubmit={(e) => { e.preventDefault(); void send(); }}>
      {picked.length > 0 && (
        <ul className="chips mention-picked" aria-label={t.mentioning}>
          {picked.map((p) => (
            <li key={p.id} className="badge plain" title={p.id}>
              @{name(p.id, p.label)}
              <button type="button" className="link" aria-label={t.unmention(name(p.id, p.label))} onClick={() => setPicked(picked.filter((x) => x.id !== p.id))}>✕</button>
            </li>
          ))}
        </ul>
      )}
      <div className="composer-box">
        <textarea ref={box} rows={2} aria-label={label ?? t.comment} placeholder={t.commentPlaceholder} value={text} autoFocus={autoFocus}
          role="combobox" aria-expanded={!!query} aria-controls={listId} aria-autocomplete="list"
          aria-activedescendant={query && shown[sel] ? listId + "-" + sel : undefined}
          onChange={(e) => { setText(e.target.value); track(e.target.value, e.target.selectionStart ?? e.target.value.length); }}
          onKeyDown={onKey} />
        {query && (
          <ul id={listId} role="listbox" className="menu-pop mention-pop" aria-label={t.mentionPick}>
            {cands === null && <li className="sub">{t.loading}</li>}
            {full && <li className="sub">{t.mentionFull(MAX_MENTIONS)}</li>}
            {cands !== null && !shown.length && !full && <li className="sub">{t.mentionNone}</li>}
            {!full && shown.map((c, i) => (
              <li key={c.id} id={listId + "-" + i} role="option" aria-selected={i === sel} aria-disabled={!c.ok || undefined} title={c.id}
                onMouseEnter={() => setSel(i)} onMouseDown={(e) => { e.preventDefault(); choose(c); }}>
                <span className="avatar" data-kind={c.kind === "main" ? "owner" : "agent"} aria-hidden="true">{[...name(c.id, c.label)][0]}</span>
                <span className="grow">{name(c.id, c.label)}</span>
                <small>{!c.ok ? t.mentionReason[c.reason ?? ""] ?? t.mentionUnavailable : c.remote ? t.remoteHost(c.host ?? "") : c.host}</small>
              </li>
            ))}
          </ul>
        )}
      </div>
      {err != null && <ErrorNote error={err} />}
      <div className="actions">
        <span className="sub">{t.mentionHint}</span>
        {onCancel && <button type="button" onClick={onCancel}>{t.cancel}</button>}
        <button type="submit" className="primary" disabled={!text.trim() || busy}>{picked.length ? t.sendMention(picked.length) : t.send}</button>
      </div>
    </form>
  );
}
