import { useState, type FormEvent } from "react";
import { ApiError } from "./api";
import { errorText, setChoice, useLang, useT } from "./i18n";
import { useSession } from "./session";
import { BRAND_MARK, LANG_NAMES, common } from "./strings";

export function Login() {
  const { login } = useSession();
  const lang = useLang();
  const t = useT(common);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const other = lang === "zh" ? "en" : "zh";

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(password);
    } catch (err) {
      setError(err instanceof ApiError ? errorText(err) : t.cannotConnect);
      setBusy(false);
    }
  }

  return (
    <main className="login">
      <div className="pitch">
        <div className="brand">
          <span className="mark" aria-hidden="true">{BRAND_MARK}</span>
          <div>
            <b>APA</b>
            <small>awesome personal agent</small>
          </div>
        </div>
        <p className="big">
          {t.loginPitch1}
          <br />
          <em>{t.loginPitch2}</em>
          {t.loginPitch3}
        </p>
        <p className="sub">{t.loginSub}</p>
      </div>
      <div className="gate">
        {/* 未登录时进不了设置页，所以登录页自带语言切换 */}
        <p className="lang-switch">
          <button type="button" className="link" lang={other} aria-label={`${t.language}: ${LANG_NAMES[other]}`} onClick={() => setChoice(other)}>
            {LANG_NAMES[other]}
          </button>
        </p>
        <h1>{t.login}</h1>
        <form onSubmit={submit}>
          <label>
            {t.password}
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required autoFocus />
          </label>
          <div>
            <button type="submit" disabled={busy}>{t.login}</button>
          </div>
          {error && <p role="alert">{error}</p>}
        </form>
      </div>
    </main>
  );
}
