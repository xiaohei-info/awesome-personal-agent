import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "../decisions";
import type { DecisionItem } from "../decisions/DecisionsPage";

// 测试边界替身：按 api/items（Web 凭据看得到初筛中的完整事项，ask.screening_pending=true）与 api/decisions 的契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const item = (id: string, screening: boolean): DecisionItem => ({
  id, version: 1, phase: "needs_input", kind: "decision", title: "事项 " + id,
  ask: { question: "问题 " + id, options: [{ id: "yes", label: "同意" }], ...(screening ? { screening_pending: true } : {}) },
});

function server(items: DecisionItem[]) {
  const calls: { method: string; path: string; body?: unknown }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    calls.push({ method, path: url, body: init.body ? JSON.parse(init.body as string) : undefined });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items?phase=needs_input") return json(200, { items });
    const d = url.match(/^\/v1\/items\/(.+)\/decide$/);
    if (d && method === "POST") {
      items = items.filter((i) => i.id !== d[1]);
      return json(200, { id: d[1], version: 2, replayed: false });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls, setItems: (n: DecisionItem[]) => (items = n) };
}

const mount = (f: typeof fetch) => {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/decisions"]}>
      <App features={[feature]} fetchImpl={f} />
    </MemoryRouter>,
  );
};

beforeEach(() => sessionStorage.clear());

describe("决策待办的初筛状态（flow-screening）", () => {
  it("初筛中的决策标明\"初筛中\"，并仍显示问题与选项；已返回的不标", async () => {
    mount(server([item("s", true), item("n", false)]).impl);
    const s = (await screen.findByText("问题 s")).closest("article")!;
    const n = screen.getByText("问题 n").closest("article")!;
    expect(within(s).getByText(/初筛中/)).toBeInTheDocument();
    expect(within(s).getByRole("button", { name: "同意" })).toBeEnabled();
    expect(within(n).queryByText(/初筛中/)).not.toBeInTheDocument();
  });

  it("本人在初筛中直接回答：照常 POST decide，答完从待办消失", async () => {
    const s = server([item("s", true)]);
    mount(s.impl);
    const card = (await screen.findByText("问题 s")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "同意" }));
    await screen.findByText("没有要你决定的事");
    expect(s.calls.find((c) => c.path === "/v1/items/s/decide")!.body).toEqual({ choice: "yes" });
  });

  it("初筛结束后刷新：标记消失，决策照常保留", async () => {
    const s = server([item("s", true), item("x", false)]);
    mount(s.impl);
    await screen.findByText(/初筛中/);
    s.setItems([item("s", false), item("x", false)]);
    const x = screen.getByText("问题 x").closest("article")!;
    await userEvent.click(within(x).getByRole("button", { name: "同意" })); // 答另一条触发刷新
    await screen.findByText("问题 s");
    expect(screen.queryByText(/初筛中/)).not.toBeInTheDocument();
  });
});
