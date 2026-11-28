import { useCallback, useEffect, useRef, useState, type ReactNode, type KeyboardEvent } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useT } from "../i18n";
import { Close, Down, External, Up } from "./icons";
import { strings } from "./strings";
import { ErrorBoundary } from "./ErrorBoundary";

// 侧边速览（规格 §4 第 3 件）：地址加 ?peek=<id>，可收藏，旧地址不受影响；列表把当前顺序交给它，它负责上一件、下一件、
// Esc 与「在新页打开」。打开时焦点进面板，关闭后回到原来的行。视口窄于 1100px 时没有速览，调用方照旧整页打开。
const WIDE = "(min-width: 1100px)";

export function useWide() {
  const [wide, setWide] = useState(() => typeof window !== "undefined" && !!window.matchMedia?.(WIDE).matches);
  useEffect(() => {
    const mq = window.matchMedia?.(WIDE);
    if (!mq) return;
    const on = () => setWide(mq.matches);
    mq.addEventListener?.("change", on);
    return () => mq.removeEventListener?.("change", on);
  }, []);
  return wide;
}

export function usePeek() {
  const [params, setParams] = useSearchParams();
  const id = params.get("peek");
  const enabled = useWide();
  const open = useCallback((next: string) => setParams((p) => { const q = new URLSearchParams(p); q.set("peek", next); return q; }, { replace: true }), [setParams]);
  const close = useCallback(() => setParams((p) => { const q = new URLSearchParams(p); q.delete("peek"); return q; }, { replace: true }), [setParams]);
  return { id: enabled ? id : null, enabled, open, close };
}

/** 列表行的 DOM：data-item 标记同一件事，用来把焦点还回去。 */
export const rowEl = (id: string) => document.querySelector<HTMLElement>(`[data-item="${CSS.escape(id)}"]`);

export function PeekPanel({ id, ids, href, title, onOpen, onClose, children }: {
  id: string;
  ids: string[];
  href: string;
  title: string;
  onOpen: (id: string) => void;
  onClose: () => void;
  children: ReactNode;
}) {
  const t = useT(strings);
  const box = useRef<HTMLElement>(null);
  const at = ids.indexOf(id);
  const go = (d: number) => {
    const next = ids[at + d];
    if (next) onOpen(next);
  };
  useEffect(() => {
    box.current?.focus({ preventScroll: true });
  }, []);
  const last = useRef(id);
  last.current = id;
  // 关闭（卸载）时焦点回到最后速览的那一行；J/K 换件不动焦点。
  useEffect(() => () => {
    const row = rowEl(last.current);
    (row?.querySelector<HTMLElement>("a") ?? row)?.focus({ preventScroll: true });
  }, []);
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape" && !e.defaultPrevented) {
      e.preventDefault();
      onClose();
    }
  };
  return (
    <aside ref={box} className="peek" tabIndex={-1} aria-label={t.peekOf(title)} onKeyDown={onKey}>
      <header>
        <span className="grow">{at >= 0 && t.position(at + 1, ids.length)}</span>
        <button type="button" aria-label={t.prev} data-tip={t.prev + " K"} disabled={at <= 0} onClick={() => go(-1)}><Up /></button>
        <button type="button" aria-label={t.next} data-tip={t.next + " J"} disabled={at < 0 || at >= ids.length - 1} onClick={() => go(1)}><Down /></button>
        <Link to={href} aria-label={t.openFull} data-tip={t.openFull}><External /></Link>
        <button type="button" aria-label={t.close} data-tip={t.close + " Esc"} onClick={onClose}><Close /></button>
      </header>
      <div className="peek-body"><ErrorBoundary resetKey={id}>{children}</ErrorBoundary></div>
    </aside>
  );
}
