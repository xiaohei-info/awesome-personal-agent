import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { errorText, useT } from "../i18n";
import { Check, EmptyList, Spinner } from "./icons";
import { strings } from "./strings";

// 保存状态、骨架、空状态、长内容收起（规格 §4 第 7、8、9 件）。

export type Save = "idle" | "saving" | "saved" | { failed: unknown };

/** 控件旁一小块：保存中（转圈）→ 已保存（2 秒后淡出）→ 失败（常驻，写原因）。 */
export function SaveState({ state }: { state: Save }) {
  const t = useT(strings);
  const [fading, setFading] = useState(false);
  const [gone, setGone] = useState(false);
  useEffect(() => {
    setFading(false);
    setGone(false);
    if (state !== "saved") return;
    const a = setTimeout(() => setFading(true), 2000);
    const b = setTimeout(() => setGone(true), 2150);
    return () => { clearTimeout(a); clearTimeout(b); };
  }, [state]);
  if (state === "idle" || gone) return <span role="status" className="sr-only" />;
  if (state === "saving") return <span role="status" className="save-state" data-state="saving"><Spinner size="sm" spin />{t.saving}</span>;
  if (state === "saved") return <span role="status" className="save-state" data-state="saved" data-fading={fading || undefined}><Check size="sm" />{t.saved}</span>;
  return <span role="status" className="save-state" data-state="failed">{t.notSaved(errorText(state.failed))}</span>;
}

/** 第一次加载的占位：200ms 内不显示（快的请求不闪），之后按页面形状画几行并呼吸；读屏仍读到「加载中」。 */
export function Skeleton({ shape = "rows", n = 5, label }: { shape?: "rows" | "card" | "chart"; n?: number; label?: string }) {
  const t = useT(strings);
  return (
    <div className="skel-wrap" role="status" aria-label={label ?? t.loading}>
      <span className="sr-only">{label ?? t.loading}</span>
      {shape === "rows" && (
        <div className="skel-rows" aria-hidden="true">
          <span className="skel skel-bar" />
          {Array.from({ length: n }, (_, i) => <div key={i} className="skel-row"><span className="skel skel-dot" /><span className="skel" /><span className="skel skel-meta" /></div>)}
        </div>
      )}
      {shape === "card" && (
        <div className="panel skel-card" aria-hidden="true"><span className="skel" /><span className="skel" /><span className="skel" /><span className="skel" /></div>
      )}
      {shape === "chart" && <div className="panel" aria-hidden="true"><span className="skel skel-chart" /></div>}
    </div>
  );
}

/** 空状态：一句话加一个动作。filtered 区分"筛选后为空"和"真的为空"由调用方给文案与动作。 */
export function EmptyState({ title, note, action, boxed }: { title: string; note?: ReactNode; action?: ReactNode; boxed?: boolean }) {
  return (
    <div className={"empty-state" + (boxed ? " boxed" : "")}>
      <EmptyList size="lg" />
      <h2>{title}</h2>
      {note && <p>{note}</p>}
      {action}
    </div>
  );
}

/** 超过 lines 行收起，底部渐隐加「展开全文」；没超过就原样显示。 */
export function Clamp({ lines = 12, children }: { lines?: number; children: ReactNode }) {
  const t = useT(strings);
  const body = useRef<HTMLDivElement>(null);
  const [over, setOver] = useState(false);
  const [open, setOpen] = useState(false);
  const lh = 23; // 14px 正文 × 1.65
  useLayoutEffect(() => {
    const el = body.current;
    if (el) setOver(el.scrollHeight > lines * lh + lh);
  }, [children, lines]);
  const clamped = over && !open;
  return (
    <div className="clamp" data-clamped={clamped || undefined} style={{ ["--clamp-h" as string]: lines * lh + "px" }}>
      <div ref={body} className="clamp-body">{children}</div>
      {over && <button type="button" className="link" aria-expanded={open} onClick={() => setOpen(!open)}>{open ? t.collapse : t.expand}</button>}
    </div>
  );
}
