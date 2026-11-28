import { useEffect, useRef, useSyncExternalStore } from "react";
import { errorText, useT } from "../i18n";
import { Check, Close } from "./icons";
import { strings } from "./strings";

// 提示条（规格 §4 第 1 件）：全局唯一，外壳挂一次 <Toaster />。
// toast(文案) 4 秒；undoToast(文案, 撤销) 10 秒、一次只留最新一条可撤销的、Z 键撤销；toastError(文案或错误) 常驻到手动关闭。
// 悬停时暂停计时；最多叠 3 条。撤销是调用方给的写入（立即写入、撤销时改回，拍板项 3），这里只负责调用与收起。
interface Toast {
  id: number;
  text: string;
  undo?: () => unknown;
  error?: boolean;
  left: number; // 剩余毫秒；Infinity 为常驻
  leaving?: boolean;
}

const MAX = 3;
const LEAVE_MS = 100; // 与 --dur-micro 一致
let list: Toast[] = [];
let seq = 0;
let paused = false;
let lastTick = 0;
let timer: ReturnType<typeof setInterval> | null = null;
const subs = new Set<() => void>();
const emit = () => subs.forEach((f) => f());
const set = (next: Toast[]) => {
  list = next;
  emit();
  schedule();
};

function schedule() {
  const live = list.some((t) => !t.leaving && Number.isFinite(t.left));
  if (live && !timer) {
    lastTick = Date.now();
    timer = setInterval(tick, 250);
  } else if (!live && timer) {
    clearInterval(timer);
    timer = null;
  }
}

function tick() {
  const now = Date.now();
  const dt = now - lastTick;
  lastTick = now;
  if (paused) return;
  const expired = list.filter((t) => !t.leaving && t.left - dt <= 0).map((t) => t.id);
  list = list.map((t) => (t.leaving ? t : { ...t, left: t.left - dt }));
  if (expired.length) expired.forEach(dismiss);
  else emit();
}

function push(t: Omit<Toast, "id">): number {
  const id = ++seq;
  // 新的可撤销提示顶掉旧的可撤销提示：旧动作仍可在对应页面找回，但 Z 只对最新的一条。
  const keep = t.undo ? list.filter((x) => !x.undo) : list;
  set([...keep, { ...t, id }].slice(-MAX));
  return id;
}

export function toast(text: string): number {
  return push({ text, left: 4000 });
}

export function undoToast(text: string, undo: () => unknown, ms = 10_000): number {
  return push({ text, undo, left: ms });
}

export function toastError(e: unknown): number {
  return push({ text: typeof e === "string" ? e : errorText(e), error: true, left: Infinity });
}

export function dismiss(id: number) {
  if (!list.some((t) => t.id === id && !t.leaving)) return;
  set(list.map((t) => (t.id === id ? { ...t, leaving: true } : t)));
  setTimeout(() => set(list.filter((t) => t.id !== id)), LEAVE_MS);
}

/** 测试用：清空提示与计时。 */
export function resetToasts() {
  list = [];
  paused = false;
  if (timer) clearInterval(timer);
  timer = null;
  emit();
}

async function runUndo(t: Toast) {
  dismiss(t.id);
  await t.undo?.();
}

/** 按下 Z 撤销最新一条可撤销的提示；在输入框里不抢键。返回是否有可撤销的。 */
export function undoLatest(): boolean {
  const t = [...list].reverse().find((x) => x.undo && !x.leaving);
  if (!t) return false;
  void runUndo(t);
  return true;
}

export const typing = (el: EventTarget | null) =>
  el instanceof HTMLElement && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName));

export function Toaster() {
  const t = useT(strings);
  const items = useSyncExternalStore((f) => (subs.add(f), () => subs.delete(f)), () => list);
  const box = useRef<HTMLOListElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "z" && e.key !== "Z") return;
      if (e.metaKey || e.ctrlKey || e.altKey || e.repeat || typing(e.target)) return;
      if (undoLatest()) e.preventDefault();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);
  return (
    <ol ref={box} className="toaster" aria-live="polite" aria-label={t.notices}
      onMouseEnter={() => { paused = true; }} onMouseLeave={() => { paused = false; }}>
      {items.map((x) => (
        <li key={x.id} className="toast" role={x.error ? "alert" : "status"} data-leaving={x.leaving || undefined}>
          {x.error ? <span className="dot bad" aria-hidden="true" /> : <Check size="sm" />}
          <span className="toast-text">{x.text}</span>
          {x.undo && (
            <button type="button" className="toast-act" onClick={() => void runUndo(x)}>
              {t.undo} <kbd aria-hidden="true">Z</kbd>
            </button>
          )}
          <button type="button" className="toast-close" aria-label={t.dismiss} data-tip={t.close} onClick={() => dismiss(x.id)}>
            <Close size="sm" />
          </button>
        </li>
      ))}
    </ol>
  );
}
