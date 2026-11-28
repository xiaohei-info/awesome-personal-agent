import { useCallback, useEffect, useRef, useState } from "react";
import { EmptyState, ErrorBoundary, Skeleton } from "../../app/ui";
import { Link } from "react-router-dom";
import { Go, PageHead } from "../../app/page";
import { useSession } from "../../app/session";
import { refreshCounts, useAttention } from "../../app/attention";
import { RejectionNote, type Rejection } from "./RejectionNote";
import { locale, useT } from "../../app/i18n";
import { ErrorNote } from "../../app/errors";
import { useNames } from "../../app/names";
import { useLabel, Val } from "../../app/values";
import { strings } from "./strings";
import { useAskSay } from "../../app/notice";

// 等你决定（界面设计 §5.2）：两节——问题（卡片）与待确认（Agent 的启动请求、文档修改，各一行加「去确认」）。
// 形状对应 GET /v1/items 返回的 domain.Item（只取本页用到的字段）。排序之外的判定（web_only、权限、只认第一次回答）都在服务端；
// 回答始终是本人在 Web 的动作，不改变任何授权规则。待确认的行只是摘要加链接，确认仍在权威位置完成。
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
  project?: string;
  priority?: "low" | "normal" | "high" | "urgent";
  ask?: {
    question: string;
    /** 内核生成的问题的类别码与参数（§6.3）；没有 code 的是旧格式或 agent 自己写的问题，照原文显示。 */
    code?: string;
    params?: Record<string, string>;
    options?: AskOption[];
    recommend?: string;
    free_text?: boolean;
    deadline?: string;
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
  steps: { type: string; reason?: string; actor?: { type: string; principal: string; via: string } }[];
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

/** 谁在问、为什么问：取回放里最近一次 decision.requested（没有时取最近一次阶段变化，内核经 item.update 挂问题时是它），默认展开。
 *  内核问的、带类别码的：理由已在标题里说了人话，这里只写谁在问；内核问的、没有类别码的：是升级前的旧格式，照原文并标出。 */
function Asker({ id, code }: { id: string; code?: string }) {
  const t = useT(strings);
  const { api } = useSession();
  const label = useLabel();
  const credential = useNames("credential");
  const [step, setStep] = useState<WhyResp["steps"][number] | null>(null);
  useEffect(() => {
    let live = true;
    api.get<WhyResp>("/why/" + encodeURIComponent(id)).then(
      (r) => {
        const back = [...r.steps].reverse();
        live && setStep(back.find((s) => s.type === "decision.requested") ?? back.find((s) => s.type === "item.phase_changed") ?? { type: "", reason: "" });
      },
      () => live && setStep(null),
    );
    return () => { live = false; };
  }, [api, id]);
  if (!step) return null;
  const a = step.actor;
  const who = !a ? "" : a.type === "agent" ? credential(a.principal) : label("actorType", a.type);
  const kernel = a?.type === "system";
  const reason = kernel && code ? "" : step.reason || t.noReason;
  return (
    <p className="why">
      {[who && t.asks(who), reason].filter(Boolean).join(" · ")}
      {kernel && !code && <> <span className="badge plain" title={t.legacyWhy}>{t.legacy}</span></>}
    </p>
  );
}

/** 一张问题卡（等你决定页与事项详情共用）：问题、截止、是哪件事、谁在问为什么问、选项、别的想法；答完收成一行回执。 */
export function QuestionCard({ item, focused, onFocus, rejection, onAnswer, inItem }: {
  item: DecisionItem;
  focused?: boolean;
  onFocus?: () => void;
  rejection?: Rejection;
  onAnswer: (id: string, body: { choice: string } | { text: string }, said: string) => Promise<boolean>;
  inItem?: boolean;
}) {
  const t = useT(strings);
  const project = useNames("project");
  const askSay = useAskSay();
  const ask = item.ask!;
  const said = askSay(ask); // 内核问题按类别码说人话；没有类别码照原文
  // 只有一个选项、又不能写别的想法：不是决定。新问题在内核侧就构造不出来（§6.14 第 20 条），这里只会是升级前的旧卡，给一个明确的收尾。
  const single = !ask.free_text && (ask.options ?? []).length === 1;
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<string | null>(null);

  const answer = async (body: { choice: string } | { text: string }, said: string) => {
    setBusy(true);
    try {
      if (await onAnswer(item.id, body, said)) setDone(said);
    } finally {
      setBusy(false);
    }
  };

  if (done)
    return (
      <p role="status" className="panel receipt" aria-label={item.title}>
        {t.answered(done)}
        {!inItem && <> · <Link to={"/items/" + encodeURIComponent(item.id)}>{t.seeItem}</Link></>}
      </p>
    );

  return (
    <article aria-label={item.title} className={"decision" + (focused ? " focus" : "")} onFocus={onFocus}>
      <header>
        <h2>{said ?? ask.question}</h2>
        {ask.deadline && <Due deadline={ask.deadline} />}
      </header>
      {said && (
        <details className="sub">
          <summary>{t.details}</summary>
          <p>{ask.params?.detail ?? ask.question}</p>
        </details>
      )}
      <p className="meta">
        {inItem ? null : <Link to={"/items/" + encodeURIComponent(item.id)}>{item.title}</Link>}
        {item.project && <span className={inItem ? undefined : "dot"}>{project(item.project)}</span>}
        {item.priority && item.priority !== "normal" && <span className="dot"><Val k="priority" id={item.priority} /></span>}
      </p>
      <Asker id={item.id} code={ask.code} />
      <RejectionNote rejection={rejection} what="answer" item={item.id} />
      {single && <p className="sub">{t.ackWhy}</p>}
      {/* 选项一律同等样式：不替本人暗示选哪个 */}
      <div role="group" aria-label={t.options} className="options">
        {single && (
          <button disabled={busy} onClick={() => void answer({ choice: ask.options![0].id }, t.ack)}>{t.ack}</button>
        )}
        {!single && (ask.options ?? []).map((o, n) => (
          <button key={o.id} disabled={busy} onClick={() => void answer({ choice: o.id }, o.label)}>
            {n < 9 && <kbd aria-hidden="true">{n + 1}</kbd>}
            {o.label}
            {ask.recommend === o.id ? t.recommended : ""}
          </button>
        ))}
      </div>
      {ask.free_text && (
        <div className="free">
          <input aria-label={t.otherIdea} placeholder={t.otherIdeaPlaceholder} value={text} onChange={(e) => setText(e.target.value)} />
          <button className="primary" disabled={busy || !text.trim()} onClick={() => void answer({ text: text.trim() }, text.trim())}>
            {t.submitIdea}
          </button>
        </div>
      )}
    </article>
  );
}

const pending = (i: DecisionItem) => !!i.ask && !i.ask.resolution;

// 焦点在输入框里时不抢键；带修饰键的组合留给浏览器和跳转面板。
const typing = (t: EventTarget | null) => t instanceof HTMLElement && !!t.closest("input, textarea, select, [contenteditable='true']");

/** 回答一个问题：失败（含 409：已有人先答）时报原因并返回 false，成功后让导航上的数字重取。 */
export function useAnswer(onError: (e: unknown) => void) {
  const { api } = useSession();
  return async (id: string, body: { choice: string } | { text: string }) => {
    try {
      await api.post("/items/" + encodeURIComponent(id) + "/decide", body);
      return true;
    } catch (e) {
      onError(e);
      return false;
    } finally {
      refreshCounts();
    }
  };
}

export function DecisionsPage() {
  const { api } = useSession();
  const t = useT(strings);
  const attention = useAttention();
  const [items, setItems] = useState<DecisionItem[] | null>(null);
  const [rejections, setRejections] = useState<Record<string, Rejection>>({});
  const [error, setError] = useState<unknown>(null);
  const [cur, setCur] = useState(0);
  const [receipts, setReceipts] = useState<{ id: string; title: string; said: string }[]>([]);
  const list = useRef<HTMLDivElement>(null);
  const post = useAnswer(setError);
  // 答完的卡收成一行回执（本次停留期间保留）；列表随即重读，以服务端为准（含 409：已有人先答）。
  const answer = async (id: string, body: { choice: string } | { text: string }, said: string) => {
    setError(null);
    const ok = await post(id, body);
    if (ok) setReceipts((r) => [...r, { id, title: items?.find((i) => i.id === id)?.title ?? id, said }]);
    await load();
    return ok;
  };

  const load = useCallback(
    () =>
      api.get<{ items: DecisionItem[]; last_rejections?: Record<string, Rejection> }>("/items?phase=needs_input").then(
        (r) => {
          setRejections(r.last_rejections ?? {});
          setItems(sortDecisions(r.items.filter(pending)));
        },
        (e) => setError(e),
      ),
    [api],
  );

  useEffect(() => {
    load();
  }, [load]);

  const confirms = attention?.confirms ?? [];
  // 答完的卡在本次停留期间收成回执，所以计数取「还没答」的：与导航角标同口径（问题 + 待确认）。
  const n = (items?.length ?? 0) + confirms.length;
  const at = Math.min(cur, Math.max((items?.length ?? 0) - 1, 0));

  // 键盘：1–9 选当前卡的第 N 个选项，J/K 切换卡。按键只是点击对应按钮，答复逻辑不变。
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
        cards[at]?.querySelectorAll<HTMLButtonElement>(".options button")[Number(key) - 1]?.click();
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
            <span className="kbd-hint">
              {t.hint1}
              <kbd>J</kbd> <kbd>K</kbd>
              {t.hint2}
            </span>
          }
        >
          {t.countPre}
          <em>{n}</em>
          {t.countPost}
        </PageHead>
      )}
      {error != null && (
        <div>
          <ErrorNote error={error} reveal />
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
        error == null && <Skeleton shape="card" label={t.loading} />
      ) : n === 0 && !receipts.length ? (
        <EmptyState title={t.empty} />
      ) : (
        <>
          {receipts.length > 0 && (
            <ul className="rows" aria-label={t.receipts}>
              {receipts.map((r) => (
                <li key={r.id} role="status">{r.title}{t.colon}{t.answered(r.said)} · <Link to={"/items/" + encodeURIComponent(r.id)}>{t.seeItem}</Link></li>
              ))}
            </ul>
          )}
          {items.length > 0 && (
            <section aria-label={t.questions}>
              <div ref={list}>
                {items.map((i, k) => (
                  <ErrorBoundary key={i.id}><QuestionCard item={i} focused={k === at} onFocus={() => setCur(k)} rejection={rejections[i.id]} onAnswer={answer} /></ErrorBoundary>
                ))}
              </div>
            </section>
          )}
          {confirms.length > 0 && (
            <section aria-label={t.confirms} className="panel">
              <h2>{t.confirms}</h2>
              <ul className="rows">
                {confirms.map((c) => (
                  <li key={c.kind + c.id} className="act-row">
                    <div><span>{c.kind === "launch" ? t.launchAsk(c.name) : t.revisionAsk(c.name)}</span></div>
                    <Go className="btn" to={c.to}>{t.goConfirm}</Go>
                  </li>
                ))}
              </ul>
            </section>
          )}
        </>
      )}
    </main>
  );
}
