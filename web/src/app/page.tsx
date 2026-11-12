import { createContext, useContext, type ReactNode } from "react";

/** 页面里的数据变了、导航上的数量需要重取时派发（如答完一条决策）。 */
export const COUNTS_EVENT = "apa:counts";

// 外壳按当前页面所在的导航分组提供小字页眉；页面只写标题，不重复声明分组。
export const GroupContext = createContext<string | undefined>(undefined);

/** 页头：小字分组名 + 宋体 h1 + 可选导语。 */
export function PageHead({ children, lede }: { children: ReactNode; lede?: ReactNode }) {
  const group = useContext(GroupContext);
  return (
    <>
      {group && <p className="eyebrow">{group}</p>}
      <h1>{children}</h1>
      {lede && <p className="lede">{lede}</p>}
    </>
  );
}
