import type { ReactNode } from "react";
import { Go } from "./page";

/** 前提清单的一步：没满足时一句状态加一个按钮——就地能做的直接做（onClick），否则带落点跳过去（to，带回程）。 */
export interface Prereq {
  ok: boolean;
  text: ReactNode;
  action?: { label: string; to?: string; onClick?: () => void };
}

/**
 * 前提清单（界面设计 §3.2 公共件，全站只有这一份）：把「这件事还差哪几步才能生效」列成清单。
 * 只列没满足的；全部满足时不渲染（不多说）。事项详情、建议卡、Agent 页共用。
 */
export function Prereqs({ items, label }: { items: Prereq[]; label: string }) {
  const left = items.filter((x) => !x.ok);
  if (!left.length) return null;
  return (
    <ol className="rows prereqs" aria-label={label}>
      {left.map((x, i) => (
        <li key={i} className="act-row">
          <div><span>{x.text}</span></div>
          {x.action?.to ? <Go className="btn" to={x.action.to}>{x.action.label}</Go> : x.action?.onClick && <button type="button" onClick={x.action.onClick}>{x.action.label}</button>}
        </li>
      ))}
    </ol>
  );
}
