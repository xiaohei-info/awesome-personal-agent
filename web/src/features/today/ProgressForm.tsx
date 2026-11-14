import { useState } from "react";
import { useT } from "../../app/i18n";
import { strings } from "./strings";

/** 写进展：文字必填，所指日期可选（不晚于今天由服务端判定）。今天页与事项详情共用。 */
export function ProgressForm({ onSubmit, label }: { onSubmit: (b: { text: string }) => Promise<void> | void; label?: string }) {
  const t = useT(strings);
  const [text, setText] = useState("");
  return (
    <form
      className="panel"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!text.trim()) return;
        await onSubmit({ text: text.trim() });
        setText("");
      }}
    >
      <label>{t.progress} <input value={text} onChange={(e) => setText(e.target.value)} /></label>
      <button type="submit" disabled={!text.trim()}>{label ?? t.writeProgress}</button>
    </form>
  );
}
