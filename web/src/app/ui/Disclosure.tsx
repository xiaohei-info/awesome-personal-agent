import { useState, type ReactNode } from "react";
import { Chevron } from "./icons";

// 折叠（规格 §4 第 2 件）：仍是原生 <details>，所以锚点落点（useHashLanding 展开所在折叠区）与按名字找元素的测试不受影响；
// 加箭头与高度过渡（样式在 ui.css），给 persistKey 时把开合记在本标签页的 sessionStorage 里，回来还是原样。
const KEY = "apa.disc.";

function stored(key: string | undefined, fallback: boolean): boolean {
  if (!key) return fallback;
  const v = sessionStorage.getItem(KEY + key);
  return v == null ? fallback : v === "1";
}

export function Disclosure({ summary, children, persistKey, defaultOpen = false, head, className, ...rest }: {
  summary: ReactNode;
  children: ReactNode;
  persistKey?: string;
  defaultOpen?: boolean;
  /** 作分组头用：吸顶灰底的 .group-head。 */
  head?: boolean;
  className?: string;
  id?: string;
  "aria-label"?: string;
}) {
  const [open, setOpen] = useState(() => stored(persistKey, defaultOpen));
  return (
    <details {...rest} className={["disc", className].filter(Boolean).join(" ")} open={open}
      onToggle={(e) => {
        const now = (e.currentTarget as HTMLDetailsElement).open;
        if (now === open) return;
        setOpen(now);
        if (persistKey) sessionStorage.setItem(KEY + persistKey, now ? "1" : "0");
      }}>
      <summary className={head ? "group-head" : undefined}>
        <Chevron />
        {summary}
      </summary>
      {children}
    </details>
  );
}
