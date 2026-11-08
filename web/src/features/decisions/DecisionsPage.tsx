import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { ScreeningBadge } from "../decision-screening/ScreeningBadge";
import { RejectionNote, type Rejection } from "./RejectionNote";

// 形状对应 GET /v1/items 返回的 domain.Item（只取本页用到的字段）。排序之外的判定（web_only、权限、只认第一次回答）都在服务端。
export interface AskOption {
  id: string;
  label: string;
}
export interface DecisionItem {
  id: string;
  version: number;
  phase: string;
  kind: string;
  title: string;
  priority?: "low" | "normal" | "high" | "urgent";
  ask?: {
    question: string;
    options?: AskOption[];
    recommend?: string;
    free_text?: boolean;
    deadline?: string;
    risk_class?: string;
    screening_pending?: boolean;
    resolution?: unknown;
  };
}

const RANK = { urgent: 0, high: 1, normal: 2, low: 3 } as const;

/** 截止早的在前（无截止最后），同截止按优先级。 */
export function sortDecisions(items: DecisionItem[]): DecisionItem[] {
  const due = (i: DecisionItem) => (i.ask?.deadline ? Date.parse(i.ask.deadline) : Infinity);
  const rank = (i: DecisionItem) => RANK[i.priority ?? "normal"] ?? RANK.normal;
  return [...items].sort((a, b) => due(a) - due(b) || rank(a) - rank(b));
}

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

interface WhyResp {
  steps: { type: string; reason?: string }[];
}

function Card({ item, rejection, onAnswer }: { item: DecisionItem; rejection?: Rejection; onAnswer: (id: string, body: { choice: string } | { text: string }) => Promise<void> }) {
  const { api } = useSession();
  const ask = item.ask!;
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [why, setWhy] = useState<string | null>(null);
  const [whyErr, setWhyErr] = useState<string | null>(null);

  const answer = async (body: { choice: string } | { text: string }) => {
    setBusy(true);
    try {
      await onAnswer(item.id, body);
    } finally {
      setBusy(false);
    }
  };

  const loadWhy = () =>
    api.get<WhyResp>("/why/" + encodeURIComponent(item.id)).then(
      (r) => {
        const step = [...r.steps].reverse().find((s) => s.type === "decision.requested");
        setWhy(step?.reason || "没有记录原因");
        setWhyErr(null);
      },
      (e) => setWhyErr(msg(e)),
    );

  return (
    <article aria-label={item.title}>
      <h3>{ask.question}</h3>
      <p>
        {item.title}
        {item.priority && item.priority !== "normal" ? ` · ${item.priority}` : ""}
        {ask.risk_class === "web_only" && <strong> 仅限 Web 确认</strong>}
        <ScreeningBadge pending={ask.screening_pending} />
      </p>
      <RejectionNote rejection={rejection} what="代答" />
      {ask.deadline && <p>截止：{new Date(ask.deadline).toLocaleString()}</p>}
      <div role="group" aria-label="选项">
        {(ask.options ?? []).map((o) => (
          <button key={o.id} disabled={busy} onClick={() => answer({ choice: o.id })}>
            {o.label}
            {ask.recommend === o.id ? "（推荐）" : ""}
          </button>
        ))}
      </div>
      {ask.free_text && (
        <div>
          <label>
            别的想法
            <input value={text} onChange={(e) => setText(e.target.value)} />
          </label>
          <button disabled={busy || !text.trim()} onClick={() => answer({ text: text.trim() })}>
            提交想法
          </button>
        </div>
      )}
      <button onClick={loadWhy}>为什么在这</button>
      {why && <p>{why}</p>}
      {whyErr && <p role="alert">{whyErr}</p>}
    </article>
  );
}

export function DecisionsPage() {
  const { api } = useSession();
  const [items, setItems] = useState<DecisionItem[] | null>(null);
  const [rejections, setRejections] = useState<Record<string, Rejection>>({});
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    () =>
      api.get<{ items: DecisionItem[]; last_rejections?: Record<string, Rejection> }>("/items?phase=needs_input").then(
        (r) => {
          setRejections(r.last_rejections ?? {});
          setItems(sortDecisions(r.items.filter((i) => i.ask && !i.ask.resolution)));
        },
        (e) => setError(msg(e)),
      ),
    [api],
  );

  useEffect(() => {
    load();
  }, [load]);

  const answer = async (id: string, body: { choice: string } | { text: string }) => {
    setError(null);
    try {
      await api.post("/items/" + encodeURIComponent(id) + "/decide", body);
    } catch (e) {
      setError(msg(e)); // 含 409：已有人先答，刷新后以服务端为准
    }
    await load();
  };

  return (
    <main>
      <h2>决策待办</h2>
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
        !error && <p>加载中…</p>
      ) : items.length === 0 ? (
        <p>没有待办决策</p>
      ) : (
        items.map((i) => <Card key={i.id} item={i} rejection={rejections[i.id]} onAnswer={answer} />)
      )}
    </main>
  );
}
