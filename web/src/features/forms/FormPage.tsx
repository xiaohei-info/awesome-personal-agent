import { useEffect, useState, type FormEvent } from "react";
import { useParams } from "react-router-dom";
import type { ApiErrorBody } from "../../app/api";
import { codeText, useT } from "../../app/i18n";
import { strings } from "./strings";

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
  return { k: "error", why: b ? codeText(b) : e instanceof Error ? e.message : "request failed" };
}

export function FormPage() {
  const token = useParams()["*"] ?? "";
  const t = useT(strings);
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
      return <main className="form-page"><p role="status">{t.loading}</p></main>;
    case "done":
      return (
        <main className="form-page">
          <h1>{t.submitted}</h1>
          <p>{t.submittedBody}</p>
        </main>
      );
    case "dead":
      return (
        <main className="form-page">
          <h1>{t.expired}</h1>
          <p role="alert">{t.expiredBody}</p>
          {/* 链接已用过、过期或事项已变化：服务端的原因收在详情里，下一步是去网页作答 */}
          <details>
            <summary>{t.details}</summary>
            <p lang="en">{st.why}</p>
          </details>
          <a href="/decisions">{t.goWeb}</a>
        </main>
      );
    case "web":
      return (
        <main className="form-page">
          <h1>{t.useWeb}</h1>
          <p role="alert">{st.why}</p>
          {st.url && <a href={st.url}>{t.goWeb}</a>}
        </main>
      );
    case "error":
      return (
        <main className="form-page">
          <p role="alert">{t.loadFailed(st.why)}</p>
          <button onClick={() => location.reload()}>{t.retry}</button>
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
            <legend>{t.options}</legend>
            {form.options.map((o) => (
              <label key={o.id}>
                <input type="radio" name="choice" value={o.id} checked={choice === o.id} onChange={() => setChoice(o.id)} /> {o.label}
              </label>
            ))}
          </fieldset>
        )}
        {form.free_text && (
          <label>
            {t.note}
            <textarea value={text} onChange={(e) => setText(e.target.value)} />
          </label>
        )}
        <label>
          {t.reason}
          <input value={reason} onChange={(e) => setReason(e.target.value)} />
        </label>
        {err && <p role="alert">{err}</p>}
        <div>
          <button type="submit" disabled={!canSubmit}>
            {t.submit}
          </button>
        </div>
        <p className="sub">{t.once}</p>
      </form>
    </main>
  );
}
