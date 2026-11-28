import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { errorText, useT } from "../../app/i18n";
import { useSession } from "../../app/session";
import { useLabel } from "../../app/values";
import { Icon, toast, toastError, undoToast } from "../../app/ui";
import type { Item } from "./ItemsPage";
import { DoneCircle, RowActions, type RowAct } from "./RowActions";
import { strings } from "./strings";

/**
 * 事项的一行（Multica 的列表行）：行首状态圆圈（点一下完成）· 紧急或高优先级的小图标 · 标题（单行截断）……靠右一列次要字（meta）；
 * 其余操作悬停或聚焦才浮在行尾（RowActions）。今天页、事项列表、项目详情共用这一行。
 * 普通点击标题整页打开，Shift+点击在右侧速览（宽屏才有 onPeek，拍板项 1）。
 */
export function ItemRow({ item, meta, where, to, state, onChange, onDone, onPeek, cursor, peeked, entering }: {
  item: Item;
  meta?: ReactNode;
  /** 写进处理经过的「在哪里做的」，如「今天页」 */
  where: string;
  to: string;
  state?: unknown;
  onChange: (it: Item, what: RowAct) => void;
  onDone: () => void;
  onPeek?: () => void;
  cursor?: boolean;
  peeked?: boolean;
  entering?: boolean;
}) {
  const t = useT(strings);
  const prio = item.priority === "urgent" || item.priority === "high" ? item.priority : null;
  return (
    <li data-item={item.id} data-phase={item.phase} data-cursor={cursor || undefined} data-peeked={peeked || undefined} className={["trow", entering && "row-enter"].filter(Boolean).join(" ")}>
      <div className="act-row trow-line">
        <DoneCircle item={item} onDone={onDone} />
        {prio && (
          <span className="prio" data-prio={prio}>
            {prio === "urgent" ? <Icon.PrioUrgent /> : <Icon.PrioHigh />}
            <span className="sr-only">{t.prio[prio]}</span>
          </span>
        )}
        <Link className="trow-title" to={to} state={state} onClick={(e) => { if (e.shiftKey && onPeek) { e.preventDefault(); onPeek(); } }}>{item.title}</Link>
        <span className="sub reasons">{meta}</span>
        <RowActions item={item} where={where} onChange={onChange} />
      </div>
    </li>
  );
}

/**
 * 行首圆圈的「完成」：与详情的完成同一个接口；写完重读这一件交给 replace 原位替换，右下角一条可撤销的提示，
 * 撤销就是改回原来的阶段（等待中的带回原来在等的对象）。事项列表、项目详情共用；今天页另有按今天口径重排的一份。
 */
export function useCircleDone(where: string, replace: (it: Item) => void) {
  const t = useT(strings);
  const { api } = useSession();
  const label = useLabel();
  return async (it: Item) => {
    const path = "/items/" + encodeURIComponent(it.id);
    try {
      await api.post(path + "/transition", { to: "done", reason: t.rowDoneReason(where) }, { "If-Match": String(it.version) });
    } catch (e) {
      toastError(e);
    }
    const after = await api.get<{ item: Item }>(path).then((r) => r.item, () => null);
    if (!after) return;
    replace(after);
    if (after.phase !== "done") return;
    const back = { to: it.phase, reason: t.rowUndoReason(where), ...(it.phase === "waiting" && it.waiting_on && { waiting_on: it.waiting_on }) };
    undoToast(t.toastDone(after.title), async () => {
      try {
        await api.post(path + "/transition", back, { "If-Match": String(after.version) });
        toast(t.undone(label("phase", it.phase)));
      } catch (e) {
        toastError(t.undoFailed(errorText(e)));
      }
      await api.get<{ item: Item }>(path).then((r) => replace(r.item), () => undefined);
    });
  };
}
