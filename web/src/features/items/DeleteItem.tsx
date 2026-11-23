import { useState } from "react";
import { useT } from "../../app/i18n";
import { strings } from "./strings";

// 删除 = 转到已放弃（走现有阶段转移，不另造状态、不物理删除）；先二次确认，确认文案写明去向与可找回。
// 按钮不用朱砂：这个操作可逆，朱砂只给不可逆操作。详情主操作区与列表行「更多」共用。
export function DeleteItem({ title, busy, onConfirm }: { title: string; busy?: boolean; onConfirm: () => void }) {
  const t = useT(strings);
  const [asking, setAsking] = useState(false);
  if (!asking) return <button type="button" aria-expanded={false} onClick={() => setAsking(true)}>{t.del}</button>;
  return (
    <div className="panel row-panel" role="group" aria-label={t.delFor(title)}>
      <p>{t.delNote}</p>
      <div className="actions">
        <button type="button" className="primary" disabled={busy} onClick={onConfirm}>{t.confirmDel}</button>
        <button type="button" onClick={() => setAsking(false)}>{t.cancel}</button>
      </div>
    </div>
  );
}
