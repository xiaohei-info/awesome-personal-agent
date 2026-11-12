import { useState, type FormEvent } from "react";
import { ApiError } from "./api";
import { useSession } from "./session";

export function Login() {
  const { login } = useSession();
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(password);
    } catch (err) {
      setError(err instanceof ApiError ? `${err.body.why}（${err.body.next}）` : "无法连接服务");
      setBusy(false);
    }
  }

  return (
    <main className="login">
      <div className="pitch">
        <div className="brand">
          <span className="mark" aria-hidden="true">决</span>
          <div>
            <b>APA</b>
            <small>awesome personal agent</small>
          </div>
        </div>
        <p className="big">
          事情交给 agent，
          <br />
          <em>决定</em>留给你。
        </p>
        <p className="sub">agent 替你读邮件、看日历、推进事项。遇到该你拍板的事，它会停下来，在这里等你一句话。</p>
      </div>
      <div className="gate">
        <h1>登录</h1>
        <form onSubmit={submit}>
          <label>
            口令
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required autoFocus />
          </label>
          <div>
            <button type="submit" disabled={busy}>登录</button>
          </div>
          {error && <p role="alert">{error}</p>}
        </form>
      </div>
    </main>
  );
}
