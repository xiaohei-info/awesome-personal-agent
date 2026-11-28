import { type ReactNode } from "react";
import { Link, useLocation, type LinkProps } from "react-router-dom";
import { useT } from "./i18n";
import { common } from "./strings";

/** 页面里的数据变了、导航上的数量需要重取时派发（如答完一条决策）。 */
export const COUNTS_EVENT = "apa:counts";

/** 页头：一行小标题加可选导语。入口名与页签在外壳的工具条上，页面不重复。 */
export function PageHead({ children, lede }: { children: ReactNode; lede?: ReactNode }) {
  return (
    <>
      <h1>{children}</h1>
      {lede && <p className="lede">{lede}</p>}
    </>
  );
}

/**
 * 跨页面的跳转（界面设计 §3.1 第 4 条）：落到具体对象的具体区块（to 里带锚点），并记下来时的地址，
 * 目标页顶部的 BackBar 据此给出「返回刚才的页面」。地址本身不变，旧书签与链接照常可用。
 */
export function Go({ to, className, children, ...rest }: Omit<LinkProps, "state"> & { to: string }) {
  const { pathname, search, hash } = useLocation();
  return (
    <Link to={to} state={{ from: pathname + search + hash }} className={className} {...rest}>
      {children}
    </Link>
  );
}

/** 经 Go 跳过来的页面顶部：一键回到刚才的页面。直接打开的页面不显示。 */
export function BackBar() {
  const t = useT(common);
  const from = (useLocation().state as { from?: string } | null)?.from;
  if (!from) return null;
  return (
    <p className="backbar">
      <Link to={from}>‹ {t.backTo}</Link>
    </p>
  );
}
