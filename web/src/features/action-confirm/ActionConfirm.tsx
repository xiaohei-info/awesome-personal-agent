import { useEffect, useState, type FormEvent } from "react";
import { Link, Route, Routes, useParams, useSearchParams } from "react-router-dom";
import { useSession } from "../../app/session";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";
import { useLabel } from "../../app/values";

// 身份由 Web 登录态承担：写请求走 Cookie + CSRF，服务端把操作者记为 Web 本人。链接本身不含凭据。
// 校验、授权、版本冲突都在服务端，本页只搬运输入并显示服务端的错误原因。
interface Item { id: string; title: string; phase: string; version: number; planned_for?: string }
const ACTIONS = { done: true, progress: true, plan: true } as const;
type Act = keyof typeof ACTIONS;


export function ActionConfirm() {
  return (
    <Routes>
      <Route path=":id" element={<Confirm />} />
    </Routes>
  );
}

function Confirm() {
  const { id = "" } = useParams();
  const [q] = useSearchParams();
  const act = q.get("do") as Act | null;
  const { api } = useSession();
  const t = useT(strings);
  const label = useLabel();
  const [item, setItem] = useState<Item | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState<string | null>(null);
  const known = act !== null && Object.hasOwn(ACTIONS, act);

  useEffect(() => {
    if (!known) return;
    setItem(null);
    setError(null);
    api.get<{ item: Item }>("/items/" + encodeURIComponent(id)).then((r) => setItem(r.item), (e) => setError(msg(e)));
  }, [api, id, known]);

  if (!known) return <main><p role="alert">{t.unsupported(act ?? t.missingDo)}</p><p><Link to="/">{t.home}</Link></p></main>;
  if (error && !item) return <main><p role="alert">{error}</p></main>;
  if (!item) return <main><p role="status">{t.loading}</p></main>;

  const path = "/items/" + encodeURIComponent(id);
  const ifMatch = { "If-Match": String(item.version) };
  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setError(null);
    try {
      await fn();
      setDone(ok);
    } catch (e) {
      setError(msg(e));
    }
  };

  return (
    <main className="act-confirm">
      <h1>{t.actions[act]}</h1>
      <div className="panel facts">
        <p><Link to={"/items/" + encodeURIComponent(id)}>{item.title}</Link></p>
        <p>{t.phase(label("phase", item.phase))}</p>
        {item.planned_for ? <p>{t.planned(item.planned_for)}</p> : null}
      </div>
      {error && <p role="alert">{error}</p>}
      {done ? (
        <p role="status">{done}</p>
      ) : act === "done" ? (
        item.phase === "done" ? (
          <p>{t.alreadyDone}</p>
        ) : (
          <button type="button" className="primary" onClick={() => run(() => api.post(path + "/transition", { to: "done", reason: t.doneReason }, ifMatch), t.doneOk)}>{t.confirmDone}</button>
        )
      ) : act === "progress" ? (
        <ProgressForm onSubmit={(b) => run(() => api.post(path + "/progress", b), t.progressOk)} />
      ) : (
        <PlanForm initial={item.planned_for ?? ""} onSubmit={(v) => run(() => api.patch(path, { planned_for: v }, ifMatch), t.planOk)} />
      )}
    </main>
  );
}

function ProgressForm({ onSubmit }: { onSubmit: (b: { text: string; on?: string }) => void }) {
  const t = useT(strings);
  const [text, setText] = useState("");
  const [on, setOn] = useState("");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    onSubmit({ text: text.trim(), ...(on ? { on } : {}) });
  };
  return (
    <form className="panel stack" onSubmit={submit}>
      <label>{t.progress} <textarea value={text} onChange={(e) => setText(e.target.value)} rows={4} /></label>
      <label>{t.date} <input type="date" value={on} onChange={(e) => setOn(e.target.value)} /></label>
      <button type="submit" disabled={!text.trim()}>{t.saveProgress}</button>
    </form>
  );
}

function PlanForm({ initial, onSubmit }: { initial: string; onSubmit: (v: string) => void }) {
  const t = useT(strings);
  const [v, setV] = useState(initial);
  return (
    <form className="panel stack" onSubmit={(e) => { e.preventDefault(); onSubmit(v); }}>
      <label>{t.plannedDate} <input type="date" value={v} onChange={(e) => setV(e.target.value)} /></label>
      <button type="submit" disabled={!v}>{t.savePlan}</button>
    </form>
  );
}
