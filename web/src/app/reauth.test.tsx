import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, createClient } from "./api";
import { REAUTH } from "./ids";
import { SessionProvider, useSession } from "./session";

// 3b 再输口令（设计 §14.3）：403 reauth_required → 弹框输口令 → POST /auth/reauth → 原请求重发一次。
const json = (status: number, body: unknown) => new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const need = () => json(403, { code: REAUTH, why: "enter it again", next: "retry", reason: REAUTH });

describe("api：再输口令", () => {
  it("本人输对后原请求只重发一次；取消则照常报错；输错（401）不退出登录", async () => {
    const seen: string[] = [];
    let first = true;
    const f = (async (url: string, init: RequestInit = {}) => {
      seen.push((init.method ?? "GET") + " " + url);
      if (url === "/v1/auth/reauth") return json(401, { code: "unauthenticated", why: "wrong", next: "retry" });
      if (first) return (first = false), need();
      return json(200, { ok: true });
    }) as unknown as typeof fetch;
    const out = vi.fn();
    const ok = createClient({ fetch: f, getCsrf: () => "t", onUnauthenticated: out, onReauth: async () => true });
    expect(await ok.put("/settings/agents/a/tier", {})).toEqual({ ok: true });
    expect(seen).toEqual(["PUT /v1/settings/agents/a/tier", "PUT /v1/settings/agents/a/tier"]);

    first = true;
    const no = createClient({ fetch: f, getCsrf: () => "t", onUnauthenticated: out, onReauth: async () => false });
    await expect(no.put("/settings/agents/a/tier", {})).rejects.toSatisfy((e: unknown) => e instanceof ApiError && e.code === REAUTH && e.body.reason === REAUTH);
    await expect(no.reauth("x")).rejects.toBeInstanceOf(ApiError);
    expect(out).not.toHaveBeenCalled();
  });
});

function Save() {
  const { api } = useSession();
  const [done, setDone] = useState("");
  return <button onClick={() => api.put("/settings/tokens/k", {}).then(() => setDone("saved"), () => setDone("failed"))}>{done || "go"}</button>;
}

beforeEach(() => sessionStorage.clear());

describe("会话：再输口令的弹框", () => {
  it("弹框输入口令后原操作完成，口令只发给 /auth/reauth", async () => {
    sessionStorage.setItem("apa.csrf", "tok-1");
    const bodies: [string, unknown][] = [];
    let fresh = false;
    const f = (async (url: string, init: RequestInit = {}) => {
      if (init.body) bodies.push([url, JSON.parse(init.body as string)]);
      if (url === "/v1/projects") return json(200, { projects: [] });
      if (url === "/v1/auth/reauth") return (fresh = true), json(204, null);
      return fresh ? json(200, {}) : need();
    }) as unknown as typeof fetch;
    render(<SessionProvider fetchImpl={f}><Save /></SessionProvider>);
    await userEvent.click(await screen.findByRole("button", { name: "go" }));
    const dialog = await screen.findByRole("dialog", { name: "再输一次口令" });
    await userEvent.type(dialog.querySelector("input")!, "secret-phrase");
    await userEvent.click(screen.getByRole("button", { name: "确认" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "saved" })).toBeInTheDocument());
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(bodies).toEqual([["/v1/settings/tokens/k", {}], ["/v1/auth/reauth", { password: "secret-phrase" }], ["/v1/settings/tokens/k", {}]]);
  });
});
