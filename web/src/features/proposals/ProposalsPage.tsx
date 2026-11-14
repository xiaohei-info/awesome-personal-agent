import { useCallback, useEffect, useState } from "react";
import { ApiError, type ApiClient } from "../../app/api";
import { useSession } from "../../app/session";
import { COUNTS_EVENT, PageHead } from "../../app/page";
import { RejectionNote, type Rejection } from "../decisions/RejectionNote";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 形状对应 GET /v1/items 返回的 domain.Item（只取本页用到的字段）。是否可接受/丢弃、授权与只认第一次处理都在服务端。
export interface Proposal {
  id: string;
  title: string;
  summary?: string;
  priority?: string;
  project?: string;
  origin?: { source: string; actor?: string };
  // 入站流水线写入的建议信息（主文档 §6.3）；agent 直接提出的建议没有 recommend。
  proposal?: { recommend?: "accept" | "attach" | "dismiss"; attach_to?: string; reason?: string; low_confidence: boolean; sources?: string[] };
}

type Action = "accept" | "dismiss" | "attach";
interface BatchResult { id: string; do: string; ok: boolean; why?: string }



function Card({ item, rejection, candidates, onAct }: { item: Proposal; rejection?: Rejection; candidates: () => Promise<Proposal[]>; onAct: (id: string, action: Action, body: object) => Promise<void> }) {
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
  const p = item.proposal;
  const meta = [item.project, item.origin && t.from(item.origin.actor || item.origin.source), item.priority && item.priority !== "normal" ? item.priority : ""].filter(Boolean).join(" · ");

  return (
    <article aria-label={item.title}>
      <h3>{item.title}</h3>
      {meta && <p className="meta">{meta}</p>}
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
          <span>{t.sources(p.sources?.length ?? 0)}</span>
          {p.low_confidence && <> · <strong>{t.lowConfidence}</strong></>}
        </p>
      )}
      <RejectionNote rejection={rejection} what="handle" />
      <div className="actions">
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

/** 导航上建议流的数量：待处理的建议条数。 */
export const countProposals = (api: ApiClient) => api.get<{ items: Proposal[] }>("/items?phase=proposed").then((r) => r.items.length);

export function ProposalsPage() {
  const t = useT(strings);
  const { api } = useSession();
  const [items, setItems] = useState<Proposal[] | null>(null);
  const [rejections, setRejections] = useState<Record<string, Rejection>>({});
  const [error, setError] = useState<string | null>(null);
  const [source, setSource] = useState("");
  const [project, setProject] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [batching, setBatching] = useState(false);
  const [outcome, setOutcome] = useState<{ done: number; skipped: { title: string; why: string }[] } | null>(null);

  const load = useCallback(
    () =>
      api.get<{ items: Proposal[]; last_rejections?: Record<string, Rejection> }>("/items?phase=proposed").then(
        (r) => {
          setRejections(r.last_rejections ?? {});
          setItems(r.items);
        },
        (e) => setError(msg(e)),
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
          setError(msg(e));
          return [] as Proposal[];
        },
      ),
    [api],
  );

  const act = async (id: string, action: Action, body: object) => {
    setError(null);
    try {
      await api.post("/items/" + encodeURIComponent(id) + "/" + action, body);
    } catch (e) {
      setError(msg(e)); // 含 409：已有人先处理 / 目标已终结，刷新后以服务端为准
    }
    await load();
    window.dispatchEvent(new Event(COUNTS_EVENT));
  };

  const all = items ?? [];
  const opts = (pick: (i: Proposal) => string | undefined) => [...new Set(all.map(pick).filter((v): v is string => !!v))].sort();
  const shown = all.filter((i) => (!source || i.origin?.source === source) && (!project || i.project === project));
  const recommended = shown.filter((i) => i.proposal?.recommend).length;

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
      setError(msg(e));
    } finally {
      setBatching(false);
    }
    await load();
    window.dispatchEvent(new Event(COUNTS_EVENT));
  };

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {error && (
        <div role="alert">
          {error}{" "}
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
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
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
                  <option key={v} value={v}>{v}</option>
                ))}
              </select>
            </label>
            <label>
              {t.project}
              <select value={project} onChange={(e) => setProject(e.target.value)}>
                <option value="">{t.all}</option>
                {opts((i) => i.project).map((v) => (
                  <option key={v} value={v}>{v}</option>
                ))}
              </select>
            </label>
            {confirming ? (
              <>
                <span>{t.batchConfirm(recommended)}</span>
                <button disabled={batching} onClick={batch}>{t.confirmBatch}</button>
                <button onClick={() => setConfirming(false)}>{t.cancel}</button>
              </>
            ) : (
              <button disabled={batching || recommended === 0} onClick={() => setConfirming(true)}>{t.batch}</button>
            )}
          </div>
          {shown.map((i) => (
            <Card key={i.id} item={i} rejection={rejections[i.id]} candidates={candidates} onAct={act} />
          ))}
        </>
      )}
    </main>
  );
}
