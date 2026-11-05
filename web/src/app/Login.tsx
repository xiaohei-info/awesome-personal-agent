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
    <main>
      <h1>登录 APA 中台</h1>
      <form onSubmit={submit}>
        <label>
          口令
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required autoFocus />
        </label>
        <button type="submit" disabled={busy}>登录</button>
        {error && <p role="alert">{error}</p>}
      </form>
    </main>
  );
}
