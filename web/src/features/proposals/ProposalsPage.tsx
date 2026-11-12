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
}

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

function Card({ item, rejection, onAct }: { item: Proposal; rejection?: Rejection; onAct: (id: string, action: "accept" | "dismiss", body: object) => Promise<void> }) {
  const [dismissing, setDismissing] = useState(false);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);

  const act = async (action: "accept" | "dismiss", body: object) => {
    setBusy(true);
    try {
      await onAct(item.id, action, body);
    } finally {
      setBusy(false);
    }
  };
  const meta = [item.project, item.origin && `来自 ${item.origin.actor || item.origin.source}`, item.priority && item.priority !== "normal" ? item.priority : ""].filter(Boolean).join(" · ");

  return (
    <article aria-label={item.title}>
      <h3>{item.title}</h3>
      {meta && <p className="meta">{meta}</p>}
      {item.summary && <p>{item.summary}</p>}
      <RejectionNote rejection={rejection} what="处理" />
      <div className="actions">
        <button disabled={busy} onClick={() => act("accept", {})}>接受</button>
        {dismissing ? (
          <>
            <label>
              丢弃原因
              <input value={reason} onChange={(e) => setReason(e.target.value)} />
            </label>
            <button disabled={busy || !reason.trim()} onClick={() => act("dismiss", { reason: reason.trim() })}>确认丢弃</button>
          </>
        ) : (
          <button disabled={busy} onClick={() => setDismissing(true)}>丢弃</button>
        )}
      </div>
    </article>
  );
}

export function ProposalsPage() {
  const { api } = useSession();
  const [items, setItems] = useState<Proposal[] | null>(null);
  const [rejections, setRejections] = useState<Record<string, Rejection>>({});
  const [error, setError] = useState<string | null>(null);

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

  const act = async (id: string, action: "accept" | "dismiss", body: object) => {
    setError(null);
    try {
      await api.post("/items/" + encodeURIComponent(id) + "/" + action, body);
    } catch (e) {
      setError(msg(e)); // 含 409：已有人先处理，刷新后以服务端为准
    }
    await load();
  };

  return (
    <main>
      <PageHead lede="agent 从来源里读到、觉得值得做的事。接受后进入事项，丢弃要写原因。">建议流</PageHead>
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
      {items === null ? (
        !error && <p role="status">加载中…</p>
      ) : items.length === 0 ? (
        <p className="empty">没有待处理的建议</p>
      ) : (
        items.map((i) => <Card key={i.id} item={i} rejection={rejections[i.id]} onAct={act} />)
      )}
    </main>
  );
}
