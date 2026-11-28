import { useEffect, useRef, useState, type ReactNode, type KeyboardEvent } from "react";
import { useT } from "../i18n";
import { strings } from "./strings";

// 确认（规格 §4 第 6 件）：行内版是一块灰底，一句后果加一个写明具体动作的按钮和取消；Esc 取消，确认中禁用。
// 不可逆的确认按钮用 .danger，可逆的用描边款。对话框版只给离开页面和不可逆的大范围操作用。
export function ConfirmInline({ consequence, action, onConfirm, onCancel, danger, label }: {
  consequence: ReactNode;
  action: string;
  onConfirm: () => unknown;
  onCancel: () => void;
  danger?: boolean;
  label?: string;
}) {
  const t = useT(strings);
  const [busy, setBusy] = useState(false);
  const btn = useRef<HTMLButtonElement>(null);
  useEffect(() => btn.current?.focus(), []);
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onCancel();
    }
  };
  return (
    <div className="confirm-inline" role="group" aria-label={label ?? action} onKeyDown={onKey}>
      <p>{consequence}</p>
      <span className="actions">
        <button ref={btn} type="button" className={danger ? "danger" : undefined} disabled={busy}
          onClick={async () => { setBusy(true); try { await onConfirm(); } finally { setBusy(false); } }}>
          {action}
        </button>
        <button type="button" disabled={busy} onClick={onCancel}>{t.cancel}</button>
      </span>
    </div>
  );
}

/** 页内对话框：模态，Esc 或点遮罩取消，焦点留在框内，关闭后回到打开前的位置。 */
export function ConfirmDialog({ title, children, action, onConfirm, onCancel, cancel, danger }: {
  title: string;
  children?: ReactNode;
  action: string;
  onConfirm: () => void;
  onCancel: () => void;
  cancel?: string;
  danger?: boolean;
}) {
  const t = useT(strings);
  const box = useRef<HTMLDivElement>(null);
  const back = useRef(document.activeElement as HTMLElement | null);
  useEffect(() => {
    box.current?.querySelector<HTMLElement>("footer button:last-child")?.focus();
    const prev = back.current;
    return () => prev?.focus?.();
  }, []);
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onCancel();
    } else if (e.key === "Tab") {
      const all = [...(box.current?.querySelectorAll<HTMLElement>("a[href], button:not(:disabled), input, select, textarea") ?? [])];
      const [first, last] = [all[0], all[all.length - 1]];
      if (e.shiftKey ? document.activeElement === first : document.activeElement === last) {
        e.preventDefault();
        (e.shiftKey ? last : first)?.focus();
      }
    }
  };
  return (
    <div className="dialog-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onCancel()}>
      <div ref={box} className="dialog" role="alertdialog" aria-modal="true" aria-label={title} onKeyDown={onKey}>
        <h2>{title}</h2>
        {children}
        <footer>
          <button type="button" onClick={onCancel}>{cancel ?? t.cancel}</button>
          <button type="button" className={danger ? "danger" : "primary"} onClick={onConfirm}>{action}</button>
        </footer>
      </div>
    </div>
  );
}
