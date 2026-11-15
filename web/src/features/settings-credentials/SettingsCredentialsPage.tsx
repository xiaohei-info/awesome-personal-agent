import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 设置 › 扩展程序：导出与转发用的 ext 密钥（§10.3、§14.2）。给 Agent 的钥匙在「设置 › Agent」，渠道的在「设置 › 渠道」。
// scope：export 或 export.<名> 是导出，forward 是转发。授权、种类校验、明文只出现一次都在服务端。
const EXT_TYPES = ["export", "forward"] as const;
const scopeOf = (type: string, name: string) => (type === "forward" ? "forward" : name ? `${type}.${name}` : type);
const mine = (scope?: string) => !!scope && EXT_TYPES.some((x) => scope === x || scope.startsWith(x + "."));

interface Credential { id: string; kind: string; label?: string; scope?: string; revoked_at?: string }

export function SettingsCredentialsPage() {
  const { api } = useSession();
  const t = useT(strings);
  // 明文只放在这个组件状态里：不进存储、不进 URL；关闭或再次签发即丢弃。
  const [secret, setSecret] = useState<string | null>(null);
  const [creds, setCreds] = useState<Credential[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [type, setType] = useState<string>("export");
  const [name, setName] = useState("");
  const [label, setLabel] = useState("");
  const [busy, setBusy] = useState(false);
  const load = useCallback(
    () => api.get<{ credentials: Credential[] }>("/settings/ext-keys").then((r) => setCreds(r.credentials.filter((c) => mine(c.scope))), (e) => setError(msg(e))),
    [api],
  );
  useEffect(() => void load(), [load]);

  const issue = async () => {
    setBusy(true);
    setError(null);
    setSecret(null);
    const body: Record<string, string> = { scope: scopeOf(type, name.trim()) };
    if (label.trim()) body.label = label.trim();
    try {
      setSecret((await api.post<{ secret: string }>("/settings/ext-keys", body)).secret);
      setName("");
      setLabel("");
    } catch (e) {
      setError(msg(e));
    } finally {
      setBusy(false);
    }
    await load();
  };
  const revoke = async (id: string) => {
    setError(null);
    try {
      await api.del("/settings/ext-keys/" + encodeURIComponent(id));
    } catch (e) {
      setError(msg(e));
    }
    await load();
  };
  const scopeText = (sc: string) => {
    const [kind, ...rest] = sc.split(".");
    return t.scopeLabel(t.ext[kind] ?? sc, rest.join("."));
  };

  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {secret && (
        <div role="alert" className="secret">
          <p>{t.secretOnce}</p>
          <code>{secret}</code>
          <div><button onClick={() => setSecret(null)}>{t.savedIt}</button></div>
        </div>
      )}
      {error && <div role="alert">{error}</div>}
      <form className="panel" onSubmit={(e) => { e.preventDefault(); void issue(); }}>
        <label>
          {t.scope}
          <select value={type} onChange={(e) => setType(e.target.value)}>
            {EXT_TYPES.map((x) => <option key={x} value={x}>{t.ext[x]}</option>)}
          </select>
        </label>
        {type === "export" && <label>{t.exportName}<input value={name} onChange={(e) => setName(e.target.value)} /></label>}
        <label>{t.label}<input value={label} onChange={(e) => setLabel(e.target.value)} /></label>
        <div><button type="submit" disabled={busy}>{t.issue}</button></div>
      </form>
      {creds === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : creds.length === 0 ? (
        <p className="sub">{t.none}</p>
      ) : (
        <ul className="rows">
          {creds.map((c) => (
            <li key={c.id}>
              <strong>{c.label || c.id}</strong> {c.scope && <span className="sub">{scopeText(c.scope)}</span>}
              {c.revoked_at ? <span className="badge">{t.revoked}</span> : <button className="danger" onClick={() => void revoke(c.id)}>{t.revoke}</button>}
            </li>
          ))}
        </ul>
      )}
    </main>
  );
}
