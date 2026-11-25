import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ApiError, createClient, type ApiClient } from "./api";
import { Reauth } from "./Reauth";

type Status = "loading" | "anon" | "authed" | "error";

interface Session {
  status: Status;
  error: string | null;
  api: ApiClient;
  login: (password: string) => Promise<void>;
  logout: () => Promise<void>;
  retry: () => void;
}

const KEY = "apa.csrf";
const Ctx = createContext<Session | null>(null);

export function useSession(): Session {
  const s = useContext(Ctx);
  if (!s) throw new Error("useSession outside SessionProvider");
  return s;
}

// 会话 Cookie 是 HttpOnly，由浏览器保持且各标签页共享；csrf_token 按页存 sessionStorage，会因别处重新登录而过时，
// 所以打开页面时与写请求返回 csrf_invalid 时都向服务端取当前会话的令牌。
export function SessionProvider({ children, fetchImpl }: { children: ReactNode; fetchImpl?: typeof fetch }) {
  const [status, setStatus] = useState<Status>("loading");
  const [error, setError] = useState<string | null>(null);
  const csrf = useRef<string | null>(sessionStorage.getItem(KEY));

  const remember = useCallback((token: string | null) => {
    csrf.current = token;
    if (token) sessionStorage.setItem(KEY, token);
    else sessionStorage.removeItem(KEY);
  }, []);

  const drop = useCallback(() => {
    remember(null);
    setStatus("anon");
  }, [remember]);

  // 3b 动作要再输口令时弹框；同时撞上的几条请求共用一次输入，结果一起交回。
  const waiting = useRef<((ok: boolean) => void)[]>([]);
  const [asking, setAsking] = useState(false);
  const onReauth = useCallback(() => new Promise<boolean>((resolve) => {
    waiting.current.push(resolve);
    setAsking(true);
  }), []);
  const answered = useCallback((ok: boolean) => {
    waiting.current.splice(0).forEach((f) => f(ok));
    setAsking(false);
  }, []);

  const api: ApiClient = useMemo(() => {
    // 重新同步不了（会话没了、网络或服务端出错）就回登录页。
    const onCsrfInvalid = async () => {
      try {
        const token = (await client.session()).csrf_token;
        if (token) {
          remember(token);
          return true;
        }
      } catch {
        /* 落到下面回登录页 */
      }
      drop();
      return false;
    };
    const client: ApiClient = createClient({ fetch: fetchImpl, getCsrf: () => csrf.current, onUnauthenticated: drop, onReauth, onCsrfInvalid });
    return client;
  }, [fetchImpl, remember, drop, onReauth]);

  const boot = useCallback(() => {
    setError(null);
    setStatus("loading");
    api.session().then(
      (r) => {
        if (r.csrf_token) remember(r.csrf_token);
        setStatus(csrf.current ? "authed" : "anon");
      },
      (e) => {
        if (e instanceof ApiError && e.status === 401) return; // onUnauthenticated 已处理
        setError(e instanceof Error ? e.message : "request failed");
        setStatus("error");
      },
    );
  }, [api, remember]);

  useEffect(boot, [boot]);

  const login = useCallback(
    async (password: string) => {
      const r = await api.login(password);
      remember(r.csrf_token);
      setStatus("authed");
    },
    [api, remember],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 401)) throw e;
    }
    drop();
  }, [api, drop]);

  return (
    <Ctx.Provider value={{ status, error, api, login, logout, retry: boot }}>
      {children}
      {asking && <Reauth api={api} done={answered} />}
    </Ctx.Provider>
  );
}
