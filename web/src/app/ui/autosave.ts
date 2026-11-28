import { useEffect, useRef, useState, type FocusEvent, type FormEvent, type MouseEvent } from "react";
import type { Save } from "./feedback";

/**
 * 即改即存（界面设计 §6.1）：把返回的 on 铺在一块表单的容器上。勾选、单选、下拉当场存；文字输入停手 800ms 或离开输入框时存；
 * 分段按钮、标签的增删与标了 data-save 的按钮点了就存。存在渲染之后做，save 拿到的是最新的草稿；一次只存一份，
 * 存的时候又改了，存完再存一次（不并发写出版本冲突）。save 抛错（含本地校验）就是「没保存：原因」，草稿不动。
 * dirty 为 false 时不存（没改动）；不给 dirty 时每次改动都存。
 */
export function useAutoSave(save: () => Promise<unknown>, dirty?: boolean) {
  const [state, setState] = useState<Save>("idle");
  const [tick, setTick] = useState(0);
  const latest = useRef(save);
  latest.current = save;
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const typed = useRef(false);
  const busy = useRef(false);
  const again = useRef(false);
  const schedule = (ms: number) => {
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setTick((n) => n + 1), ms);
  };
  useEffect(() => () => clearTimeout(timer.current), []);
  useEffect(() => {
    if (!tick || dirty === false) return;
    if (busy.current) {
      again.current = true;
      return;
    }
    busy.current = true;
    typed.current = false;
    setState("saving");
    latest.current().then(() => setState("saved"), (e: unknown) => setState({ failed: e })).finally(() => {
      busy.current = false;
      if (again.current) {
        again.current = false;
        setTick((n) => n + 1);
      }
    });
  }, [tick]); // eslint-disable-line react-hooks/exhaustive-deps
  const text = (el: EventTarget) => el instanceof HTMLTextAreaElement || (el instanceof HTMLInputElement && !["checkbox", "radio"].includes(el.type));
  return {
    state,
    on: {
      onChange: (e: FormEvent) => { if (text(e.target)) { typed.current = true; schedule(800); } else schedule(0); },
      onBlur: (e: FocusEvent) => { if (typed.current && text(e.target)) schedule(0); },
      onClick: (e: MouseEvent) => { if ((e.target as HTMLElement).closest?.("button[aria-pressed], .chips button, button[data-save]")) schedule(0); },
    },
  };
}
