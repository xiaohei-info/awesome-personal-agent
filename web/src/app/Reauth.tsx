import { useState, type FormEvent } from "react";
import type { ApiClient } from "./api";
import { errorText, useT } from "./i18n";
import { common } from "./strings";

// 3b 动作（发钥匙、填凭据、改工具权限等，设计 §14.3）前再输一次口令：输对了原请求自动重发；取消则原操作照常报「要再输一次口令」。
export function Reauth({ api, done }: { api: ApiClient; done: (ok: boolean) => void }) {
  const t = useT(common);
  const [password, setPw] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.reauth(password);
      done(true);
    } catch (err) {
      setError(errorText(err));
      setBusy(false);
    }
  }
  return (
    <div className="palette-backdrop">
      <div role="dialog" aria-modal="true" aria-label={t.reauthTitle} className="palette panel stack">
        <h2>{t.reauthTitle}</h2>
        <p className="sub">{t.reauthWhy}</p>
        <form onSubmit={submit} className="stack">
          <label>
            {t.password}
            <input type="password" value={password} onChange={(e) => setPw(e.target.value)} autoComplete="current-password" required autoFocus />
          </label>
          {error && <p role="alert">{error}</p>}
          <div>
            <button type="submit" className="primary" disabled={busy}>{t.reauthGo}</button>{" "}
            <button type="button" onClick={() => done(false)}>{t.cancel}</button>
          </div>
        </form>
      </div>
    </div>
  );
}
