import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { RejectionNote, type Rejection } from "../decisions/RejectionNote";

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

const DO: Record<string, string> = { accept: "接受", attach: "补到已有事项", dismiss: "忽略" };

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

function Card({ item, rejection, candidates, onAct }: { item: Proposal; rejection?: Rejection; candidates: () => Promise<Proposal[]>; onAct: (id: string, action: Action, body: object) => Promise<void> }) {
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
  const meta = [item.project, item.origin && `来自 ${item.origin.actor || item.origin.source}`, item.priority && item.priority !== "normal" ? item.priority : ""].filter(Boolean).join(" · ");

  return (
    <article aria-label={item.title}>
      <h3>{item.title}</h3>
      {meta && <p className="meta">{meta}</p>}
      {item.summary && <p>{item.summary}</p>}
      {p && (
        <p className="meta">
          {p.recommend && (
            <span>
              推荐：{DO[p.recommend]}
              {p.reason ? ` — ${p.reason}` : ""}
            </span>
          )}
          {p.recommend && " · "}
          <span>来源 {p.sources?.length ?? 0} 条</span>
          {p.low_confidence && <> · <strong>把握低</strong></>}
        </p>
      )}
      <RejectionNote rejection={rejection} what="处理" />
      <div className="actions">
        <button disabled={busy} onClick={() => act("accept", {})}>接受</button>
        <button disabled={busy} onClick={openAttach}>补到已有事项</button>
        <button disabled={busy} onClick={() => setMode("dismiss")}>忽略</button>
      </div>
      {mode === "dismiss" && (
        <div>
          <label>
            忽略原因
            <input value={reason} onChange={(e) => setReason(e.target.value)} />
          </label>
          <button disabled={busy || !reason.trim()} onClick={() => act("dismiss", { reason: reason.trim() })}>确认忽略</button>
        </div>
      )}
      {mode === "attach" && (
        <div>
          <label>
            补到哪个事项
            <select value={target} onChange={(e) => setTarget(e.target.value)} disabled={targets === null}>
              <option value="">{targets === null ? "加载中…" : "选择事项"}</option>
              {targets?.map((t) => (
                <option key={t.id} value={t.id}>{t.title}</option>
              ))}
            </select>
          </label>
          <button disabled={busy || !target} onClick={() => act("attach", { to: target })}>确认补到</button>
        </div>
      )}
    </article>
  );
}

export function ProposalsPage() {
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
        skipped: r.results.filter((x) => !x.ok).map((x) => ({ title: title(x.id), why: x.why ?? "未处理" })),
      });
    } catch (e) {
      setError(msg(e));
    } finally {
      setBatching(false);
    }
    await load();
  };

  return (
    <main>
      <PageHead lede="agent 从来源里读到、觉得值得做的事。可接受、补到已有事项或忽略（要写原因）；带推荐的可一次按推荐处理。">建议流</PageHead>
      {error && (
        <div role="alert">
          {error}{" "}
          <button
            onClick={() => {
              setError(null);
              load();
            }}
          >
            重试
          </button>
        </div>
      )}
      {outcome && (
        <div role="status" aria-label="批量处理结果">
          <p>已处理 {outcome.done} 条{outcome.skipped.length > 0 && `，跳过 ${outcome.skipped.length} 条`}</p>
          {outcome.skipped.length > 0 && (
            <ul>
              {outcome.skipped.map((x, n) => (
                <li key={n}>{x.title}：{x.why}</li>
              ))}
            </ul>
          )}
        </div>
      )}
      {items === null ? (
        !error && <p role="status">加载中…</p>
      ) : items.length === 0 ? (
        <p className="empty">没有待处理的建议</p>
      ) : (
        <>
          <div className="actions">
            <label>
              来源
              <select value={source} onChange={(e) => setSource(e.target.value)}>
                <option value="">全部</option>
                {opts((i) => i.origin?.source).map((v) => (
                  <option key={v} value={v}>{v}</option>
                ))}
              </select>
            </label>
            <label>
              项目
              <select value={project} onChange={(e) => setProject(e.target.value)}>
                <option value="">全部</option>
                {opts((i) => i.project).map((v) => (
                  <option key={v} value={v}>{v}</option>
                ))}
              </select>
            </label>
            {confirming ? (
              <>
                <span>将对当前范围内 {recommended} 条带推荐的建议各自执行推荐处置；没有推荐的不动。</span>
                <button disabled={batching} onClick={batch}>确认按推荐处理</button>
                <button onClick={() => setConfirming(false)}>取消</button>
              </>
            ) : (
              <button disabled={batching || recommended === 0} onClick={() => setConfirming(true)}>按推荐处理</button>
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
