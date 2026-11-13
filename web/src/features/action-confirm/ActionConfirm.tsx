import { useEffect, useState, type FormEvent } from "react";
import { Link, Route, Routes, useParams, useSearchParams } from "react-router-dom";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";

// 身份由 Web 登录态承担：写请求走 Cookie + CSRF，服务端把操作者记为 Web 本人。链接本身不含凭据。
// 校验、授权、版本冲突都在服务端，本页只搬运输入并显示服务端的错误原因。
interface Item { id: string; title: string; phase: string; version: number; planned_for?: string }
const ACTIONS = { done: "完成事项", progress: "写进展", plan: "改计划日期" } as const;
type Act = keyof typeof ACTIONS;

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

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

  if (!known) return <main><p role="alert">{`不支持的动作：${act ?? "（缺少 do 参数）"}`}</p><p><Link to="/">回到首页</Link></p></main>;
  if (error && !item) return <main><p role="alert">{error}</p></main>;
  if (!item) return <main><p role="status">加载中…</p></main>;

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
      <h1>{ACTIONS[act]}</h1>
      <div className="panel facts">
        <p><Link to={"/items/" + encodeURIComponent(id)}>{item.title}</Link></p>
        <p>{`阶段：${item.phase}`}</p>
        {item.planned_for ? <p>{`计划日期：${item.planned_for}`}</p> : null}
      </div>
      {error && <p role="alert">{error}</p>}
      {done ? (
        <p role="status">{done}</p>
      ) : act === "done" ? (
        item.phase === "done" ? (
          <p>该事项已经是完成状态</p>
        ) : (
          <button type="button" className="primary" onClick={() => run(() => api.post(path + "/transition", { to: "done", reason: "Web 动作链接确认" }, ifMatch), "已完成")}>确认完成</button>
        )
      ) : act === "progress" ? (
        <ProgressForm onSubmit={(b) => run(() => api.post(path + "/progress", b), "已记录进展")} />
      ) : (
        <PlanForm initial={item.planned_for ?? ""} onSubmit={(v) => run(() => api.patch(path, { planned_for: v }, ifMatch), "已改计划日期")} />
      )}
    </main>
  );
}

function ProgressForm({ onSubmit }: { onSubmit: (b: { text: string; on?: string }) => void }) {
  const [text, setText] = useState("");
  const [on, setOn] = useState("");
  const submit = (e: FormEvent) => {
    e.preventDefault();
    onSubmit({ text: text.trim(), ...(on ? { on } : {}) });
  };
  return (
    <form className="panel stack" onSubmit={submit}>
      <label>进展 <textarea value={text} onChange={(e) => setText(e.target.value)} rows={4} /></label>
      <label>日期（可选） <input type="date" value={on} onChange={(e) => setOn(e.target.value)} /></label>
      <button type="submit" disabled={!text.trim()}>保存进展</button>
    </form>
  );
}

function PlanForm({ initial, onSubmit }: { initial: string; onSubmit: (v: string) => void }) {
  const [v, setV] = useState(initial);
  return (
    <form className="panel stack" onSubmit={(e) => { e.preventDefault(); onSubmit(v); }}>
      <label>计划日期 <input type="date" value={v} onChange={(e) => setV(e.target.value)} /></label>
      <button type="submit" disabled={!v}>保存计划日期</button>
    </form>
  );
}
