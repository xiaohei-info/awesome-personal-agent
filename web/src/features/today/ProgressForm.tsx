import { useState } from "react";

/** 写进展：文字必填，所指日期可选（不晚于今天由服务端判定）。今天页与事项详情共用。 */
export function ProgressForm({ onSubmit, label = "写进展" }: { onSubmit: (b: { text: string }) => Promise<void> | void; label?: string }) {
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
      <label>进展 <input value={text} onChange={(e) => setText(e.target.value)} /></label>
      <button type="submit" disabled={!text.trim()}>{label}</button>
    </form>
  );
}
