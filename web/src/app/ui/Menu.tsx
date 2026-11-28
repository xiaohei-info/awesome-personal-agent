import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

// 弹出菜单与弹出卡（规格 §4 第 5 件）：上下、Home、End 移动，Esc 关闭并回到触发按钮，点外面或 Tab 关闭。
// 取代 RowActions 与建议页各自的一份菜单键盘代码。容器同时带 row-more-menu：styles.css 的通用按钮样式按它排除菜单项。
export interface MenuItem {
  label: ReactNode;
  onSelect: () => void;
  disabled?: boolean;
  /** 只给不可逆的项（永久删除等）；可逆的「删除…」不用朱砂。 */
  danger?: boolean;
}

function useDismiss(open: boolean, wrap: React.RefObject<HTMLElement>, close: () => void) {
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) close();
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open, wrap, close]);
}

export function Menu({ trigger, label, items, className }: { trigger: ReactNode; label: string; items: MenuItem[]; className?: string }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const wrap = useRef<HTMLSpanElement>(null);
  const btn = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const edge = useRef<"first" | "last">("first");
  const close = useRef(() => setOpen(false)).current;
  useDismiss(open, wrap, close);
  const buttons = () => [...(menu.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)') ?? [])];
  useEffect(() => {
    if (!open) return;
    const bs = buttons();
    (edge.current === "last" ? bs[bs.length - 1] : bs[0])?.focus();
  }, [open]);
  const show = (e: "first" | "last") => {
    edge.current = e;
    setOpen(true);
  };
  const onTrigger = (e: KeyboardEvent) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      show(e.key === "ArrowDown" ? "first" : "last");
    } else if (e.key === "Escape" && open) {
      e.preventDefault();
      setOpen(false);
    }
  };
  const onMenu = (e: KeyboardEvent) => {
    const bs = buttons();
    const at = bs.indexOf(document.activeElement as HTMLButtonElement);
    const go = (i: number) => bs[(i + bs.length) % bs.length]?.focus();
    if (e.key === "Escape") {
      e.preventDefault();
      setOpen(false);
      btn.current?.focus();
    } else if (e.key === "ArrowDown") { e.preventDefault(); go(at + 1); }
    else if (e.key === "ArrowUp") { e.preventDefault(); go(at - 1); }
    else if (e.key === "Home") { e.preventDefault(); go(0); }
    else if (e.key === "End") { e.preventDefault(); go(bs.length - 1); }
    else if (e.key === "Tab") setOpen(false);
  };
  return (
    <span className={["row-more", className].filter(Boolean).join(" ")} ref={wrap}>
      <button ref={btn} type="button" aria-haspopup="menu" aria-expanded={open} aria-controls={open ? id : undefined}
        onClick={() => (open ? setOpen(false) : show("first"))} onKeyDown={onTrigger}>
        {trigger}
      </button>
      {open && (
        <div id={id} ref={menu} className="menu-pop row-more-menu" role="menu" aria-label={label} onKeyDown={onMenu}>
          {items.map((it, i) => (
            <button key={i} type="button" role="menuitem" className={it.danger ? "danger" : undefined} disabled={it.disabled}
              onClick={() => { setOpen(false); it.onSelect(); }}>
              {it.label}
            </button>
          ))}
        </div>
      )}
    </span>
  );
}

/** 弹出卡：点触发按钮开合，Esc 或点外面关闭，关闭后焦点回到触发按钮。 */
export function Popover({ trigger, label, children }: { trigger: ReactNode; label: string; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLSpanElement>(null);
  const btn = useRef<HTMLButtonElement>(null);
  const close = useRef(() => setOpen(false)).current;
  useDismiss(open, wrap, close);
  return (
    <span className="row-more" ref={wrap} onKeyDown={(e) => {
      if (e.key === "Escape" && open) { e.preventDefault(); setOpen(false); btn.current?.focus(); }
    }}>
      <button ref={btn} type="button" aria-expanded={open} onClick={() => setOpen(!open)}>{trigger}</button>
      {open && <div className="popover" role="dialog" aria-label={label}>{children}</div>}
    </span>
  );
}
