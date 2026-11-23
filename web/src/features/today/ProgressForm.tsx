import { useState } from "react";
import { useT } from "../../app/i18n";
import { strings } from "./strings";

/** 写进展：文字必填，所指日期可选（不晚于今天由服务端判定）。今天页与事项详情共用。 */
// onSubmit 返回 false 表示没写成：保留输入，方便改了再存。
export function ProgressForm({ onSubmit, label, autoFocus }: { onSubmit: (b: { text: string }) => Promise<void | boolean> | void | boolean; label?: string; autoFocus?: boolean }) {
  const t = useT(strings);
  const [text, setText] = useState("");
  return (
    <form
      className="panel"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!text.trim()) return;
        if ((await onSubmit({ text: text.trim() })) !== false) setText("");
      }}
    >
      <label>{t.progress} <input value={text} autoFocus={autoFocus} onChange={(e) => setText(e.target.value)} /></label>
      <button type="submit" disabled={!text.trim()}>{label ?? t.writeProgress}</button>
    </form>
  );
}
