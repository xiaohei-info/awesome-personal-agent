import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { beforeEach, describe, expect, it } from "vitest";
import { CSRF_INVALID } from "./ids";
import { SessionProvider, useSession } from "./session";

// 会话与 CSRF 失步（设计 §14.13）：Cookie 全标签页共享，令牌各页各存；过时的页面向 GET /auth/session 重新同步。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const stale = () => json(403, { code: "permission_denied", why: "CSRF token is missing or wrong", next: "x", reason: CSRF_INVALID });

// current 是服务端当前会话的令牌；null 表示 Cookie 已失效。
function server() {
  const s = { current: "tok-1" as string | null, calls: [] as string[], impl: undefined as unknown as typeof fetch };
  s.impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const csrf = (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? "";
    s.calls.push(`${method} ${url} ${csrf}`.trim());
    if (!s.current) return json(401, { code: "unauthenticated", why: "expired", next: "log in" });
    if (url === "/v1/auth/session") return json(200, { csrf_token: s.current });
    return csrf === s.current ? json(200, {}) : stale();
  }) as typeof fetch;
  return s;
}

function Page() {
  const { api, status } = useSession();
  const [done, setDone] = useState("go");
  if (status !== "authed") return <p>{status}</p>;
  return <button onClick={() => api.post("/items/a/dismiss", {}).then(() => setDone("saved"), () => setDone("failed"))}>{done}</button>;
}

const mount = (f: typeof fetch) => render(<SessionProvider fetchImpl={f}><Page /></SessionProvider>);

beforeEach(() => sessionStorage.clear());

describe("会话与 CSRF 同步", () => {
  it("打开页面时同步：新标签页（无令牌）与过时令牌都换成当前会话的", async () => {
    for (const old of [null, "tok-old"]) {
      sessionStorage.clear();
      if (old) sessionStorage.setItem("apa.csrf", old);
      const { unmount } = mount(server().impl);
      await screen.findByRole("button", { name: "go" });
      expect(sessionStorage.getItem("apa.csrf")).toBe("tok-1");
      unmount();
    }
  });

  it("写请求 403 csrf_invalid：重新同步后原操作重发一次并成功", async () => {
    const s = server();
    mount(s.impl);
    const go = await screen.findByRole("button", { name: "go" });
    s.current = "tok-2"; // 别的标签页重新登录
    await userEvent.click(go);
    await screen.findByRole("button", { name: "saved" });
    expect(s.calls.slice(1)).toEqual(["POST /v1/items/a/dismiss tok-1", "GET /v1/auth/session", "POST /v1/items/a/dismiss tok-2"]);
    expect(sessionStorage.getItem("apa.csrf")).toBe("tok-2");
  });

  it("同步不了（会话已失效）：清掉令牌回登录页", async () => {
    const s = server();
    const impl = ((u: string, i: RequestInit) => (i.method === "POST" ? ((s.current = null), Promise.resolve(stale())) : s.impl(u, i))) as typeof fetch;
    mount(impl);
    await userEvent.click(await screen.findByRole("button", { name: "go" }));
    await screen.findByText("anon");
    expect(sessionStorage.getItem("apa.csrf")).toBeNull();
  });

  it("重发仍被拒：只重发一次，错误交给调用方", async () => {
    const s = server();
    const impl = ((u: string, i: RequestInit) => (i.method === "POST" ? (s.calls.push("POST"), Promise.resolve(stale())) : s.impl(u, i))) as typeof fetch;
    mount(impl);
    await userEvent.click(await screen.findByRole("button", { name: "go" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "failed" })).toBeInTheDocument());
    expect(s.calls.filter((c) => c === "POST")).toHaveLength(2);
  });
});
