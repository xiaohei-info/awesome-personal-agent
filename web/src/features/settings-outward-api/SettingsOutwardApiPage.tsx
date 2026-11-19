import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";

// 设置 › 对外接口：外部程序来取数据（导出）或接转发用的密钥（§4.1、§10.3、§14.2）。与「提醒渠道」的渠道钥匙不是一回事：渠道钥匙是 APA 把提醒推给外部程序，这里是外部程序来向 APA 要。
// scope：export 或 export.<名> 是导出，forward 是转发。授权、种类校验、明文只出现一次都在服务端。
const EXT_TYPES = ["export", "forward"] as const;
const scopeOf = (type: string, name: string) => (type === "forward" ? "forward" : name ? `${type}.${name}` : type);
const mine = (scope?: string) => !!scope && EXT_TYPES.some((x) => scope === x || scope.startsWith(x + "."));

interface Credential { id: string; kind: string; label?: string; scope?: string; revoked_at?: string }

export function SettingsOutwardApiPage() {
  const { api } = useSession();
  const t = useT(strings);
  // 明文只放在组件状态里，离开页面、主动关闭或再次签发即丢弃。
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
    return t.scopeLabel(t.types[kind] ?? sc, rest.join("."));
  };

  return (
    <main className="stack">
      <PageHead lede={t.lede}>{t.title}</PageHead>
      <p className="sub">{t.hint}</p>
      <p className="sub">{t.notChannel}</p>
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
            {EXT_TYPES.map((x) => <option key={x} value={x}>{t.types[x]}</option>)}
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
