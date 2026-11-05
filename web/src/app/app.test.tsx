import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import { ApiError, createClient } from "./api";
import { collectFeatures, type Feature } from "./registry";
import { useSession } from "./session";

// 测试边界替身：按 api/core 的契约（/v1/auth/login、/v1/auth/logout、会话 Cookie + X-CSRF-Token）应答。
function fakeServer() {
  const calls: { method: string; path: string; csrf: string | null }[] = [];
  let sessionValid = false;
  const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  const unauth = () => json(401, { code: "unauthenticated", why: "credential is missing, unknown, revoked or expired", next: "obtain a valid credential in Web" });
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const csrf = (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null;
    calls.push({ method, path: url, csrf });
    if (url === "/v1/auth/login") {
      const pw = JSON.parse(init.body as string).password;
      if (pw !== "correct-horse") return unauth();
      sessionValid = true;
      return json(200, { csrf_token: "tok-1", expires_at: "2030-01-01T00:00:00Z" });
    }
    if (!sessionValid) return unauth();
    if (url === "/v1/auth/logout") {
      if (csrf !== "tok-1") return json(403, { code: "permission_denied", why: "CSRF token is missing or wrong", next: "x" });
      sessionValid = false;
      return json(200, { ok: true });
    }
    return json(200, { projects: [] });
  }) as unknown as typeof fetch;
  return { impl, calls, setValid: () => (sessionValid = true) };
}

const feat = (id: string, title: string, order?: number): Feature => ({ id, title, path: id, order, element: <h2>{title} 页面</h2> });
const FEATURES = [feat("decisions", "决策待办", 1), feat("items", "事项", 2)];

function mount(fetchImpl: typeof fetch, features = FEATURES) {
  return render(
    <MemoryRouter>
      <App features={features} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

async function signIn(pw: string) {
  await userEvent.type(await screen.findByLabelText("口令"), pw);
  await userEvent.click(screen.getByRole("button", { name: "登录" }));
}

beforeEach(() => sessionStorage.clear());

describe("登录与导航框架", () => {
  it("口令错误：显示服务端原因并留在登录页", async () => {
    mount(fakeServer().impl);
    await signIn("wrong-pass");
    expect(await screen.findByRole("alert")).toHaveTextContent("credential is missing");
    expect(screen.queryByRole("navigation")).not.toBeInTheDocument();
  });

  it("登录成功进入外壳：导航来自注册的页面，默认落到第一个页面", async () => {
    mount(fakeServer().impl);
    await signIn("correct-horse");
    const nav = await screen.findByRole("navigation", { name: "主导航" });
    expect(nav).toHaveTextContent("决策待办");
    expect(nav).toHaveTextContent("事项");
    expect(await screen.findByRole("heading", { name: "决策待办 页面" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("link", { name: "事项" }));
    expect(await screen.findByRole("heading", { name: "事项 页面" })).toBeInTheDocument();
    expect(sessionStorage.getItem("apa.csrf")).toBe("tok-1");
  });

  it("刷新后保持会话：csrf 与有效 Cookie 都在则直接进入外壳", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const srv = fakeServer();
    srv.setValid();
    mount(srv.impl);
    expect(await screen.findByRole("navigation", { name: "主导航" })).toBeInTheDocument();
    expect(srv.calls[0]).toMatchObject({ method: "GET", path: "/v1/projects" });
  });

  it("Cookie 已过期：探针 401 回登录页并清掉 csrf", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    mount(fakeServer().impl);
    expect(await screen.findByLabelText("口令")).toBeInTheDocument();
    expect(sessionStorage.getItem("apa.csrf")).toBeNull();
  });

  it("探针网络失败不冒充未登录：显示错误并可重试", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const srv = fakeServer();
    srv.setValid();
    let down = true;
    const impl = ((u: string, i: RequestInit) => (down ? Promise.reject(new TypeError("network down")) : srv.impl(u, i))) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("network down");
    down = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("navigation", { name: "主导航" })).toBeInTheDocument();
  });

  it("退出登录带 CSRF 令牌，成功后回登录页", async () => {
    const srv = fakeServer();
    mount(srv.impl);
    await signIn("correct-horse");
    await userEvent.click(await screen.findByRole("button", { name: "退出登录" }));
    expect(await screen.findByLabelText("口令")).toBeInTheDocument();
    expect(srv.calls.find((c) => c.path === "/v1/auth/logout")?.csrf).toBe("tok-1");
    expect(sessionStorage.getItem("apa.csrf")).toBeNull();
  });

  it("没有注册页面时给出空状态而不是白屏", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const srv = fakeServer();
    srv.setValid();
    mount(srv.impl, []);
    expect(await screen.findByText("暂无可用页面")).toBeInTheDocument();
  });
});

describe("类型化 API 客户端", () => {
  it("写请求带 X-CSRF-Token，读请求不带", async () => {
    const srv = fakeServer();
    srv.setValid();
    const c = createClient({ fetch: srv.impl, getCsrf: () => "tok-1", onUnauthenticated: () => {} });
    await c.get("/items");
    await c.post("/items", { title: "x" });
    expect(srv.calls.map((x) => x.csrf)).toEqual([null, "tok-1"]);
  });

  it("会话中途失效（401）通知外壳；错误体保留 code/why/next", async () => {
    let dropped = 0;
    const c = createClient({ fetch: fakeServer().impl, getCsrf: () => null, onUnauthenticated: () => dropped++ });
    const err = (await c.get("/items").catch((e: unknown) => e)) as ApiError;
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401, code: "unauthenticated" });
    expect(err.body.next).toContain("Web");
    expect(dropped).toBe(1);
  });

  it("登录口令错误的 401 不触发会话失效回调", async () => {
    let dropped = 0;
    const c = createClient({ fetch: fakeServer().impl, getCsrf: () => null, onUnauthenticated: () => dropped++ });
    await expect(c.login("bad")).rejects.toMatchObject({ status: 401 });
    expect(dropped).toBe(0);
  });

  it("非 JSON 错误响应（反向代理错误页）落到 internal 兜底", async () => {
    const impl = (async () => new Response("<html>bad gateway</html>", { status: 502 })) as unknown as typeof fetch;
    const c = createClient({ fetch: impl, getCsrf: () => null, onUnauthenticated: () => {} });
    await expect(c.get("/items")).rejects.toMatchObject({ status: 502, code: "internal" });
  });

  it("useSession 在 Provider 外使用直接报错", () => {
    function Bad() {
      useSession();
      return null;
    }
    expect(() => render(<Bad />)).toThrow("outside SessionProvider");
  });
});

describe("页面注册", () => {
  it("按 order、再按 id 排序，忽略没有 feature 导出的模块", () => {
    const got = collectFeatures({ a: { feature: feat("b", "B") }, b: { feature: feat("a", "A") }, c: {}, d: { feature: feat("z", "Z", 1) } });
    expect(got.map((f) => f.id)).toEqual(["z", "a", "b"]);
  });

  it("重复 id 在启动时报错", () => {
    expect(() => collectFeatures({ a: { feature: feat("x", "X") }, b: { feature: feat("x", "X2") } })).toThrow("duplicate feature id: x");
  });
});
