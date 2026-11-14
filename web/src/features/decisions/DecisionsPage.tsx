import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, type ApiClient } from "../../app/api";
import { COUNTS_EVENT, PageHead } from "../../app/page";
import { useSession } from "../../app/session";
import { ScreeningBadge } from "../decision-screening/ScreeningBadge";
import { RejectionNote, type Rejection } from "./RejectionNote";
import { errorText as msg, locale, useT } from "../../app/i18n";
import { strings } from "./strings";

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


interface WhyResp {
  steps: { type: string; reason?: string }[];
}

const DAY_MS = 86_400_000;
const SOON_MS = 3 * DAY_MS; // 三天内截止（或已过）用朱砂提示

function Due({ deadline }: { deadline: string }) {
  const t = useT(strings);
  const at = new Date(deadline);
  const left = at.getTime() - Date.now();
  const short = at.toLocaleString(locale(), { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
  return (
    <span className={"due" + (left < SOON_MS ? " soon" : "")} title={at.toLocaleString(locale())}>
      {t.due(short)} · {left < 0 ? t.overdue : t.daysLeft(Math.ceil(left / DAY_MS))}
    </span>
  );
}

function Card({ item, focused, onFocus, rejection, onAnswer }: { item: DecisionItem; focused: boolean; onFocus: () => void; rejection?: Rejection; onAnswer: (id: string, body: { choice: string } | { text: string }) => Promise<void> }) {
  const { api } = useSession();
  const t = useT(strings);
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

  const toggleWhy = () =>
    why !== null
      ? setWhy(null)
      : api.get<WhyResp>("/why/" + encodeURIComponent(item.id)).then(
          (r) => {
            const step = [...r.steps].reverse().find((s) => s.type === "decision.requested");
            setWhy(step?.reason || t.noReason);
            setWhyErr(null);
          },
          (e) => setWhyErr(msg(e)),
        );

  return (
    <article aria-label={item.title} className={"decision" + (focused ? " focus" : "")} onFocus={onFocus}>
      <header>
        <h2>{ask.question}</h2>
        {ask.deadline && <Due deadline={ask.deadline} />}
      </header>
      <p className="meta">
        <span>{item.title}</span>
        {item.priority && item.priority !== "normal" && <span className="dot">{item.priority}</span>}
        {ask.risk_class === "web_only" && <span className="badge bad plain">{t.webOnly}</span>}
        <ScreeningBadge pending={ask.screening_pending} />
      </p>
      <RejectionNote rejection={rejection} what="answer" />
      {/* 选项一律同等样式：不替本人暗示选哪个 */}
      <div role="group" aria-label={t.options} className="options">
        {(ask.options ?? []).map((o, n) => (
          <button key={o.id} disabled={busy} onClick={() => answer({ choice: o.id })}>
            {n < 9 && <kbd aria-hidden="true">{n + 1}</kbd>}
            {o.label}
            {ask.recommend === o.id ? t.recommended : ""}
          </button>
        ))}
      </div>
      {ask.free_text && (
        <div className="free">
          <input aria-label={t.otherIdea} placeholder={t.otherIdeaPlaceholder} value={text} onChange={(e) => setText(e.target.value)} />
          <button className="primary" disabled={busy || !text.trim()} onClick={() => answer({ text: text.trim() })}>
            {t.submitIdea}
          </button>
        </div>
      )}
      <footer>
        <button className="quiet why-toggle" aria-expanded={why !== null} onClick={toggleWhy}>
          {t.why} <kbd aria-hidden="true">?</kbd>
        </button>
      </footer>
      {why && <p className="why">{why}</p>}
      {whyErr && <p role="alert">{whyErr}</p>}
    </article>
  );
}

const pending = (i: DecisionItem) => !!i.ask && !i.ask.resolution;

/** 导航上的待决数：与本页同一个接口、同一个过滤。 */
export const countDecisions = (api: ApiClient) =>
  api.get<{ items: DecisionItem[] }>("/items?phase=needs_input").then((r) => r.items.filter(pending).length);

// 焦点在输入框里时不抢键；带修饰键的组合留给浏览器和跳转面板。
const typing = (t: EventTarget | null) => t instanceof HTMLElement && !!t.closest("input, textarea, select, [contenteditable='true']");

export function DecisionsPage() {
  const { api } = useSession();
  const t = useT(strings);
  const [items, setItems] = useState<DecisionItem[] | null>(null);
  const [rejections, setRejections] = useState<Record<string, Rejection>>({});
  const [error, setError] = useState<string | null>(null);
  const [cur, setCur] = useState(0);
  const list = useRef<HTMLDivElement>(null);

  const load = useCallback(
    () =>
      api.get<{ items: DecisionItem[]; last_rejections?: Record<string, Rejection> }>("/items?phase=needs_input").then(
        (r) => {
          setRejections(r.last_rejections ?? {});
          setItems(sortDecisions(r.items.filter(pending)));
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
    window.dispatchEvent(new Event(COUNTS_EVENT));
  };

  const n = items?.length ?? 0;
  const at = Math.min(cur, Math.max(n - 1, 0));

  // 键盘：1–9 选当前卡的第 N 个选项，J/K 切换卡，? 展开「为什么在这」。按键只是点击对应按钮，答复逻辑不变。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented || typing(e.target)) return;
      const cards = list.current?.querySelectorAll<HTMLElement>("article.decision");
      if (!cards?.length) return;
      const key = e.key.toLowerCase();
      if (key === "j" || key === "k") {
        const next = Math.max(0, Math.min(cards.length - 1, at + (key === "j" ? 1 : -1)));
        setCur(next);
        cards[next].scrollIntoView?.({ block: "nearest", behavior: "smooth" });
      } else if (/^[1-9]$/.test(key)) {
        cards[at].querySelectorAll<HTMLButtonElement>(".options button")[Number(key) - 1]?.click();
      } else if (key === "?") {
        cards[at].querySelector<HTMLButtonElement>(".why-toggle")?.click();
      } else return;
      e.preventDefault();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [at]);

  return (
    <main>
      {items === null || n === 0 ? (
        <PageHead>{items === null ? t.title : t.none}</PageHead>
      ) : (
        <PageHead
          lede={
            <>
              {t.waiting}
              <span className="kbd-hint">
                {t.hint1}
                <kbd>J</kbd> <kbd>K</kbd>
                {t.hint2}
                <kbd>?</kbd>
                {t.hint3}
              </span>
            </>
          }
        >
          {t.countPre}
          <em>{n}</em>
          {t.countPost}
        </PageHead>
      )}
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
      {items === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : n === 0 ? (
        <p className="empty">{t.empty}</p>
      ) : (
        <div ref={list}>
          {items.map((i, k) => (
            <Card key={i.id} item={i} focused={k === at} onFocus={() => setCur(k)} rejection={rejections[i.id]} onAnswer={answer} />
          ))}
        </div>
      )}
    </main>
  );
}
