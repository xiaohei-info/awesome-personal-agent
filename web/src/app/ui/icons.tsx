import type { ReactNode } from "react";

// 手写线性图标（交互样式规格 §5.8）：24 格网格，16px 时描边 1.5，颜色跟随文字，一律 aria-hidden。
// 只用在展开箭头、更多、速览头部、提示条、搜索、保存状态、空状态；导航与页签不加图标。
type Size = "sm" | "md" | "lg";
const make = (name: string, body: ReactNode) =>
  function Icon({ size = "md", spin }: { size?: Size; spin?: boolean }) {
    return (
      <svg className={["icon", "icon-" + name, size !== "md" && size, spin && "spin"].filter(Boolean).join(" ")} viewBox="0 0 24 24" width="16" height="16"
        fill="none" stroke="currentColor" strokeWidth={size === "lg" ? 1.25 : 2.25} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
        {body}
      </svg>
    );
  };

export const Chevron = make("chevron", <path d="M9 6l6 6-6 6" />);
export const More = make("more", <><circle cx="5" cy="12" r="1" /><circle cx="12" cy="12" r="1" /><circle cx="19" cy="12" r="1" /></>);
export const Check = make("check", <path d="M5 12.5l4.5 4.5L19 7.5" />);
export const Close = make("close", <path d="M6 6l12 12M18 6L6 18" />);
export const Search = make("search", <><circle cx="11" cy="11" r="6.5" /><path d="M16 16l4 4" /></>);
export const Undo = make("undo", <><path d="M9 14L4 9l5-5" /><path d="M4 9h10.5a5.5 5.5 0 010 11H11" /></>);
export const External = make("external", <><path d="M14 4h6v6" /><path d="M20 4l-9 9" /><path d="M18 14v5a1 1 0 01-1 1H5a1 1 0 01-1-1V7a1 1 0 011-1h5" /></>);
export const Plus = make("plus", <path d="M12 5v14M5 12h14" />);
export const Up = make("up", <path d="M6 15l6-6 6 6" />);
export const Down = make("down", <path d="M6 9l6 6 6-6" />);
export const Spinner = make("spinner", <path d="M12 3a9 9 0 109 9" />);
export const EmptyList = make("empty", <><rect x="4" y="4" width="16" height="16" rx="3" /><path d="M8 9h8M8 13h5" /></>);
