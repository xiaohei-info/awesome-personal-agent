import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按附录 B 认证节的契约应答（api/core/session.go）：
// 改口令 {current,new}，新口令<12 返回 400，当前口令错误返回 403，成功后其他会话失效；会话可逐个或全部吊销。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface Sess { id: string; created_at: string; last_used_at: string; addr: string; agent: string; current: boolean }
const sess = (id: string, over: Partial<Sess> = {}): Sess => ({ id, created_at: "2026-01-01T00:00:00Z", last_used_at: "2026-01-02T03:04:05Z", addr: "198.51.100." + id.length, agent: "Browser-" + id, current: false, ...over });

function server(initial: Sess[], password = "correct-horse-battery") {
  let sessions = initial;
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (sessions.length === 0) return json(401, { code: "unauthenticated", why: "session expired", next: "log in again" }); // 全部吊销后 Cookie 失效
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/auth/sessions" && method === "GET") return json(200, { sessions });
    if (url === "/v1/auth/sessions" && method === "DELETE") {
      const n = sessions.length;
      sessions = [];
      return json(200, { ok: true, revoked: n });
    }
    const one = url.match(/^\/v1\/auth\/sessions\/(.+)$/);
    if (one && method === "DELETE") {
      const id = decodeURIComponent(one[1]);
      if (!sessions.some((s) => s.id === id)) return json(404, { code: "not_found", why: "session not found", next: "list sessions and retry" });
      sessions = sessions.filter((s) => s.id !== id);
      return json(200, { ok: true, revoked: 1 });
    }
    if (url === "/v1/auth/password" && method === "POST") {
      if (body.new.length < 12) return json(400, { code: "invalid_request", why: "the new password must be at least 12 characters", next: "choose a longer one" });
      if (body.current !== password) return json(403, { code: "permission_denied", why: "the current password is wrong", next: "enter the current login password" });
      sessions = sessions.filter((s) => s.current);
      return json(200, { ok: true });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/settings-security"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const sessionsRegion = () => screen.getByRole("region", { name: "在线会话" });
const passwordRegion = () => screen.getByRole("region", { name: "修改口令" });
const base = [sess("me", { current: true, agent: "Mine" }), sess("other", { agent: "Phone" }), sess("third", { agent: "Tablet" })];

beforeEach(() => sessionStorage.clear());

describe("口令与在线会话（flow-web-login）", () => {
  it("ACT-LGN-02：改口令需当前口令与不少于 12 个字符；成功后其他会话从列表消失", async () => {
    const s = server(base);
    mount(s.impl);
    const user = userEvent.setup();
    await screen.findByText("Phone");

    const form = within(passwordRegion());
    await user.type(form.getByLabelText("当前口令"), "correct-horse-battery");
    await user.type(form.getByLabelText("新口令"), "a-brand-new-secret");
    await user.click(form.getByRole("button", { name: "修改口令" }));

    await screen.findByText("口令已修改，其他会话已失效");
    const post = s.calls.find((c) => c.method === "POST" && c.path === "/v1/auth/password")!;
    expect(post.body).toEqual({ current: "correct-horse-battery", new: "a-brand-new-secret" });
    expect(post.csrf).toBe("tok-1");
    await waitFor(() => expect(within(sessionsRegion()).queryByText("Phone")).toBeNull());
    expect(within(sessionsRegion()).getByText("Mine")).toBeInTheDocument();
    expect(form.getByLabelText("新口令")).toHaveValue(""); // 口令不留在页面上
  });

  it("ACT-LGN-02：新口令少于 12 个字符不发请求；服务端拒绝（当前口令错误）如实显示", async () => {
    const s = server(base);
    mount(s.impl);
    const user = userEvent.setup();
    await screen.findByText("Phone");
    const form = within(passwordRegion());

    await user.type(form.getByLabelText("当前口令"), "correct-horse-battery");
    await user.type(form.getByLabelText("新口令"), "short");
    await user.click(form.getByRole("button", { name: "修改口令" }));
    expect(await form.findByRole("alert")).toHaveTextContent("至少 12 个字符");
    expect(s.calls.some((c) => c.path === "/v1/auth/password")).toBe(false);

    await user.clear(form.getByLabelText("当前口令"));
    await user.type(form.getByLabelText("当前口令"), "wrong-current-pass");
    await user.clear(form.getByLabelText("新口令"));
    await user.type(form.getByLabelText("新口令"), "a-brand-new-secret");
    await user.click(form.getByRole("button", { name: "修改口令" }));
    expect(await form.findByRole("alert")).toHaveTextContent("the current password is wrong");
    expect(within(sessionsRegion()).getByText("Phone")).toBeInTheDocument(); // 失败不影响会话
  });

  it("ACT-LGN-07：列出最近使用时间、来源地址、浏览器标识，标出当前会话", async () => {
    mount(server(base).impl);
    const rows = within(await screen.findByRole("region", { name: "在线会话" }));
    await rows.findByText("Phone");
    expect(rows.getAllByRole("listitem")).toHaveLength(3);
    const mine = rows.getByText("Mine").closest("li")!;
    expect(within(mine).getByText("当前会话")).toBeInTheDocument();
    expect(within(mine).getByText("198.51.100.2")).toBeInTheDocument();
    expect(within(mine).getByText(/2026-01-02/)).toBeInTheDocument();
  });

  it("ACT-LGN-07：逐个吊销只移除该会话；全部吊销清空并回到登录页", async () => {
    const s = server(base);
    mount(s.impl);
    const user = userEvent.setup();
    const rows = within(await screen.findByRole("region", { name: "在线会话" }));
    await rows.findByText("Phone");

    await user.click(within(rows.getByText("Phone").closest("li")!).getByRole("button", { name: "吊销" }));
    await waitFor(() => expect(rows.queryByText("Phone")).toBeNull());
    expect(rows.getByText("Tablet")).toBeInTheDocument();
    expect(s.calls.some((c) => c.method === "DELETE" && c.path === "/v1/auth/sessions/other" && c.csrf === "tok-1")).toBe(true);

    await user.click(rows.getByRole("button", { name: "吊销全部会话" }));
    await waitFor(() => expect(s.calls.some((c) => c.method === "DELETE" && c.path === "/v1/auth/sessions")).toBe(true));
    // 全部吊销包含当前会话：之后的任何请求都会 401，页面须回到登录
    await waitFor(() => expect(sessionStorage.getItem("apa.csrf")).toBeNull());
  });

  it("ACT-LGN-07：吊销失败（会话已不存在）显示服务端原因并刷新列表", async () => {
    const s = server([sess("me", { current: true, agent: "Mine" }), sess("gone", { agent: "Ghost" })]);
    mount(s.impl);
    const user = userEvent.setup();
    const rows = within(await screen.findByRole("region", { name: "在线会话" }));
    await rows.findByText("Ghost");
    // 另一处先把它吊销了
    await fetchDelete(s.impl, "/v1/auth/sessions/gone");
    await user.click(within(rows.getByText("Ghost").closest("li")!).getByRole("button", { name: "吊销" }));
    expect(await rows.findByRole("alert")).toHaveTextContent("session not found");
    await waitFor(() => expect(rows.queryByText("Ghost")).toBeNull());
  });
});

const fetchDelete = (f: typeof fetch, path: string) => f(path, { method: "DELETE" });
