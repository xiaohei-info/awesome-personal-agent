import { useEffect, useState, type FormEvent } from "react";
import { useParams } from "react-router-dom";
import type { ApiErrorBody } from "../../app/api";

// 对应 GET/POST /forms/{令牌}（不带 /v1，无需登录）。页面只展示问题与选项，不含任何账本其他内容，也不判定令牌有效性——以服务端为准。
interface Form {
  question: string;
  options: { id: string; label: string }[];
  free_text: boolean;
}

type State =
  | { k: "loading" }
  | { k: "ready"; form: Form }
  | { k: "done" }
  | { k: "dead"; why: string } // 404 / 409：不存在、已用过、已过期或事项已变化，重试没有意义
  | { k: "web"; why: string; url: string | null } // web_only / 初筛未返回：去 Web 作答
  | { k: "error"; why: string };

async function call(method: string, token: string, body?: unknown) {
  const res = await fetch("/forms/" + encodeURIComponent(token), {
    method,
    credentials: "omit",
    referrerPolicy: "no-referrer",
    // Accept 让同一路径的浏览器导航拿到页面、这里的 fetch 拿到 JSON。
    headers: { Accept: "application/json", ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.ok) return res.json();
  let e: ApiErrorBody;
  try {
    e = await res.json();
    if (typeof e?.code !== "string") throw new Error();
  } catch {
    e = { code: "internal", why: `unexpected response (HTTP ${res.status})`, next: "retry later" };
  }
  throw Object.assign(new Error(e.why), { status: res.status, body: e });
}

function toState(e: unknown): State {
  const b = (e as { body?: ApiErrorBody }).body;
  if (b && (b.code === "not_found" || b.code === "conflict")) return { k: "dead", why: b.why };
  if (b && (b.code === "web_only" || b.code === "risk_screening_pending")) return { k: "web", why: b.why, url: b.web_url ?? null };
  return { k: "error", why: b ? `${b.why}（${b.next}）` : e instanceof Error ? e.message : "request failed" };
}

export function FormPage() {
  const token = useParams()["*"] ?? "";
  const [st, setSt] = useState<State>({ k: "loading" });
  const [choice, setChoice] = useState("");
  const [text, setText] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    setSt({ k: "loading" });
    if (!token) {
      setSt({ k: "dead", why: "form link not found" });
      return;
    }
    call("GET", token).then(
      (f: Form) => live && setSt({ k: "ready", form: { ...f, options: f.options ?? [] } }),
      (e) => live && setSt(toState(e)),
    );
    return () => {
      live = false;
    };
  }, [token]);

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await call("POST", token, { ...(choice && { choice }), ...(text.trim() && { text: text.trim() }), ...(reason.trim() && { reason: reason.trim() }) });
      setSt({ k: "done" });
    } catch (e) {
      const next = toState(e);
      // 校验类失败令牌不算用过，留在表单上让本人改；失效/转 Web 才换页面。
      if (next.k === "error") setErr(next.why);
      else setSt(next);
    }
    setBusy(false);
  };

  switch (st.k) {
    case "loading":
      return <main className="form-page"><p role="status">加载中…</p></main>;
    case "done":
      return (
        <main className="form-page">
          <h1>已提交</h1>
          <p>你的回答已记录，此链接已失效，可以关闭本页。</p>
        </main>
      );
    case "dead":
      return (
        <main className="form-page">
          <h1>链接已失效</h1>
          <p role="alert">此链接已被使用、已过期或对应事项已变化（{st.why}）。请使用最新消息中的链接，或在 Web 中作答。</p>
        </main>
      );
    case "web":
      return (
        <main className="form-page">
          <h1>请在 Web 中作答</h1>
          <p role="alert">{st.why}</p>
          {st.url && <a href={st.url}>前往 Web 登录并作答</a>}
        </main>
      );
    case "error":
      return (
        <main className="form-page">
          <p role="alert">无法加载表单：{st.why}</p>
          <button onClick={() => location.reload()}>重试</button>
        </main>
      );
  }

  const { form } = st;
  const canSubmit = !busy && (choice !== "" || (form.free_text && text.trim() !== ""));
  return (
    <main className="form-page">
      <form className="panel" onSubmit={submit}>
        <h1>{form.question}</h1>
        {form.options.length > 0 && (
          <fieldset>
            <legend>选项</legend>
            {form.options.map((o) => (
              <label key={o.id}>
                <input type="radio" name="choice" value={o.id} checked={choice === o.id} onChange={() => setChoice(o.id)} /> {o.label}
              </label>
            ))}
          </fieldset>
        )}
        {form.free_text && (
          <label>
            补充说明
            <textarea value={text} onChange={(e) => setText(e.target.value)} />
          </label>
        )}
        <label>
          理由（可选）
          <input value={reason} onChange={(e) => setReason(e.target.value)} />
        </label>
        {err && <p role="alert">{err}</p>}
        <div>
          <button type="submit" disabled={!canSubmit}>
            提交
          </button>
        </div>
        <p className="sub">此链接只能提交一次。</p>
      </form>
    </main>
  );
}
