// 类型化 API 客户端：只调 HTTP API（/v1），不含业务规则。错误体见设计附录 B.3。
import { CSRF_INVALID, REAUTH } from "./ids";

export interface ApiErrorBody {
  code: string;
  why: string;
  next: string;
  allowed?: string[];
  web_url?: string | null;
  /** 同一个码下更细的稳定原因（拒绝的三种口径 hard_line / tier_closed / out_of_scope 等，设计 §14.3）。 */
  reason?: string;
}

export class ApiError extends Error {
  constructor(public status: number, public body: ApiErrorBody) {
    super(body.why);
  }
  get code() {
    return this.body.code;
  }
}

export interface LoginResult {
  csrf_token: string;
  expires_at: string;
}

export interface ClientOptions {
  fetch?: typeof fetch;
  getCsrf: () => string | null;
  /** 任何 401 都会触发：会话失效，调用方应回到登录页。 */
  onUnauthenticated: () => void;
  /** 3b 动作要再输一次口令（403 reauth_required）时调用：本人输对了返回 true，原请求重发一次；取消返回 false，照常报错。 */
  onReauth?: () => Promise<boolean>;
  /** 403 csrf_invalid（手里的令牌过时，请求没有执行）时调用：重新同步成功返回 true，原请求重发一次。 */
  onCsrfInvalid?: () => Promise<boolean>;
}

const SAFE = new Set(["GET", "HEAD", "OPTIONS"]);

export function createClient(o: ClientOptions) {
  const f = o.fetch ?? ((...a: Parameters<typeof fetch>) => fetch(...a));

  async function request<T>(method: string, path: string, body?: unknown, extra?: Record<string, string>, tried: { reauth?: boolean; csrf?: boolean } = {}): Promise<T> {
    const headers: Record<string, string> = { ...extra };
    const text = typeof body === "string";
    if (body !== undefined && !text) headers["Content-Type"] = "application/json";
    if (!SAFE.has(method)) {
      const t = o.getCsrf();
      if (t) headers["X-CSRF-Token"] = t;
    }
    const res = await f("/v1" + path, {
      method,
      headers,
      credentials: "same-origin",
      body: body === undefined ? undefined : text ? body : JSON.stringify(body),
    });
    if (!res.ok) {
      const err = new ApiError(res.status, await readError(res));
      // 再输口令输错也是 401，但会话仍然有效，不能因此退出登录。
      if (res.status === 401 && path !== "/auth/login" && path !== "/auth/reauth") o.onUnauthenticated();
      if (!tried.reauth && res.status === 403 && err.code === REAUTH && o.onReauth && (await o.onReauth())) return request<T>(method, path, body, extra, { ...tried, reauth: true });
      if (!tried.csrf && res.status === 403 && err.body.reason === CSRF_INVALID && o.onCsrfInvalid && (await o.onCsrfInvalid())) return request<T>(method, path, body, extra, { ...tried, csrf: true });
      throw err;
    }
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }

  return {
    get: <T>(path: string) => request<T>("GET", path),
    /** headers 用于 If-Match / Idempotency-Key 等请求头。 */
    post: <T>(path: string, body?: unknown, headers?: Record<string, string>) => request<T>("POST", path, body ?? {}, headers),
    patch: <T>(path: string, body: unknown, headers?: Record<string, string>) => request<T>("PATCH", path, body, headers),
    put: <T>(path: string, body: unknown, headers?: Record<string, string>) => request<T>("PUT", path, body, headers),
    del: <T>(path: string) => request<T>("DELETE", path),
    login: (password: string) => request<LoginResult>("POST", "/auth/login", { password: password }),
    logout: () => request<{ ok: boolean }>("POST", "/auth/logout", {}),
    /** 3b 动作前再输一次口令（POST /auth/reauth，204）。 */
    reauth: (password: string) => request<void>("POST", "/auth/reauth", { password }),
    /** 当前会话的 CSRF 令牌；Cookie 无效为 401。 */
    session: () => request<{ csrf_token?: string }>("GET", "/auth/session"),
  };
}

export type ApiClient = ReturnType<typeof createClient>;

async function readError(res: Response): Promise<ApiErrorBody> {
  try {
    const b = await res.json();
    if (b && typeof b.code === "string") return b as ApiErrorBody;
  } catch {
    /* 非 JSON 响应（如反向代理的错误页）落到下面的兜底 */
  }
  return { code: "internal", why: `unexpected response (HTTP ${res.status})`, next: "retry; check the server log" };
}
