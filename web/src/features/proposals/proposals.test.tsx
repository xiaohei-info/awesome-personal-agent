import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature as decisions } from "../decisions";
import { feature } from "./index";

// 测试边界替身：按 api/items（phase 过滤）与 api/decisions（accept/dismiss）的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

interface P { id: string; phase: string; title: string; summary?: string; project?: string; priority?: string; origin?: { source: string; actor?: string }; ask?: unknown }
const prop = (id: string, over: Partial<P> = {}): P => ({ id, phase: "proposed", title: "建议 " + id, ...over });

function server(initial: P[], write?: (path: string, body: any) => Response | undefined, rejections: Record<string, unknown> = {}) {
  let items = initial;
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    const q = url.match(/^\/v1\/items\?phase=(\w+)$/);
    if (q) return json(200, { items: items.filter((i) => i.phase === q[1]), last_rejections: rejections });
    const w = url.match(/^\/v1\/items\/(.+)\/(accept|dismiss)$/);
    if (w && method === "POST") {
      const res = write?.(url, body);
      if (res) return res;
      items = items.filter((i) => i.id !== w[1]);
      return json(200, { id: w[1], version: 2, replayed: false });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch, path = "/proposals") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App features={[decisions, feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

const rej = (over: object = {}) => ({ seq: 9, at: "2030-01-01T00:00:00Z", actor: { type: "agent", principal: "DEL", via: "cli" }, code: "basis_required", why: "the grant does not cover this action", grant: "grt_bad", ...over });


describe("建议流页（flow-proposal-triage）", () => {
  it("ACT-PROP-01：只列 proposed 事项，请求 phase=proposed，不与决策待办混在一起", async () => {
    const s = server([prop("a", { project: "acc-open", origin: { source: "agent", actor: "observer" } }), prop("n", { phase: "needs_input", title: "决策 n" })]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    expect(within(card).getByText(/acc-open/)).toBeInTheDocument();
    expect(screen.queryByText("决策 n")).not.toBeInTheDocument();
    expect(s.calls.some((c) => c.path === "/v1/items?phase=proposed")).toBe(true);
    expect(s.calls.some((c) => c.path === "/v1/items?phase=needs_input")).toBe(false);
  });

  it("ACT-PROP-01：决策待办页不出现建议", async () => {
    const s = server([prop("a"), prop("n", { phase: "needs_input", title: "决策 n", ask: { question: "问 n", options: [] } })]);
    mount(s.impl, "/decisions");
    await screen.findByText("问 n");
    expect(screen.queryByText("建议 a")).not.toBeInTheDocument();
  });

  it("ACT-PROP-03：接受 → POST accept 带 CSRF，离开建议流", async () => {
    const s = server([prop("a"), prop("b")]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "接受" }));
    await screen.findByText("建议 b");
    expect(screen.queryByText("建议 a")).not.toBeInTheDocument();
    const w = s.calls.find((c) => c.method === "POST" && c.path === "/v1/items/a/accept")!;
    expect(w.csrf).toBe("tok-1");
  });

  it("ACT-PROP-04：丢弃必须填原因；原因随 dismiss 提交，离开建议流", async () => {
    const s = server([prop("a"), prop("b")]);
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "丢弃" }));
    const confirm = within(card).getByRole("button", { name: "确认丢弃" });
    expect(confirm).toBeDisabled();
    await userEvent.type(within(card).getByLabelText("丢弃原因"), "   ");
    expect(confirm).toBeDisabled();
    await userEvent.clear(within(card).getByLabelText("丢弃原因"));
    await userEvent.type(within(card).getByLabelText("丢弃原因"), "重复");
    await userEvent.click(confirm);
    await screen.findByText("建议 b");
    expect(screen.queryByText("建议 a")).not.toBeInTheDocument();
    expect(s.calls.find((c) => c.path === "/v1/items/a/dismiss")!.body).toEqual({ reason: "重复" });
    expect(s.calls.some((c) => c.path === "/v1/items/a/accept")).toBe(false);
  });

  it("已被别处处理（409）：显示服务端原因并刷新列表", async () => {
    const s = server([prop("a")], () => json(409, { code: "conflict", why: "the item is not a pending proposal", next: "re-read the item; the first answer stands" }));
    mount(s.impl);
    const card = (await screen.findByText("建议 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "接受" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not a pending proposal");
    expect(s.calls.filter((c) => c.path === "/v1/items?phase=proposed").length).toBeGreaterThan(1);
  });

  it("空列表", async () => {
    mount(server([]).impl);
    expect(await screen.findByText("没有待处理的建议")).toBeInTheDocument();
  });

  it("读取失败：显示错误并可重试", async () => {
    let fail = true;
    const s = server([prop("a")]);
    const impl = ((url: string, init?: RequestInit) =>
      fail && url.startsWith("/v1/items") ? Promise.resolve(json(500, { code: "internal", why: "boom", next: "retry" })) : s.impl(url, init)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("建议 a")).toBeInTheDocument();
  });
});

describe("被拒绝的处理（ACT-PROP-05、flow-proposal-triage）", () => {
  it("标明这次被拒绝的处理与原因；阶段不变仍可由本人接受", async () => {
    const s = server([prop("a"), prop("b")], undefined, { a: rej({ why: "no basis for dismiss" }) });
    mount(s.impl);
    const a = await screen.findByRole("article", { name: "建议 a" });
    const note = within(a).getByRole("note", { name: "被拒绝的处理" });
    expect(note).toHaveTextContent("no basis for dismiss");
    expect(note).toHaveTextContent("授权单 grt_bad");
    expect(within(screen.getByRole("article", { name: "建议 b" })).queryByRole("note")).toBeNull();
    expect(within(a).getByRole("button", { name: "接受" })).toBeEnabled();
  });
});
