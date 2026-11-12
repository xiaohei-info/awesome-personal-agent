import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

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

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

interface Group {
  path: "tokens" | "ext-keys";
  title: string;
}

function CredentialSection({ group, onSecret }: { group: Group; onSecret: (s: string | null) => void }) {
  const { api } = useSession();
  const base = "/settings/" + group.path;
  const isExt = group.path === "ext-keys";
  const [creds, setCreds] = useState<Credential[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [kind, setKind] = useState("observer");
  const [label, setLabel] = useState("");
  const [executor, setExecutor] = useState("");
  const [scope, setScope] = useState("");
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
    const body: Record<string, string> = isExt ? { scope: scope.trim() } : { kind };
    if (label.trim()) body.label = label.trim();
    if (!isExt && kind === "worker" && executor.trim()) body.executor = executor.trim();
    try {
      const r = await api.post<{ credential: Credential; secret: string }>(base, body);
      onSecret(r.secret);
      setLabel("");
      setExecutor("");
      setScope("");
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

  const heading = `heading-${group.path}`;
  return (
    <section role="region" aria-labelledby={heading}>
      <h2 id={heading}>{group.title}</h2>
      {error && <div role="alert">{error}</div>}
      <form
        className="panel"
        onSubmit={(e) => {
          e.preventDefault();
          issue();
        }}
      >
        {isExt ? (
          <label>
            范围（scope）
            <input value={scope} onChange={(e) => setScope(e.target.value)} />
          </label>
        ) : (
          <label>
            类型
            <select value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="observer">observer</option>
              <option value="worker">worker</option>
              <option value="delegate">delegate</option>
            </select>
          </label>
        )}
        <label>
          标签
          <input value={label} onChange={(e) => setLabel(e.target.value)} />
        </label>
        {!isExt && kind === "worker" && (
          <label>
            执行者
            <input value={executor} onChange={(e) => setExecutor(e.target.value)} />
          </label>
        )}
        <div>
          <button type="submit" disabled={busy || (isExt && !scope.trim())}>签发</button>
        </div>
      </form>
      {creds === null ? (
        !error && <p role="status">加载中…</p>
      ) : creds.length === 0 ? (
        <p className="sub">还没有凭据</p>
      ) : (
        <ul className="rows">
          {creds.map((c) => (
            <li key={c.id}>
              <strong>{c.label || c.id}</strong> <span className="badge plain">{c.kind}</span>
              {c.scope && <code>{c.scope}</code>}
              {c.executor && <code>{c.executor}</code>}
              {c.revoked_at ? <span className="badge">已吊销</span> : <button className="danger" onClick={() => revoke(c.id)}>吊销</button>}
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
  return (
    <main>
      <PageHead lede="harness、agent 与 ext 接入 APA 用的令牌和密钥。明文只在签发时显示一次。">凭据</PageHead>
      {secret && (
        <div role="alert" className="secret">
          <p>明文只显示这一次，请立即保存：</p>
          <code>{secret}</code>
          <div>
            <button onClick={() => setSecret(null)}>我已保存</button>
          </div>
        </div>
      )}
      <CredentialSection group={{ path: "tokens", title: "令牌" }} onSecret={setSecret} />
      <CredentialSection group={{ path: "ext-keys", title: "ext 密钥" }} onSecret={setSecret} />
    </main>
  );
}
