import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { ApiError, createClient, type ApiClient } from "./api";

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

// 会话 Cookie 是 HttpOnly，由浏览器保持；csrf_token 只在登录响应里给一次，放 sessionStorage 以便刷新后继续写。
// 只有 Cookie 有效且手里有 csrf 才算已登录；缺 csrf 就回登录页重新签发，不让写请求在半路 403。
export function SessionProvider({ children, fetchImpl }: { children: ReactNode; fetchImpl?: typeof fetch }) {
  const [status, setStatus] = useState<Status>("loading");
  const [error, setError] = useState<string | null>(null);
  const csrf = useRef<string | null>(sessionStorage.getItem(KEY));

  const drop = useCallback(() => {
    csrf.current = null;
    sessionStorage.removeItem(KEY);
    setStatus("anon");
  }, []);

  const api = useMemo(
    () => createClient({ fetch: fetchImpl, getCsrf: () => csrf.current, onUnauthenticated: drop }),
    [fetchImpl, drop],
  );

  const boot = useCallback(() => {
    setError(null);
    if (!csrf.current) return setStatus("anon");
    setStatus("loading");
    api.probe().then(
      () => setStatus("authed"),
      (e) => {
        if (e instanceof ApiError && e.status === 401) return; // onUnauthenticated 已处理
        setError(e instanceof Error ? e.message : "request failed");
        setStatus("error");
      },
    );
  }, [api]);

  useEffect(boot, [boot]);

  const login = useCallback(
    async (password: string) => {
      const r = await api.login(password);
      csrf.current = r.csrf_token;
      sessionStorage.setItem(KEY, r.csrf_token);
      setStatus("authed");
    },
    [api],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 401)) throw e;
    }
    drop();
  }, [api, drop]);

  return <Ctx.Provider value={{ status, error, api, login, logout, retry: boot }}>{children}</Ctx.Provider>;
}
