import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/channel 的表单契约应答 GET/POST /forms/{令牌}；令牌第一次作答后失效（409），与服务端一次性语义一致。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const dead = () => json(409, { code: "conflict", why: "form link has been used or has expired", next: "answer in the Web UI" });
const FORM = { question: "是否发布 v2？", options: [{ id: "yes", label: "发布" }, { id: "no", label: "暂缓" }], free_text: false };

function server(over: { get?: () => Response; post?: (b: Record<string, string>) => Response | undefined } = {}) {
  let used = false;
  const calls: { method: string; url: string; body?: Record<string, string>; credentials?: RequestCredentials }[] = [];
  const f = vi.fn(async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, url, body, credentials: init.credentials });
    if (url !== "/forms/tok-1") return json(404, { code: "not_found", why: "form link not found", next: "x" });
    if (method === "GET") return used ? dead() : (over.get?.() ?? json(200, FORM));
    const r = over.post?.(body);
    if (r) return r;
    if (used) return dead();
    used = true;
    return json(200, { id: "it-1", replayed: false });
  });
  vi.stubGlobal("fetch", f);
  return calls;
}

const open = (token = "tok-1") =>
  render(
    <MemoryRouter initialEntries={["/forms/" + token]}>
      <App features={[feature]} />
    </MemoryRouter>,
  );

afterEach(() => vi.unstubAllGlobals());

describe("一次性表单页", () => {
  it("无需登录：直接显示问题与选项，且不请求任何 /v1 会话接口", async () => {
    const calls = server();
    open();
    expect(await screen.findByRole("heading", { name: "是否发布 v2？" })).toBeInTheDocument();
    expect(screen.getByLabelText(/发布/)).toBeInTheDocument();
    expect(screen.queryByText(/登录/)).toBeNull();
    expect(calls.map((c) => c.url)).toEqual(["/forms/tok-1"]);
    expect(calls[0].credentials).toBe("omit");
  });

  it("选择后提交，页面显示已提交；再次打开同一链接明确失效", async () => {
    const calls = server();
    const u = userEvent.setup();
    const first = open();
    await u.click(await screen.findByLabelText(/暂缓/));
    await u.type(screen.getByLabelText(/理由/), "再观察");
    await u.click(screen.getByRole("button", { name: "提交" }));
    expect(await screen.findByText(/已记录/)).toBeInTheDocument();
    expect(calls.at(-1)).toMatchObject({ method: "POST", url: "/forms/tok-1", body: { choice: "no", reason: "再观察" } });
    first.unmount();

    open();
    expect(await screen.findByRole("heading", { name: "链接已失效" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "登录网页去作答" })).toHaveAttribute("href", "/decisions"); // 失效后给出下一步
    expect(screen.queryByRole("button", { name: "提交" })).toBeNull();
  });

  it("过期或不存在的链接明确失效", async () => {
    server({ get: dead });
    open();
    expect(await screen.findByRole("heading", { name: "链接已失效" })).toBeInTheDocument();
    open("nope");
    expect(await screen.findAllByRole("heading", { name: "链接已失效" })).not.toHaveLength(0);
  });

  it("提交时链接被他处用掉（409）：转为失效页，不假装成功", async () => {
    server({ post: () => dead() });
    const u = userEvent.setup();
    open();
    await u.click(await screen.findByLabelText(/发布/));
    await u.click(screen.getByRole("button", { name: "提交" }));
    expect(await screen.findByRole("heading", { name: "链接已失效" })).toBeInTheDocument();
    expect(screen.queryByText(/已记录/)).toBeNull();
  });

  it("web_only 被拒：给出 Web 链接", async () => {
    server({ post: () => json(403, { code: "web_only", why: "this decision is web_only", next: "answer in the Web UI", web_url: "https://apa.local/items/it-1" }) });
    const u = userEvent.setup();
    open();
    await u.click(await screen.findByLabelText(/发布/));
    await u.click(screen.getByRole("button", { name: "提交" }));
    expect(await screen.findByRole("link", { name: "登录网页去作答" })).toHaveAttribute("href", "https://apa.local/items/it-1");
  });

  it("校验类失败：显示错误，表单保留可再提交", async () => {
    let n = 0;
    server({ post: () => (n++ === 0 ? json(400, { code: "invalid_request", why: "unknown choice", next: "pick a listed option" }) : undefined) });
    const u = userEvent.setup();
    open();
    await u.click(await screen.findByLabelText(/发布/));
    await u.click(screen.getByRole("button", { name: "提交" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("unknown choice");
    await u.click(screen.getByRole("button", { name: "提交" }));
    expect(await screen.findByText(/已记录/)).toBeInTheDocument();
  });

  it("free_text 决策可只填文字提交；未选未填时不可提交", async () => {
    const calls = server({ get: () => json(200, { question: "怎么处理？", options: [], free_text: true }) });
    const u = userEvent.setup();
    open();
    const btn = await screen.findByRole("button", { name: "提交" });
    expect(btn).toBeDisabled();
    await u.type(screen.getByLabelText(/补充说明/), "  回滚  ");
    await u.click(btn);
    expect(await screen.findByText(/已记录/)).toBeInTheDocument();
    expect(calls.at(-1)?.body).toEqual({ text: "回滚" });
  });

  it("加载失败（非 JSON 的 5xx）：显示错误与重试，不当作失效", async () => {
    server({ get: () => new Response("bad gateway", { status: 502 }) });
    open();
    expect(await screen.findByRole("alert")).toHaveTextContent("HTTP 502");
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });
});

describe("与 API 同路径", () => {
  it("页面的 fetch 声明 Accept: application/json，使同一路径的浏览器导航才能拿到页面", async () => {
    const f = vi.fn(async () => json(200, FORM));
    vi.stubGlobal("fetch", f);
    open();
    await screen.findByRole("heading", { name: "是否发布 v2？" });
    const init = (f.mock.calls[0] as unknown as [string, RequestInit])[1];
    expect((init.headers as Record<string, string>).Accept).toBe("application/json");
  });
});
