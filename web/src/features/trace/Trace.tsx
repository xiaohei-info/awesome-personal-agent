import { useEffect, useState, type ReactNode } from "react";
import { Skeleton } from "../../app/ui";
import { Link, useParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { locale, useLang, useT } from "../../app/i18n";
import { useRaw } from "../../app/raw";
import { ErrorNote } from "../../app/errors";
import { useNames } from "../../app/names";
import { entryOf, useLabel } from "../../app/values";
import { HANDOFF_TAG } from "../../app/ids";
import { strings } from "./strings";
import { RawColumn } from "./RawColumn";

// 处理经过（设计 §15.1）：GET /v1/source-events/{id}/trace 与 GET /v1/items/{id}/trace 的展示。每行「谁 · 做了什么 · 结果」，
// 句子由本目录的词表按行的种类与参数生成；相邻的同类机械记录折成一行；时间就近显示；原始记录只在「显示原始值」里。

export interface TraceStep { seq: number; type: string; object: string; actor: { type: string; principal: string; via: string }; at: string; reason?: string; payload?: unknown; basis?: unknown }
export interface TraceEntry {
  at: string;
  who: { kind: string; name?: string; role?: string };
  kind: string;
  args?: Record<string, unknown>;
  item?: { id: string; title?: string; gone?: boolean };
  mech?: boolean;
  steps: TraceStep[];
}
export interface TraceHead { id: string; source: string; type: string; subject?: string; sender?: string; sender_verified: boolean; link?: string; disposition: string; occurred_at: string; arrived_at?: string }
export interface TraceData { message?: TraceHead; origins?: TraceData[]; more_origins?: number; entries: TraceEntry[] }

type T = (typeof strings)["zh"];
const pct = (p: unknown) => `${Math.round(Number(p) * 100)}%`;
const strs = (v: unknown) => (Array.isArray(v) ? (v as string[]) : []);

/** 时间就近显示：与上一行同一分钟不重复；今天只写时分；今年写月日时分；更早写完整日期。 */
export function near(at: string, prev?: string, now = new Date()): string {
  const d = new Date(at);
  if (prev && Math.floor(new Date(prev).getTime() / 60000) === Math.floor(d.getTime() / 60000)) return "";
  const hm: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit", hour12: false };
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString(locale(), hm);
  if (d.getFullYear() === now.getFullYear()) return d.toLocaleString(locale(), { month: "numeric", day: "numeric", ...hm });
  return d.toLocaleString(locale(), { year: "numeric", month: "numeric", day: "numeric", ...hm });
}

/** 相邻、同类、同一主语的机械记录折成一组；其余每行一组。 */
export function fold(es: TraceEntry[]): TraceEntry[][] {
  const out: TraceEntry[][] = [];
  for (const e of es) {
    const last = out[out.length - 1]?.[0];
    if (e.mech && last?.mech && last.kind === e.kind && last.who.kind === e.who.kind && last.who.name === e.who.name) out[out.length - 1].push(e);
    else out.push([e]);
  }
  return out;
}

function useWho() {
  const t = useT(strings);
  const lang = useLang();
  const source = useNames("source");
  const credential = useNames("credential");
  return (w: TraceEntry["who"]): string => {
    switch (w.kind) {
      case "owner": return t.you;
      case "agent": return w.name ? t.agentNamed(credential(w.name)) : t.agent;
      case "rule": return w.name ? t.rule(ruleName(lang, w.name)) : t.someRule;
      case "model": return w.role === "generate" ? t.writer : w.name ? t.judgeNamed(w.name) : t.judge;
      case "source": return source(w.name);
    }
    return t.system;
  };
}

/** 规则名：内置规则查词表；能力包自带的规则用它在包里的 id（本人或包作者起的名字）。 */
export const ruleName = (lang: "zh" | "en", key: string) => entryOf(lang, "rule", key)?.label ?? key.split("/").pop()!;

function ItemLink({ item }: { item?: TraceEntry["item"] }) {
  if (!item) return null;
  if (item.gone) return <>「{item.title || "—"}」</>;
  return <Link to={"/items/" + encodeURIComponent(item.id)}>{item.title || "—"}</Link>;
}

interface Level { material?: Mat; backend: string; model?: string; p?: number; seg?: string; answer?: string; outcome: string; error?: string }

/** 一条判断记录按级展开成多行：每行主语是那一级的模型，按答题顺序；各级都没定时末尾补一行系统的保守做法。
 * 试用打分与过保留期的记录不展开。原始记录随最后一行。 */
export function expand(es: TraceEntry[]): TraceEntry[] {
  return es.flatMap((e) => {
    const a = e.args ?? {};
    if (e.kind !== "judged" || a.expired || a.shadow) return [e];
    const ls = (a.levels ?? []) as Level[];
    const rows: TraceEntry[] = ls.map((l, i) => ({ ...e, who: { kind: "model", role: "judge", name: l.model || undefined }, args: { level: l, more: i < ls.length - 1, low: a.low, high: a.high }, steps: [] }));
    if (a.decided_by === "default" || !rows.length) rows.push({ ...e, who: { kind: "system" }, args: { undecided: true }, steps: [] });
    rows[rows.length - 1].steps = e.steps;
    return rows;
  });
}

function judged(t: T, a: Record<string, unknown>): string {
  if (a.expired) return a.answer === "no" ? t.expiredNo : t.expiredYes;
  if (a.shadow) return t.shadow(pct(((a.levels ?? []) as Level[]).find((l) => l.p !== undefined)?.p ?? 0));
  const lv = a.level as Level | undefined;
  if (!lv) return t.undecided;
  const next = a.more ? t.passOn : "";
  if (lv.outcome === "error" || lv.outcome === "skipped") return t.levelFailed(t.levelWhy[lv.error ?? ""] ?? t.levelWhy.error) + next;
  const lo = pct(a.low), hi = pct(a.high);
  if (lv.p === undefined) {
    if (lv.outcome === "uncertain") return t.chatUnsure + next;
    return lv.answer === "no" ? t.chatNo : t.chatYes;
  }
  if (lv.seg === "below") return t.below(pct(lv.p), lo);
  if (lv.seg === "above") return t.above(pct(lv.p), hi);
  return t.between(pct(lv.p), lo, hi) + next;
}

/** 一行的「做了什么 · 结果」。 */
function What({ e, subjects }: { e: TraceEntry; subjects?: boolean }) {
  const t = useT(strings);
  const label = useLabel();
  const executor = useNames("executor");
  const a = e.args ?? {};
  const fields = (v: unknown) => strs(v).map((f) => t.fields[f] ?? f).join(t.listSep);
  const item = <ItemLink item={e.item} />;
  switch (e.kind) {
    case "arrived": {
      const s = String(a.sender ?? "");
      const head = subjects && a.subject ? t.arrivedSubject(s, String(a.subject)) : t.arrived(s);
      // 事项经过里的来源消息可以单独打开它自己的经过。
      const text = a.origin ? <Link to={"/sources/events/" + encodeURIComponent(String(a.origin))}>{head}</Link> : head;
      return <>{text}{s ? ` · ${a.verified ? t.verified : t.unverified}` : ""}</>;
    }
    case "captured": return <>{t.captured}</>;
    case "rule": return <>{t.then[String(a.then)] ?? t.then.judge}</>;
    case "judged": return <>{judged(t, a)}</>;
    case "backlog": return <>{t.backlog(t.backlogWhy[String(a.reason)] ?? t.backlogWhy.model_unavailable)}</>;
    case "expired": return <>{t.expired}</>;
    case "requeued": return <>{t.requeued}</>;
    case "ignored": return <>{t.ignored}</>;
    case "proposed": return <>{a.count ? t.proposedMany(Number(a.count)) : t.proposed} {item}{a.low_confidence ? ` · ${t.lowConfidence}` : ""}</>;
    case "merged": return <>{t.merged} {item}</>;
    case "digest": return <>{t.digest}</>;
    case "alerted": return <>{t.alerted}</>;
    case "reminded": return <>{t.reminded}</>;
    case "attached": return <>{a.via === "proposal" ? t.attachedProposal : t.attachedFollow} {item}{strs(a.fields).length ? ` · ${t.rewrote(fields(a.fields))}` : ""}</>;
    case "drafted": {
      const o = String(a.outcome);
      if (o === "ok") return <>{t.draftOk}</>;
      if (o === "failed") return <>{t.draftFailed(t.draftWhy[String(a.code)] ?? t.draftWhy.model)}</>;
      if (o === "skipped") return <>{t.draftSkipped}</>;
      return <>{t.draftUnknown}</>;
    }
    case "spot": return <>{a.verdict === "should_see" ? t.spotShouldSee : t.spotCorrect} {item}</>;
    case "created": return <>{t.created} {item}</>;
    case "accepted": return <>{t.accepted}</>;
    case "dismissed": return <>{t.dismissed}</>;
    case "phase": {
      const w = a.waiting_kind === "executor" ? t.waitingFor(executor(String(a.waiting_ref))) : "";
      return <>{t.phase(label("phase", String(a.to)))}{w ? ` · ${w}` : ""}</>;
    }
    case "edited": {
      const add = strs(a.tags_added), del = strs(a.tags_removed);
      const parts: string[] = [];
      if (add.includes(HANDOFF_TAG)) parts.push(t.handedToAgent);
      if (del.includes(HANDOFF_TAG)) parts.push(t.takenBack);
      const rest = strs(a.fields).filter((f) => f !== "tags");
      if (rest.length) parts.push(t.edited(fields(rest)));
      const plainAdd = add.filter((x) => x !== HANDOFF_TAG), plainDel = del.filter((x) => x !== HANDOFF_TAG);
      if (plainAdd.length) parts.push(t.tagsAdded(plainAdd.join(t.listSep)));
      if (plainDel.length) parts.push(t.tagsRemoved(plainDel.join(t.listSep)));
      return <>{parts.length ? parts.join(" · ") : t.editedNothing}</>;
    }
    case "synced": return <>{strs(a.fields).length ? t.synced(fields(a.fields)) : t.editedNothing}</>;
    case "related": return <>{t.related}</>;
    case "absorbed": return <>{t.absorbed} {item}</>;
    case "attach_to": return <>{t.attachTo} {item}</>;
    case "progress": return <>{t.progress(String(a.text ?? ""))}</>;
    case "deadline_passed": return <>{t.deadline}</>;
    case "decision": return <>{t.decision[String(a.state)] ?? t.internal}</>;
    case "follow": {
      const what = t.follow[String(a.state)] ?? t.internal;
      const quote = a.text ?? a.subject; // 评论的标题常是所属对象的标题，正文才区分得开
      return <>{what}{quote ? `「${String(quote)}」` : ""}{a.count ? t.followCount(Number(a.count)) : ""}</>;
    }
    case "observed": return <>{t.observed}</>;
    case "run": {
      // 以 Agent 本人身份运行时写明这次在哪里做、按哪一级设置；选了对话里却还没打开唤醒时说原因（§13.6）。
      if (a.state !== "launched" || a.runner !== "main_session") return <>{t.run[String(a.state)] ?? t.internal}</>;
      const how = t.launched[String(a.launch)] ?? t.run.launched;
      const from = t.launchFrom[String(a.from)];
      return <>{how}{from ? ` · ${from}` : ""}{a.fallback ? ` · ${t.launchFallback}` : ""}</>;
    }
    case "generated": {
      const w = t.templates[String(a.template)];
      const st = a.state === "failed" ? "failed" : "succeeded";
      return <>{w ? t.generatedOf[st](w) : t.generated[st]}</>;
    }
    case "alert": return <>{t.alert}</>;
    case "delegated": return <>{t.delegated(executor(String(a.assignee ?? "")))} {item}</>;
    case "dispatched": return <>{t.dispatched(executor(String(a.executor ?? "")))} {item}</>;
    case "dispatch_updated": return <>{t.dispatchUpdated(executor(String(a.executor ?? "")), t.dispatchState[String(a.state)] ?? String(a.state ?? ""))}</>;
    case "reported": return <>{t.reported(executor(String(a.assignee ?? "")), label("phase", String(a.phase)))} {item}</>;
    case "rejected": return <>{t.rejected}</>;
  }
  const type = String(a.type ?? "");
  return <>{label("event", type) !== type ? label("event", type) : t.internal}</>;
}

interface Mat { sent_chars: number; original_chars: number }
const isMat = (m: unknown): m is Mat => typeof (m as Mat)?.sent_chars === "number" && typeof (m as Mat)?.original_chars === "number";

/** 这一行送给模型的资料裁剪量（§8.4、§15.1）：判断的一级读那一级的 material，处置行读处置载荷的 material；旧数据没有就不显示。 */
export function materialOf(e: TraceEntry): Mat | null {
  if (e.kind === "judged") return isMat((e.args?.level as Level | undefined)?.material) ? (e.args!.level as Level).material! : null;
  for (const s of e.steps) {
    const m = (s.payload as { material?: unknown } | undefined)?.material;
    if (s.type === "inbound.disposed" && isMat(m)) return m;
  }
  return null;
}

function Raw({ steps }: { steps: TraceStep[] }) {
  const raw = useRaw();
  if (!raw || !steps.length) return null;
  return <pre className="raw-steps">{steps.map((s) => JSON.stringify(s, null, 1)).join("\n")}</pre>;
}

function Line({ e, prev, subjects, extra }: { e: TraceEntry; prev?: string; subjects?: boolean; extra?: ReactNode }) {
  const who = useWho();
  // 导入的变更只知道日期时（payload.precision 为 day）只显示到日（§7.1）。
  const day = e.steps.some((x) => (x.payload as { precision?: string } | undefined)?.precision === "day");
  const at = day ? new Date(e.at).toLocaleDateString(locale()) : near(e.at, prev);
  const t = useT(strings);
  const mat = materialOf(e);
  const fmt = (n: number) => n.toLocaleString(locale());
  return (
    <li data-kind={e.kind}>
      <div className="trace-line">
        <span className="who">{who(e.who)}</span>
        <span className="what"><What e={e} subjects={subjects} />{mat ? ` · ${t.material(fmt(mat.sent_chars), fmt(mat.original_chars))}` : ""}{extra}</span>
        <time className="when" dateTime={e.at} title={day ? at : new Date(e.at).toLocaleString(locale())}>{at}</time>
      </div>
      <Raw steps={e.steps} />
    </li>
  );
}

function Group({ g, prev, subjects }: { g: TraceEntry[]; prev?: string; subjects?: boolean }) {
  const t = useT(strings);
  const [open, setOpen] = useState(false);
  if (g.length === 1) return <Line e={g[0]} prev={prev} subjects={subjects} />;
  const toggle = <> <button type="button" className="link" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? t.hideMore : t.showMore(g.length - 1)}</button></>;
  return (
    <>
      <Line e={g[0]} prev={prev} subjects={subjects} extra={toggle} />
      {open && g.slice(1).map((e, i) => <Line key={i} e={e} prev={g[i].at} subjects={subjects} />)}
    </>
  );
}

/** 一份经过的列表：事项经过先列来源消息，再列事项本身。 */
export function TraceList({ data }: { data: TraceData }) {
  const t = useT(strings);
  const origins = data.origins ?? [];
  // 事项经过里的来源消息写出标题，并能单独打开那条消息的经过。
  const from = (o: TraceData) => o.entries.map((e) => (e.kind === "arrived" && o.message ? { ...e, args: { ...e.args, subject: o.message.subject, origin: o.message.id } } : e));
  const entries = expand([...origins.flatMap(from), ...[...data.entries].sort((a, b) => Date.parse(a.at) - Date.parse(b.at))]);
  if (!entries.length) return <p className="sub">{t.empty}</p>;
  const subjects = origins.length > 0;
  const groups = fold(entries);
  return (
    <>
      <ol aria-label={t.title} className="rows trace">
        {groups.map((g, i) => (
          <Group key={i} g={g} subjects={subjects} prev={i > 0 ? groups[i - 1][groups[i - 1].length - 1].at : undefined} />
        ))}
      </ol>
      {data.more_origins ? <p className="sub">{t.moreOrigins(data.more_origins)}</p> : null}
    </>
  );
}

/** 按路径取一份经过并显示；rev 变化时重取（事项详情写入后）。 */
export function TraceView({ path, rev }: { path: string; rev?: unknown }) {
  const t = useT(strings);
  const { api } = useSession();
  const [data, setData] = useState<TraceData | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    api.get<TraceData>(path).then((d) => live && (setData(d), setError(null)), (e) => live && setError(e));
    return () => {
      live = false;
    };
  }, [api, path, rev]);
  if (error != null) return <ErrorNote error={error} />;
  if (!data) return <Skeleton label={t.loading} />;
  return <TraceList data={data} />;
}

/** /sources/events/:id：一条消息的处理经过单独成页（消息来源下的子页，不是一级页面）。 */
export function MessageTracePage() {
  const t = useT(strings);
  const { id = "" } = useParams();
  const { api } = useSession();
  const source = useNames("source");
  const [data, setData] = useState<TraceData | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    api.get<TraceData>(`/source-events/${encodeURIComponent(id)}/trace`).then(setData, setError);
  }, [api, id]);
  if (error != null) return <main><ErrorNote error={error} /></main>;
  if (!data?.message) return <main><Skeleton shape="card" label={t.loading} /></main>;
  const m = data.message;
  return (
    <main>
      <p className="backbar"><Link to="/sources">{source(m.source)}</Link></p>
      <h1>{m.link ? <a href={m.link} target="_blank" rel="noreferrer noopener">{m.subject || m.type}</a> : m.subject || m.type}</h1>
      <RawColumn id={m.id} />
      <h2>{t.title}</h2>
      <TraceList data={data} />
    </main>
  );
}
