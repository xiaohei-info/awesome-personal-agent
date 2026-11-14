import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import type { UnreadDelivery } from "./UnreadPage";

// 测试边界替身：按 api/deliveries 的契约应答（GET /deliveries/unread、POST /deliveries/{id}/read，错误体见附录 B.3）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function d(id: string, over: Partial<UnreadDelivery> = {}): UnreadDelivery {
  return { id, item: "it-" + id, class: "decision", version: 1, state: "fallback_web", content: { title: "投递 " + id }, ...over };
}

function server(initial: UnreadDelivery[], read?: (id: string) => Response) {
  let list = initial;
  const calls: { method: string; path: string; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    calls.push({ method, path: url, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/deliveries/unread") return json(200, { deliveries: list });
    const m = url.match(/^\/v1\/deliveries\/(.+)\/read$/);
    if (m && method === "POST") {
      const res = read?.(m[1]);
      if (res) return res;
      list = list.filter((x) => x.id !== m[1]);
      return json(200, { id: m[1] });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/unread"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

describe("Web 未读页（flow-hermes-context / ACT-HCTX-06）", () => {
  it("展示未读投递的标题、摘要、选项与链接；打开页面本身不确认任何投递", async () => {
    const s = server([d("a", { content: { title: "续费确认", summary: "到期前处理", options: ["同意", "拒绝"], web_url: "/decisions" } })]);
    mount(s.impl);
    const card = (await screen.findByRole("article", { name: "续费确认" }));
    expect(within(card).getByText("到期前处理")).toBeInTheDocument();
    expect(within(card).getByText(/同意 \/ 拒绝/)).toBeInTheDocument();
    expect(within(card).getByRole("link", { name: "打开" })).toHaveAttribute("href", "/decisions");
    expect(s.calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("已被注入的投递仍然可见，并说明注入不算确认，且不会因此发出确认请求", async () => {
    const s = server([d("a", { injections: [{ injected_to_session: "sess-1", at: "2030-01-01T00:00:00Z" }, { injected_to_session: "sess-2", at: "2030-01-01T00:01:00Z" }] })]);
    mount(s.impl);
    const card = await screen.findByRole("article", { name: "投递 a" });
    expect(within(card).getByText(/已在 2 次聊天里提过，仍未确认/)).toBeInTheDocument();
    expect(s.calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("被日上限或暂停提醒压下的投递标出原因，没被压的不标", async () => {
    mount(server([d("a", { suppressed: "daily_cap" }), d("b", { suppressed: "muted" }), d("c")]).impl);
    expect(within(await screen.findByRole("article", { name: "投递 a" })).getByText("超过今天上限")).toBeInTheDocument();
    expect(within(screen.getByRole("article", { name: "投递 b" })).getByText("已暂停提醒")).toBeInTheDocument();
    const c = screen.getByRole("article", { name: "投递 c" });
    expect(within(c).queryByText("超过今天上限")).not.toBeInTheDocument();
    expect(within(c).queryByText("已暂停提醒")).not.toBeInTheDocument();
  });

  it("本人点击才确认：POST read 带 CSRF，成功后该条从未读消失、其余仍在", async () => {
    const s = server([d("a"), d("b")]);
    mount(s.impl);
    const card = await screen.findByRole("article", { name: "投递 a" });
    await userEvent.click(within(card).getByRole("button", { name: "标记已读" }));
    await screen.findByRole("article", { name: "投递 b" });
    expect(screen.queryByRole("article", { name: "投递 a" })).not.toBeInTheDocument();
    const w = s.calls.filter((c) => c.method === "POST");
    expect(w).toEqual([{ method: "POST", path: "/v1/deliveries/a/read", csrf: "tok-1" }]);
  });

  it("全部读完显示空状态", async () => {
    mount(server([d("a")]).impl);
    const card = await screen.findByRole("article", { name: "投递 a" });
    await userEvent.click(within(card).getByRole("button", { name: "标记已读" }));
    expect(await screen.findByText("都看过了")).toBeInTheDocument();
  });

  it("确认失败（409）：显示服务端原因，该条仍留在未读里，并重新读取", async () => {
    const s = server([d("a")], () => json(409, { code: "conflict", why: "delivery a is acknowledged, not an unread web item", next: "refresh" }));
    mount(s.impl);
    const card = await screen.findByRole("article", { name: "投递 a" });
    await userEvent.click(within(card).getByRole("button", { name: "标记已读" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not an unread web item");
    expect(screen.getByRole("article", { name: "投递 a" })).toBeInTheDocument();
    expect(s.calls.filter((c) => c.path === "/v1/deliveries/unread").length).toBeGreaterThan(1);
  });

  it("读取失败：显示错误并可重试", async () => {
    let fail = true;
    const s = server([d("a")]);
    const impl = ((url: string, init?: RequestInit) =>
      fail && url === "/v1/deliveries/unread" ? Promise.resolve(json(500, { code: "internal", why: "boom", next: "retry" })) : s.impl(url, init)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByRole("article", { name: "投递 a" })).toBeInTheDocument();
  });
});
