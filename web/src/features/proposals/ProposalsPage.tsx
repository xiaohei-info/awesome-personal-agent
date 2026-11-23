import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiError, type ApiClient } from "../../app/api";
import { useSession } from "../../app/session";
import { COUNTS_EVENT, PageHead } from "../../app/page";
import { RejectionNote, type Rejection } from "../decisions/RejectionNote";
import { useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { strings } from "./strings";
import { useItemTitle, useNames } from "../../app/names";
import { Val } from "../../app/values";
import { HANDOFF_TAG } from "../../app/ids";
import { Prereqs } from "../../app/Prereqs";
import { handoffPrereqs, useHandoff } from "../items/HandOff";
import { strings as itemStrings } from "../items/strings";

// 形状对应 GET /v1/items 返回的 domain.Item（只取本页用到的字段）。是否可接受/丢弃、授权与只认第一次处理都在服务端。
export interface Proposal {
  id: string;
  version?: number;
  tags?: string[];
  title: string;
  summary?: string;
  priority?: string;
  project?: string;
  origin?: { source: string; actor?: string };
  // 入站流水线写入的建议信息（主文档 §6.3）；agent 直接提出的建议没有 recommend。
  proposal?: { recommend?: "accept" | "attach" | "dismiss"; attach_to?: string; reason?: string; low_confidence: boolean; sources?: string[];
    merges?: Merge[]; similar?: { item: string; score: number }[]; split_from?: string; not_same?: string[]; sweep?: Sweep };
}
/** 一次合并的留痕（§9.3 跨来源合并）：并入的来源事件、当时的内容、判断方与理由；item 是被折叠的那条建议（批量归类才有）。 */
interface Merge { event: string; source: string; title: string; summary?: string; by: string; score?: number; judgment?: string; why?: string; batch?: string; item?: string; at: string }
/** 批量归类的记录：结论、去处、理由、判断方。 */
interface Sweep { state: string; action?: string; target?: string; why?: string; by?: string; at?: string }
interface SourceRead { event: { id: string; source_id: string; subject: string; summary?: string; occurred_at: string; link?: string } }

type Action = "accept" | "dismiss" | "attach" | "handoff" | "split" | "unmerge";
/** 处理完的一行回执：说清结果，并给去处（界面设计 §5.2、§7 路径一）。 */
interface Receipt { id: string; title: string; text: string; to?: string; missing?: boolean }
interface BatchResult { id: string; do: string; ok: boolean; why?: string }



function Card({ item, rejection, candidates, onAct, ready }: { item: Proposal; rejection?: Rejection; candidates: () => Promise<Proposal[]>; onAct: (id: string, action: Action, body: object) => Promise<void>; ready: boolean }) {
  const t = useT(strings);
  const [mode, setMode] = useState<"dismiss" | "attach" | null>(null);
  const [reason, setReason] = useState("");
  const [targets, setTargets] = useState<Proposal[] | null>(null);
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);

  const act = async (action: Action, body: object) => {
    setBusy(true);
    try {
      await onAct(item.id, action, body);
    } finally {
      setBusy(false);
    }
  };
  const openAttach = async () => {
    setMode("attach");
    const list = await candidates();
    setTargets(list);
    const rec = item.proposal?.attach_to;
    if (rec && list.some((t) => t.id === rec)) setTarget(rec);
  };
  const project = useNames("project");
  const credential = useNames("credential");
  const sourceName = useNames("source");
  const p = item.proposal;
  const from = item.origin && t.from(item.origin.actor ? credential(item.origin.actor) : sourceName(item.origin.source));
  const meta = [project(item.project), from].filter(Boolean).join(" · ");
  const pri = item.priority && item.priority !== "normal" ? item.priority : "";

  return (
    <article aria-label={item.title}>
      <h3>{item.title}</h3>
      {(meta || pri) && (
        <p className="meta">
          {meta}
          {meta && pri && " · "}
          <Val k="priority" id={pri} />
        </p>
      )}
      {item.summary && <p>{item.summary}</p>}
      {p && (
        <p className="meta">
          {p.recommend && (
            <span>
              {t.recommend(t.do[p.recommend] ?? p.recommend)}
              {p.reason ? ` — ${p.reason}` : ""}
            </span>
          )}
          {p.recommend && " · "}
          {p.sources?.length ? (
            <Link to={p.sources.length === 1 ? "/sources/events/" + encodeURIComponent(p.sources[0]) : "/items/" + encodeURIComponent(item.id)}>{t.sources(p.sources.length)}</Link>
          ) : (
            <span>{t.sources(0)}</span>
          )}
          {p.low_confidence && <> · <strong title={t.lowConfidenceWhy}>{t.lowConfidence}</strong> <span className="sub">{t.lowConfidenceWhy}</span></>}
        </p>
      )}
      {p && <Similar p={p} />}
      {p?.sources && p.sources.length > 1 && <Sources item={item} onSplit={(event) => onAct(item.id, "split", { event })} onUnmerge={(member) => onAct(member, "unmerge", {})} />}
      <RejectionNote rejection={rejection} what="handle" />
      <div className="actions">
        <button disabled={busy} className={ready ? "primary" : undefined} onClick={() => act("handoff", {})}>{t.acceptHandoff}</button>
        <button disabled={busy} onClick={() => act("accept", {})}>{t.do.accept}</button>
        <button disabled={busy} onClick={openAttach}>{t.do.attach}</button>
        <button disabled={busy} onClick={() => setMode("dismiss")}>{t.do.dismiss}</button>
      </div>
      {mode === "dismiss" && (
        <div>
          <label>
            {t.dismissReason}
            <input value={reason} placeholder={t.reasonPlaceholder} onChange={(e) => setReason(e.target.value)} />
          </label>
          <button disabled={busy} onClick={() => act("dismiss", reason.trim() ? { reason: reason.trim() } : {})}>{t.confirmDismiss}</button>
        </div>
      )}
      {mode === "attach" && (
        <div>
          <label>
            {t.attachTo}
            <select value={target} onChange={(e) => setTarget(e.target.value)} disabled={targets === null}>
              <option value="">{targets === null ? t.loading : t.chooseItem}</option>
              {targets?.map((x) => (
                <option key={x.id} value={x.id}>{x.title}</option>
              ))}
            </select>
          </label>
          <button disabled={busy || !target} onClick={() => act("attach", { to: target })}>{t.confirmAttach}</button>
        </div>
      )}
    </article>
  );
}

/** 只提示、没自动合并的相似建议，以及拆分出来的出处：点名字去看那一条，由本人决定忽略哪条。 */
function Similar({ p }: { p: NonNullable<Proposal["proposal"]> }) {
  const t = useT(strings);
  return (
    <>
      {p.split_from && <p className="sub"><ItemName id={p.split_from} say={t.splitFrom} /></p>}
      {p.sweep?.action === "undone" && p.sweep.target && <p className="sub"><ItemName id={p.sweep.target} say={t.undoneFrom} /></p>}
      {p.similar?.map((x) => (
        <p key={x.item} className="sub">
          <Link to={"/items/" + encodeURIComponent(x.item)}><ItemName id={x.item} say={t.similar} /></Link> · {t.similarWhy(x.score)}
        </p>
      ))}
    </>
  );
}

function ItemName({ id, say }: { id: string; say: (title: string) => string }) {
  const title = useItemTitle(id);
  return <>{say(title || "…")}</>;
}

/** 来自几个来源时展开看每个来源：名字、时间、原话或摘要、原文链接；并入的那几个写明何时、由谁、凭什么合并，并可一键撤销或拆出。 */
function Sources({ item, onSplit, onUnmerge }: { item: Proposal; onSplit: (event: string) => Promise<void>; onUnmerge: (member: string) => Promise<void> }) {
  const t = useT(strings);
  const { api } = useSession();
  const sourceName = useNames("source");
  const [rows, setRows] = useState<SourceRead["event"][] | null>(null);
  const [busy, setBusy] = useState(false);
  const p = item.proposal!;
  const load = () => {
    if (rows) return;
    Promise.all(p.sources!.map((id) => api.get<SourceRead>("/source-events/" + encodeURIComponent(id)).then((r) => r.event, () => ({ id, source_id: "", subject: "", occurred_at: "" }))))
      .then(setRows);
  };
  const merges = new Map((p.merges ?? []).map((m) => [m.event, m]));
  const run = (f: () => Promise<void>) => async () => { setBusy(true); try { await f(); } finally { setBusy(false); } };
  return (
    <details onToggle={(e) => (e.currentTarget as HTMLDetailsElement).open && load()}>
      <summary>{t.sourcesHead}</summary>
      {!rows ? <p role="status">{t.loadingSources}</p> : (
        <ul className="rows">
          {rows.map((ev, i) => {
            const m = merges.get(ev.id);
            const when = ev.occurred_at ? new Date(ev.occurred_at).toLocaleString() : "";
            const why = !m ? (i === 0 ? t.first : "") : [t.mergedBy[m.by] ?? t.mergedBy.default, m.score ? t.score(m.score) : "", t.mergedAt(new Date(m.at).toLocaleString())].filter(Boolean).join(" · ");
            return (
              <li key={ev.id} className="act-row" aria-label={sourceName(m?.source || ev.source_id) || ev.id}>
                <div>
                  <span>{sourceName(m?.source || ev.source_id)} · {when}</span>
                  <span className="sub">{m?.title || ev.subject}{(m?.summary || ev.summary) ? " — " + (m?.summary || ev.summary) : ""}</span>
                  <span className="sub">
                    {why}
                    {m?.judgment && <> · <Link to={"/judgment?id=" + encodeURIComponent(m.judgment)}>{t.seeJudgment}</Link></>}
                    {" · "}<Link to={"/sources/events/" + encodeURIComponent(ev.id)}>{t.original}</Link>
                    {ev.link && <> · <a href={ev.link} target="_blank" rel="noreferrer">{t.link}</a></>}
                  </span>
                  {m?.why && <span className="sub">{t.reasonIs(m.why)}</span>}
                </div>
                {m?.item ? (
                  <button type="button" title={t.unmergeWhy} disabled={busy} onClick={run(() => onUnmerge(m.item!))}>{t.unmerge}</button>
                ) : i > 0 && (
                  <button type="button" title={t.splitWhy} disabled={busy} onClick={run(() => onSplit(ev.id))}>{t.splitOut}</button>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </details>
  );
}

interface Activity {
  days: number; pending: number; model_calls: number; chars: number;
  batches: { batch: string; at: string; size: number; run?: string; chars: number; deferred?: number;
    decisions: { item: string; title: string; source?: string; action: string; target?: string; target_title?: string; why?: string; by: string }[] }[];
}

// 规则比过、没有相似记录而留着的不逐条列，只记条数；折叠掉的排前面，其次是模型看过的。
const quiet = (d: { action: string; by: string }) => d.action === "keep" && d.by === "rule";
const rank = (d: { action: string }) => (d.action === "keep" || d.action === "undone" ? 1 : 0);

/** 最近自动归类（§9.3）：每一批归了几条、调了几次模型、送出多少字；每条写明去处与理由，折叠掉的可以一键撤销。 */
function MergeActivity({ onUnmerge, reload }: { onUnmerge: (member: string) => Promise<void>; reload: number }) {
  const t = useT(strings);
  const { api } = useSession();
  const sourceName = useNames("source");
  const [a, setA] = useState<Activity | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    api.get<Activity>("/merge/activity?days=7").then(setA, () => setA(null));
  }, [api, reload]);
  if (!a || (a.batches.length === 0 && a.pending === 0)) return null;
  const all = a.batches.flatMap((b) => b.decisions);
  const n = (k: string) => all.filter((d) => d.action === k).length;
  const parts = (["attach", "merge", "duplicate", "keep", "undone"] as const).filter((k) => n(k) > 0).map((k) => t.actionCount(t.sweepAction[k], n(k)));
  return (
    <details className="panel" aria-label={t.activityHead}>
      <summary>{t.activitySummary(a.days, parts.join("、") || t.nothingYet, a.model_calls, a.chars, a.pending)}</summary>
      {a.batches.map((b) => (
        <section key={b.batch} aria-label={t.batchHead(new Date(b.at).toLocaleString())}>
          <h3>{t.batchHead(new Date(b.at).toLocaleString())}</h3>
          <p className="sub">{t.batchCost(b.size, !!b.run, b.chars)}{b.deferred ? t.batchDeferred(b.deferred) : ""}</p>
          <ul className="rows">
            {b.decisions.filter((d) => !quiet(d)).sort((x, y) => rank(x) - rank(y)).map((d) => (
              <li key={d.item} className="act-row" aria-label={d.title}>
                <div>
                  <span>
                    <Link to={"/items/" + encodeURIComponent(d.item)}>{d.title}</Link>
                    {d.source && <span className="sub"> · {sourceName(d.source)}</span>}
                  </span>
                  <span className="sub">
                    {t.sweepAction[d.action] ?? d.action}
                    {d.target && <> → <Link to={"/items/" + encodeURIComponent(d.target)}>「{d.target_title || "…"}」</Link></>}
                    {" · "}{t.sweepBy[d.by] ?? d.by}
                  </span>
                  {d.why && <span className="sub">{t.reasonIs(d.why)}</span>}
                </div>
                {(d.action === "attach" || d.action === "merge" || d.action === "duplicate") && (
                  <button type="button" title={t.unmergeWhy} disabled={busy} onClick={async () => { setBusy(true); try { await onUnmerge(d.item); } finally { setBusy(false); } }}>{t.unmerge}</button>
                )}
              </li>
            ))}
          </ul>
          {b.decisions.some(quiet) && <p className="sub">{t.ruleKept(b.decisions.filter(quiet).length)}</p>}
        </section>
      ))}
      <p className="sub"><Link to="/settings/merge">{t.mergeSettings}</Link></p>
    </details>
  );
}

/** 导航上建议流的数量：待处理的建议条数。 */
export const countProposals = (api: ApiClient) => api.get<{ items: Proposal[] }>("/items?phase=proposed").then((r) => r.items.length);

export function ProposalsPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<Proposal[] | null>(null);
  const [rejections, setRejections] = useState<Record<string, Rejection>>({});
  const [error, setError] = useState<unknown>(null);
  const [source, setSource] = useState("");
  const [project, setProject] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [batching, setBatching] = useState(false);
  const [outcome, setOutcome] = useState<{ done: number; skipped: { title: string; why: string }[] } | null>(null);
  const [receipts, setReceipts] = useState<Receipt[]>([]);
  const [activityTick, setActivityTick] = useState(0);
  const it = useT(itemStrings);
  const handoff = useHandoff();
  const pre = handoffPrereqs(it, handoff, true);
  const ready = pre.every((x) => x.ok);

  const load = useCallback(
    () =>
      api.get<{ items: Proposal[]; last_rejections?: Record<string, Rejection> }>("/items?phase=proposed").then(
        (r) => {
          setRejections(r.last_rejections ?? {});
          setItems(r.items);
        },
        (e) => setError(e),
      ),
    [api],
  );

  useEffect(() => {
    load();
  }, [load]);

  // 候选 = open 事项；失败时抛给调用方展示，不静默给空列表。
  const candidates = useCallback(
    () =>
      api.get<{ items: Proposal[] }>("/items?phase=open").then(
        (r) => r.items,
        (e) => {
          setError(e);
          return [] as Proposal[];
        },
      ),
    [api],
  );

  // 处理完收成一行回执。「接受并交给 Agent」先给这条建议加上「交给 Agent」再接受：事项进入进行中时带着标记，规则得以触发；
  // 前提没满足时只接受、不交出，回执写明，下面列出还差的几步（只是两个现有接口，判定都在服务端）。
  const act = async (id: string, action: Action, body: object) => {
    setError(null);
    const item = (items ?? []).find((i) => i.id === id);
    const title = item?.title ?? id;
    const path = "/items/" + encodeURIComponent(id);
    try {
      if (action === "handoff") {
        if (ready) await api.patch(path, { tags: [...(item?.tags ?? []).filter((x) => x !== HANDOFF_TAG), HANDOFF_TAG], reason: it.handoff }, item?.version ? { "If-Match": String(item.version) } : undefined);
        await api.post(path + "/accept", {});
        const who = handoff.preview?.selected?.agent_name;
        setReceipts((r) => [...r, { id, title, to: "/items/" + encodeURIComponent(id) + "#agent", text: ready ? t.handedTo(who || t.agent) : t.acceptedNotHanded, missing: !ready }]);
      } else if (action === "unmerge") {
        await api.post(path + "/unmerge", {});
        setReceipts((x) => [...x, { id, title: item?.title ?? t.foldedOne, text: t.unmergeDone, to: path }]);
        setActivityTick((n) => n + 1);
      } else if (action === "split") {
        const r = await api.post<{ item: string }>("/source-events/" + encodeURIComponent((body as { event: string }).event) + "/split", { from: id });
        setReceipts((x) => [...x, { id, title, text: t.splitDone, to: "/items/" + encodeURIComponent(r.item) }]);
      } else {
        await api.post(path + "/" + action, body);
        const target = action === "attach" ? (body as { to: string }).to : id;
        setReceipts((r) => [...r, { id, title, text: t.did[action], to: action === "dismiss" ? undefined : "/items/" + encodeURIComponent(target) }]);
      }
    } catch (e) {
      setError(e); // 含 409：已有人先处理 / 目标已终结，刷新后以服务端为准
    }
    await load();
    window.dispatchEvent(new Event(COUNTS_EVENT));
  };

  const all = items ?? [];
  const opts = (pick: (i: Proposal) => string | undefined) => [...new Set(all.map(pick).filter((v): v is string => !!v))].sort();
  const shown = all.filter((i) => (!source || i.origin?.source === source) && (!project || i.project === project));
  const recommended = shown.filter((i) => i.proposal?.recommend).length;
  const projectName = useNames("project");
  const sourceName = useNames("source");
  // 按推荐处理前先看清楚会发生什么：各推荐处置各有几条。
  const plan = (["accept", "attach", "dismiss"] as const)
    .map((a) => [a, shown.filter((i) => i.proposal?.recommend === a).length] as const)
    .filter(([, n]) => n > 0)
    .map(([a, n]) => t.planPart(t.do[a], n))
    .join(" · ");

  const batch = async () => {
    setConfirming(false);
    setBatching(true);
    setError(null);
    setOutcome(null);
    try {
      const filter = { ...(source && { source }), ...(project && { project }) };
      const r = await api.post<{ results: BatchResult[] }>("/proposals/batch", { mode: "recommended", filter });
      const title = (id: string) => all.find((i) => i.id === id)?.title ?? id;
      setOutcome({
        done: r.results.filter((x) => x.ok).length,
        skipped: r.results.filter((x) => !x.ok).map((x) => ({ title: title(x.id), why: x.why ?? t.notHandled })),
      });
    } catch (e) {
      setError(e);
    } finally {
      setBatching(false);
    }
    await load();
    window.dispatchEvent(new Event(COUNTS_EVENT));
  };

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <MergeActivity onUnmerge={(member) => act(member, "unmerge", {})} reload={activityTick} />
      {error != null && (
        <div>
          <ErrorNote error={error} />
          <button
            onClick={() => {
              setError(null);
              load();
            }}
          >
            {t.retry}
          </button>
        </div>
      )}
      {outcome && (
        <div role="status" aria-label={t.batchResult}>
          <p>
            {t.handled(outcome.done)}
            {outcome.skipped.length > 0 && t.skipped(outcome.skipped.length)}
          </p>
          {outcome.skipped.length > 0 && (
            <ul>
              {outcome.skipped.map((x, n) => (
                <li key={n}>
                  {x.title}
                  {t.colon}
                  {x.why}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {receipts.length > 0 && (
        <ul className="rows" aria-label={t.receipts}>
          {receipts.map((r) => (
            <li key={r.id} role="status">
              {r.title}{t.colon}{r.text}{r.to && <> · <Link to={r.to}>{t.seeItem}</Link></>}
              {r.missing && <Prereqs label={it.prereqs} items={pre} />}
            </li>
          ))}
        </ul>
      )}
      {items === null ? (
        error == null && <p role="status">{t.loading}</p>
      ) : items.length === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        <>
          <div className="actions">
            <label>
              {t.source}
              <select value={source} onChange={(e) => setSource(e.target.value)}>
                <option value="">{t.all}</option>
                {opts((i) => i.origin?.source).map((v) => (
                  <option key={v} value={v}>{sourceName(v)}</option>
                ))}
              </select>
            </label>
            <label>
              {t.project}
              <select value={project} onChange={(e) => setProject(e.target.value)}>
                <option value="">{t.all}</option>
                {opts((i) => i.project).map((v) => (
                  <option key={v} value={v}>{projectName(v)}</option>
                ))}
              </select>
            </label>
            {confirming ? (
              <>
                <span>{plan}</span>
                <button disabled={batching} onClick={batch}>{t.confirmBatch}</button>
                <button onClick={() => setConfirming(false)}>{t.cancel}</button>
              </>
            ) : (
              <button disabled={batching || recommended === 0} onClick={() => setConfirming(true)}>{t.batch(recommended)}</button>
            )}
          </div>
          {shown.map((i) => (
            <Card key={i.id} item={i} rejection={rejections[i.id]} candidates={candidates} onAct={act} ready={ready} />
          ))}
        </>
      )}
    </main>
  );
}
