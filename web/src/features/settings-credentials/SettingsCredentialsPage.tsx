import { useCallback, useEffect, useState } from "react";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";
import { errorText as msg, useT } from "../../app/i18n";
import { strings } from "./strings";
import { Pick, Val } from "../../app/values";
import { EntityPick } from "../../app/names";

// 扩展程序密钥的 scope（§10.3）：channel.<名> 是消息渠道，export 或 export.<名> 是导出，forward 是转发。
const EXT_TYPES = ["channel", "export", "forward"] as const;
const scopeOf = (type: string, name: string) => (type === "forward" ? "forward" : name ? `${type}.${name}` : type);
function parseScope(scope: string): [string, string] {
  const [type, ...rest] = scope.split(".");
  return [type, rest.join(".")];
}

// 形状对应 /v1/settings/{tokens,ext-keys} 返回的 auth.Credential。授权（仅 Web 会话）、
// 种类与分组校验、明文只出现一次都在服务端；本页只负责调用并如实展示服务端的回答。
interface Credential {
  id: string;
  kind: string;
  label?: string;
  executor?: string;
  scope?: string;
  created_at: string;
  revoked_at?: string;
}


interface Group {
  path: "tokens" | "ext-keys";
}

function CredentialSection({ group, onSecret }: { group: Group; onSecret: (s: string | null) => void }) {
  const { api } = useSession();
  const t = useT(strings);
  const title = t.groups[group.path];
  const base = "/settings/" + group.path;
  const isExt = group.path === "ext-keys";
  const [creds, setCreds] = useState<Credential[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [kind, setKind] = useState("observer");
  const [label, setLabel] = useState("");
  const [executor, setExecutor] = useState("");
  const [extType, setExtType] = useState<string>("channel");
  const [extName, setExtName] = useState("");
  const scope = scopeOf(extType, extName.trim());
  const [busy, setBusy] = useState(false);

  const load = useCallback(
    () =>
      api.get<{ credentials: Credential[] }>(base).then(
        (r) => setCreds(r.credentials),
        (e) => setError(msg(e)),
      ),
    [api, base],
  );

  useEffect(() => {
    load();
  }, [load]);

  const issue = async () => {
    setBusy(true);
    setError(null);
    onSecret(null);
    const body: Record<string, string> = isExt ? { scope } : { kind };
    if (label.trim()) body.label = label.trim();
    if (!isExt && kind === "worker" && executor.trim()) body.executor = executor.trim();
    try {
      const r = await api.post<{ credential: Credential; secret: string }>(base, body);
      onSecret(r.secret);
      setLabel("");
      setExecutor("");
      setExtName("");
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
      await api.del(base + "/" + encodeURIComponent(id));
    } catch (e) {
      setError(msg(e));
    }
    await load(); // 以服务端状态为准
  };

  const scopeText = (sc: string) => {
    const [type, name] = parseScope(sc);
    return t.ext[type] ? t.scopeLabel(t.ext[type], name) : sc;
  };
  const heading = `heading-${group.path}`;
  return (
    <section role="region" aria-labelledby={heading}>
      <h2 id={heading}>{title}</h2>
      {error && <div role="alert">{error}</div>}
      <form
        className="panel"
        onSubmit={(e) => {
          e.preventDefault();
          issue();
        }}
      >
        {isExt ? (
          <>
            <label>
              {t.scope}
              <select value={extType} onChange={(e) => setExtType(e.target.value)}>
                {EXT_TYPES.map((x) => <option key={x} value={x}>{t.ext[x]}</option>)}
              </select>
            </label>
            {extType !== "forward" && (
              <label>
                {extType === "channel" ? t.channelName : t.exportName}
                <input value={extName} onChange={(e) => setExtName(e.target.value)} />
              </label>
            )}
          </>
        ) : (
          <label>
            {t.kind}
            <Pick k="credKind" value={kind} onChange={setKind} ids={["observer", "worker", "delegate"]} />
          </label>
        )}
        <label>
          {t.label}
          <input value={label} onChange={(e) => setLabel(e.target.value)} />
        </label>
        {!isExt && kind === "worker" && (
          <label>
            {t.executor}
            <EntityPick kind="executor" value={executor} onChange={setExecutor} empty={t.noExecutor} />
          </label>
        )}
        <div>
          <button type="submit" disabled={busy || (isExt && extType === "channel" && !extName.trim())}>{t.issue}</button>
        </div>
      </form>
      {creds === null ? (
        !error && <p role="status">{t.loading}</p>
      ) : creds.length === 0 ? (
        <p className="sub">{t.none}</p>
      ) : (
        <ul className="rows">
          {creds.map((c) => (
            <li key={c.id}>
              <strong>{c.label || c.id}</strong> <span className="badge plain"><Val k="credKind" id={c.kind} /></span>
              {c.scope && <span className="sub">{scopeText(c.scope)}</span>}
              {c.executor && <code>{c.executor}</code>}
              {c.revoked_at ? <span className="badge">{t.revoked}</span> : <button className="danger" onClick={() => revoke(c.id)}>{t.revoke}</button>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export function SettingsCredentialsPage() {
  // 明文只放在这个组件状态里：不进存储、不进 URL；关闭或再次签发即丢弃，刷新页面即消失。
  const [secret, setSecret] = useState<string | null>(null);
  const t = useT(strings);
  return (
    <main>
      <PageHead lede={t.lede}>{t.title}</PageHead>
      {secret && (
        <div role="alert" className="secret">
          <p>{t.secretOnce}</p>
          <code>{secret}</code>
          <div>
            <button onClick={() => setSecret(null)}>{t.savedIt}</button>
          </div>
        </div>
      )}
      <CredentialSection group={{ path: "tokens" }} onSecret={setSecret} />
      <CredentialSection group={{ path: "ext-keys" }} onSecret={setSecret} />
    </main>
  );
}
