import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按附录 B.2 的契约应答（登录发 csrf_token；写请求带 CSRF 与 If-Match）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
type Call = { method: string; path: string; body?: any; csrf: string | null; ifMatch: string | null };

function server(o: { phase?: string; conflict?: boolean } = {}) {
  const calls: Call[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const h = (init.headers ?? {}) as Record<string, string>;
    calls.push({ method, path: url, body: init.body ? JSON.parse(init.body as string) : undefined, csrf: h["X-CSRF-Token"] ?? null, ifMatch: h["If-Match"] ?? null });
    if (url === "/v1/auth/login") return json(200, { csrf_token: "tok-1", expires_at: "2099-01-01T00:00:00Z" });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items/it-1" && method === "GET") return json(200, { item: { id: "it-1", title: "写季度报告", phase: o.phase ?? "open", version: 7, kind: "task" }, relations: [] });
    if (url === "/v1/items/gone") return json(404, { code: "not_found", why: "item not found", next: "check the id" });
    if (method !== "GET" && o.conflict) return json(409, { code: "conflict", why: "item changed", next: "reload and retry" });
    if (url === "/v1/items/it-1/transition") return json(200, { item: { id: "it-1", phase: "done", version: 8 } });
    if (url === "/v1/items/it-1/progress") return json(201, { text: "x" });
    if (url === "/v1/items/it-1" && method === "PATCH") return json(200, { item: { id: "it-1", version: 8 } });
    return json(404, { code: "not_found", why: "no such route", next: "-" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(s: ReturnType<typeof server>, at: string, authed = true) {
  if (authed) sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[feature]} fetchImpl={s.impl} />
    </MemoryRouter>,
  );
}
const writes = (s: ReturnType<typeof server>) => s.calls.filter((c) => c.method !== "GET" && c.path !== "/v1/auth/login");

beforeEach(() => sessionStorage.clear());

describe("动作链接与确认页（flow-export-ext / ACT-EXP-16、17）", () => {
  it("未登录打开：先出现登录页且不读不写；登录后直接到该事项的完成确认页，点确认前阶段不变", async () => {
    const s = server();
    mount(s, "/act/it-1?do=done", false);
    await userEvent.type(await screen.findByLabelText("口令"), "pw");
    expect(s.calls.filter((c) => c.path.startsWith("/v1/items"))).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "登录" }));
    expect(await screen.findByRole("heading", { name: "完成事项" })).toBeInTheDocument();
    expect(screen.getByText("写季度报告")).toBeInTheDocument();
    expect(screen.getByText("阶段：open")).toBeInTheDocument();
    expect(writes(s)).toHaveLength(0);
  });

  it("完成：确认后 POST transition {to:done} 带 CSRF 与 If-Match，并显示结果", async () => {
    const s = server();
    mount(s, "/act/it-1?do=done");
    await userEvent.click(await screen.findByRole("button", { name: "确认完成" }));
    expect(await screen.findByRole("status")).toHaveTextContent("已完成");
    const [w, ...rest] = writes(s);
    expect(rest).toHaveLength(0);
    expect(w).toMatchObject({ method: "POST", path: "/v1/items/it-1/transition", csrf: "tok-1", ifMatch: "7" });
    expect(w.body.to).toBe("done");
    expect(screen.queryByRole("button", { name: "确认完成" })).not.toBeInTheDocument();
  });

  it("已完成的事项：说明已完成，不提供确认按钮", async () => {
    const s = server({ phase: "done" });
    mount(s, "/act/it-1?do=done");
    expect(await screen.findByText("该事项已经是完成状态")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "确认完成" })).not.toBeInTheDocument();
  });

  it("写进展：POST progress {text,on}，空文本不提交", async () => {
    const s = server();
    mount(s, "/act/it-1?do=progress");
    const submit = await screen.findByRole("button", { name: "保存进展" });
    expect(submit).toBeDisabled();
    await userEvent.type(screen.getByLabelText("进展"), "初稿完成");
    await userEvent.type(screen.getByLabelText("日期（可选）"), "2026-11-12");
    await userEvent.click(submit);
    expect(await screen.findByRole("status")).toHaveTextContent("已记录进展");
    expect(writes(s)).toHaveLength(1);
    expect(writes(s)[0]).toMatchObject({ method: "POST", path: "/v1/items/it-1/progress", csrf: "tok-1", body: { text: "初稿完成", on: "2026-11-12" } });
  });

  it("改计划日期：PATCH planned_for 带 If-Match", async () => {
    const s = server();
    mount(s, "/act/it-1?do=plan");
    await userEvent.type(await screen.findByLabelText("计划日期"), "2026-12-05");
    await userEvent.click(screen.getByRole("button", { name: "保存计划日期" }));
    expect(await screen.findByRole("status")).toHaveTextContent("已改计划日期");
    expect(writes(s)[0]).toMatchObject({ method: "PATCH", path: "/v1/items/it-1", ifMatch: "7", body: { planned_for: "2026-12-05" } });
  });

  it("服务端拒绝（版本冲突）如实显示原因，不显示成功", async () => {
    const s = server({ conflict: true });
    mount(s, "/act/it-1?do=done");
    await userEvent.click(await screen.findByRole("button", { name: "确认完成" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("item changed");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("未知动作或事项不存在：显示错误且不发写请求", async () => {
    const a = server();
    const { unmount } = mount(a, "/act/it-1?do=delete");
    expect(await screen.findByRole("alert")).toHaveTextContent("不支持的动作");
    expect(writes(a)).toHaveLength(0);
    unmount();
    const b = server();
    mount(b, "/act/gone?do=done");
    expect(await screen.findByRole("alert")).toHaveTextContent("item not found");
    expect(within(document.body).queryByRole("button", { name: "确认完成" })).not.toBeInTheDocument();
  });

  it("确认页不进导航", async () => {
    const s = server();
    mount(s, "/act/it-1?do=done");
    await screen.findByRole("heading", { name: "完成事项" });
    expect(within(screen.getByRole("navigation", { name: "主导航" })).queryByRole("link")).toBeNull();
  });
});
