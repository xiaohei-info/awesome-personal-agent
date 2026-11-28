import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { useT } from "../i18n";
import { strings } from "./strings";
import { typing } from "./toast";
import { rowEl } from "./Peek";

// 列表键盘（规格 §4 第 4 件，拍板项 4：只做列表键和 ? 说明）：J/K 与上下键移动光标行，Enter 打开，空格速览，E 完成，X 勾选，Esc。
// 输入框里不抢键；焦点在按钮或链接上时 Enter 与空格交还给它；鼠标一动就交给鼠标（光标描边消失）。按住不放时 Enter、空格、E、X 不连发。
export interface ListKeys {
  ids: string[];
  onOpen?: (id: string) => void;
  onPeek?: (id: string) => void;
  onDone?: (id: string) => void;
  onToggle?: (id: string) => void;
  onEscape?: () => boolean | void;
  /** 光标移动时（速览开着就跟着换件）。 */
  onMove?: (id: string) => void;
}

export function useListKeys(o: ListKeys): string | null {
  const [cursor, setCursor] = useState<string | null>(null);
  const ref = useRef(o);
  ref.current = o;
  const cur = useRef(cursor);
  cur.current = cursor;
  useEffect(() => {
    const move = (d: number) => {
      const { ids, onMove } = ref.current;
      if (!ids.length) return;
      const at = cur.current ? ids.indexOf(cur.current) : -1;
      const next = ids[at < 0 ? (d > 0 ? 0 : ids.length - 1) : Math.min(Math.max(at + d, 0), ids.length - 1)];
      setCursor(next);
      rowEl(next)?.scrollIntoView?.({ block: "nearest" });
      onMove?.(next);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || typing(e.target)) return;
      if ((e.target as HTMLElement | null)?.closest?.('[role="dialog"], [role="alertdialog"], [role="menu"]')) return;
      const k = e.key.length === 1 ? e.key.toLowerCase() : e.key;
      const onControl = e.target instanceof HTMLElement && !!e.target.closest("button, a[href], summary");
      const id = cur.current && ref.current.ids.includes(cur.current) ? cur.current : null;
      const fire = (f?: (id: string) => void) => {
        if (!f || !id || e.repeat) return;
        e.preventDefault();
        f(id);
      };
      if (k === "j" || k === "ArrowDown") { e.preventDefault(); move(1); }
      else if (k === "k" || k === "ArrowUp") { e.preventDefault(); move(-1); }
      else if (k === "Enter" && !onControl) fire(ref.current.onOpen);
      else if (k === " " && !onControl) fire(ref.current.onPeek);
      else if (k === "e") fire(ref.current.onDone);
      else if (k === "x") fire(ref.current.onToggle);
      else if (k === "Escape" && ref.current.onEscape?.()) e.preventDefault();
    };
    const onPointer = () => cur.current && setCursor(null);
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousemove", onPointer);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousemove", onPointer);
    };
  }, []);
  return cursor && o.ids.includes(cursor) ? cursor : null;
}

// ---- 快捷键说明：页面登记自己能用的键，外壳按 ? 打开 ----
export type KeyName = "move" | "open" | "peek" | "done" | "undo" | "escape" | "help" | "palette";
export interface KeyDef { keys: string[]; what: KeyName }

let pageKeys: KeyDef[] = [];
const subs = new Set<() => void>();

/** 页面登记当前能用的键（离开页面时撤掉）。 */
export function usePageKeys(keys: KeyDef[]) {
  const sig = JSON.stringify(keys);
  useEffect(() => {
    pageKeys = JSON.parse(sig);
    subs.forEach((f) => f());
    return () => {
      pageKeys = [];
      subs.forEach((f) => f());
    };
  }, [sig]);
}

export function usePageKeyList(): KeyDef[] {
  return useSyncExternalStore((f) => (subs.add(f), () => subs.delete(f)), () => pageKeys);
}

/** 列表页通用的那几个键。 */
export const LIST_KEYS: KeyDef[] = [
  { keys: ["J", "K"], what: "move" },
  { keys: ["↵"], what: "open" },
  { keys: ["Space"], what: "peek" },
  { keys: ["E"], what: "done" },
  { keys: ["Z"], what: "undo" },
  { keys: ["Esc"], what: "escape" },
];

export function KeyList({ keys }: { keys: KeyDef[] }) {
  const t = useT(strings);
  if (!keys.length) return <p>{t.keysNone}</p>;
  return (
    <dl className="keys">
      {keys.map((k) => (
        <div key={k.what}>
          <dt>{k.keys.map((x) => <kbd key={x}>{x === "Space" ? t.spaceKey : x}</kbd>)}</dt>
          <dd>{t.keys[k.what]}</dd>
        </div>
      ))}
    </dl>
  );
}

/** ? 打开的快捷键说明：只列当前页登记的键，加上全站都能用的两个。 */
export function ShortcutSheet({ onClose }: { onClose: () => void }) {
  const t = useT(strings);
  const keys = usePageKeyList();
  const btn = useRef<HTMLButtonElement>(null);
  const back = useRef(document.activeElement as HTMLElement | null);
  useEffect(() => {
    btn.current?.focus();
    const prev = back.current;
    return () => prev?.focus?.();
  }, []);
  const mod = /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘" : "Ctrl";
  return (
    <div className="dialog-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={t.keysTitle}
        onKeyDown={(e) => { if (e.key === "Escape" || e.key === "Tab") { e.preventDefault(); if (e.key === "Escape") onClose(); } }}>
        <h2>{t.keysTitle}</h2>
        <KeyList keys={[...keys, { keys: [mod, "K"], what: "palette" }, { keys: ["?"], what: "help" }]} />
        <footer><button ref={btn} type="button" onClick={onClose}>{t.close}</button></footer>
      </div>
    </div>
  );
}
