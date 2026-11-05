// 类型化 API 客户端：只调 HTTP API（/v1），不含业务规则。错误体见设计附录 B.3。
export interface ApiErrorBody {
  code: string;
  why: string;
  next: string;
  allowed?: string[];
  web_url?: string | null;
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
}

const SAFE = new Set(["GET", "HEAD", "OPTIONS"]);

export function createClient(o: ClientOptions) {
  const f = o.fetch ?? ((...a: Parameters<typeof fetch>) => fetch(...a));

  async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = {};
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (!SAFE.has(method)) {
      const t = o.getCsrf();
      if (t) headers["X-CSRF-Token"] = t;
    }
    const res = await f("/v1" + path, {
      method,
      headers,
      credentials: "same-origin",
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!res.ok) {
      const err = new ApiError(res.status, await readError(res));
      if (res.status === 401 && path !== "/auth/login") o.onUnauthenticated();
      throw err;
    }
    return (await res.json()) as T;
  }

  return {
    get: <T>(path: string) => request<T>("GET", path),
    post: <T>(path: string, body?: unknown) => request<T>("POST", path, body ?? {}),
    patch: <T>(path: string, body: unknown) => request<T>("PATCH", path, body),
    del: <T>(path: string) => request<T>("DELETE", path),
    login: (password: string) => request<LoginResult>("POST", "/auth/login", { password: password }),
    logout: () => request<{ ok: boolean }>("POST", "/auth/logout", {}),
    /** 会话探针：用已有的只读路由判断 Cookie 是否仍有效。 */
    probe: () => request<unknown>("GET", "/projects"),
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
