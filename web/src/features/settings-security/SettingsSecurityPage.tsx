import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../app/api";
import { useSession } from "../../app/session";
import { PageHead } from "../../app/page";

// 对应 /v1/auth/password 与 /v1/auth/sessions（附录 B 认证节）。长度下限、当前口令校验、
// 限速和"改口令后其他会话失效"都在服务端；本页只做 12 字符的即时提示并如实展示服务端的回答。
const MIN_LEN = 12;

interface SessionInfo {
  id: string;
  created_at: string;
  last_used_at: string;
  addr: string;
  agent: string;
  current: boolean;
}

const msg = (e: unknown) => (e instanceof ApiError ? `${e.body.why}（${e.body.next}）` : e instanceof Error ? e.message : "request failed");

function PasswordSection({ onChanged }: { onChanged: () => void }) {
  const { api } = useSession();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    setDone(false);
    if ([...next].length < MIN_LEN) return setError(`新口令至少 ${MIN_LEN} 个字符`);
    setBusy(true);
    setError(null);
    try {
      await api.post("/auth/password", { current, new: next });
      setCurrent("");
      setNext("");
      setDone(true);
      onChanged();
    } catch (e) {
      setError(msg(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section role="region" aria-labelledby="heading-password">
      <h2 id="heading-password">修改口令</h2>
      {error && <div role="alert">{error}</div>}
      {done && <p role="status">口令已修改，其他会话已失效</p>}
      <form
        className="panel"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <label>
          当前口令
          <input type="password" autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
        </label>
        <label>
          新口令
          <input type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />
        </label>
        <div>
          <button type="submit" disabled={busy || !current || !next}>修改口令</button>
        </div>
      </form>
    </section>
  );
}

function SessionsSection({ reloadKey }: { reloadKey: number }) {
  const { api } = useSession();
  const [list, setList] = useState<SessionInfo[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    () =>
      api.get<{ sessions: SessionInfo[] }>("/auth/sessions").then(
        (r) => setList(r.sessions),
        (e) => setError(msg(e)),
      ),
    [api],
  );

  useEffect(() => {
    load();
  }, [load, reloadKey]);

  const revoke = async (path: string) => {
    setError(null);
    try {
      await api.del(path);
    } catch (e) {
      setError(msg(e));
    }
    await load(); // 以服务端状态为准；吊销了当前会话时 401 会把页面带回登录
  };

  return (
    <section role="region" aria-labelledby="heading-sessions">
      <h2 id="heading-sessions">在线会话</h2>
      {error && <div role="alert">{error}</div>}
      {list === null ? (
        !error && <p role="status">加载中…</p>
      ) : list.length === 0 ? (
        <p className="sub">没有在线会话</p>
      ) : (
        <>
          <ul className="rows">
            {list.map((s) => (
              <li key={s.id}>
                <strong>{s.agent || "未知浏览器"}</strong> <code>{s.addr}</code> <span className="sub">最近使用 {s.last_used_at.replace("T", " ").replace(/Z$/, " UTC")}</span>
                {s.current && <span className="badge">当前会话</span>}
                <button className="danger" onClick={() => revoke("/auth/sessions/" + encodeURIComponent(s.id))}>吊销</button>
              </li>
            ))}
          </ul>
          <button className="danger" onClick={() => revoke("/auth/sessions")}>吊销全部会话</button>
        </>
      )}
    </section>
  );
}

export function SettingsSecurityPage() {
  const [reloadKey, setReloadKey] = useState(0);
  return (
    <main>
      <PageHead lede="修改登录口令，查看并吊销在线会话。改口令后，除当前会话外的其他会话全部失效。">登录安全</PageHead>
      <PasswordSection onChanged={() => setReloadKey((k) => k + 1)} />
      <SessionsSection reloadKey={reloadKey} />
    </main>
  );
}
