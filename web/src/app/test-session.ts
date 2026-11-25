// 测试边界替身：给只模拟业务路由的假服务端补上 GET /v1/auth/session——会话是否有效沿用它对 /v1/projects 的应答，
// 令牌取用例预置的那个。要验证会话与 CSRF 行为的用例（app.test、session.test）自己模拟这条路由。
const wrapped = new WeakMap<typeof fetch, typeof fetch>(); // 同一个替身只包一次：fetchImpl 换了身份会重建 API 客户端

export function withSession(f: typeof fetch): typeof fetch {
  let w = wrapped.get(f);
  if (!w) {
    w = (async (url: string, init?: RequestInit) => {
      if (url !== "/v1/auth/session") return f(url, init);
      const token = sessionStorage.getItem("apa.csrf");
      if (!token) return new Response(JSON.stringify({ code: "unauthenticated", why: "no session", next: "log in" }), { status: 401 });
      const r = await f("/v1/projects", init);
      return r.ok ? new Response(JSON.stringify({ csrf_token: token })) : r;
    }) as typeof fetch;
    wrapped.set(f, w);
  }
  return w;
}
