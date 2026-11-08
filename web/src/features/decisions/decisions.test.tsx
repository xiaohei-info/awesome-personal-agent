import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { sortDecisions, type DecisionItem } from "./DecisionsPage";

// 测试边界替身：按 api/decisions、api/items、api/why 的契约应答（错误体见附录 B.3）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function item(id: string, over: Partial<Omit<DecisionItem, "ask">> & { ask?: Partial<NonNullable<DecisionItem["ask"]>> } = {}): DecisionItem {
  const { ask, ...rest } = over;
  return {
    id, version: 1, phase: "needs_input", kind: "decision", title: "事项 " + id, priority: "normal",
    ask: { question: "问题 " + id, options: [{ id: "yes", label: "同意" }, { id: "no", label: "拒绝" }], ...ask },
    ...rest,
  };
}

function server(initial: DecisionItem[], decide?: (id: string, body: any) => Response, rejections: Record<string, unknown> = {}) {
  let items = initial;
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    const csrf = (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null;
    calls.push({ method, path: url, body, csrf });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items?phase=needs_input") return json(200, { items, last_rejections: rejections });
    const why = url.match(/^\/v1\/why\/(.+)$/);
    if (why) return json(200, { id: why[1], steps: [{ seq: 1, type: "decision.requested", reason: "合并前需要本人确认", actor: {}, at: "", object: why[1], chain_depth: 0 }] });
    const d = url.match(/^\/v1\/items\/(.+)\/decide$/);
    if (d && method === "POST") {
      const res = decide?.(d[1], body);
      if (res) return res;
      items = items.filter((i) => i.id !== d[1]);
      return json(200, { id: d[1], version: 2, replayed: false });
    }
    return json(404, { code: "not_found", why: "no such route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/decisions"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

beforeEach(() => sessionStorage.clear());

const rej = (over: object = {}) => ({ seq: 9, at: "2030-01-01T00:00:00Z", actor: { type: "agent", principal: "DEL", via: "cli" }, code: "basis_required", why: "the grant does not cover this action", grant: "grt_bad", ...over });

describe("排序", () => {
  it("先按截止（无截止最后），再按优先级", () => {
    const a = item("a", { priority: "low", ask: { deadline: "2030-01-02T00:00:00Z" } });
    const b = item("b", { priority: "urgent" });
    const c = item("c", { priority: "normal", ask: { deadline: "2030-01-01T00:00:00Z" } });
    const d = item("d", { priority: "high", ask: { deadline: "2030-01-01T00:00:00Z" } });
    expect(sortDecisions([a, b, c, d]).map((i) => i.id)).toEqual(["d", "c", "a", "b"]);
  });
});

describe("决策待办页（flow-decision-answer）", () => {
  it("卡片展示问题、选项、推荐、截止，并按截止排序", async () => {
    const s = server([
      item("late", { ask: { question: "晚的", deadline: "2031-01-01T00:00:00Z" } }),
      item("soon", { ask: { question: "早的", deadline: "2030-01-01T00:00:00Z", recommend: "yes" } }),
    ]);
    mount(s.impl);
    const cards = await screen.findAllByRole("article");
    expect(cards.map((c) => within(c).getByRole("heading").textContent)).toEqual(["早的", "晚的"]);
    expect(within(cards[0]).getByRole("button", { name: /同意.*推荐/ })).toBeInTheDocument();
    expect(within(cards[0]).getByText(/截止/)).toBeInTheDocument();
  });

  it("本人选择选项：POST decide 带 choice 与 CSRF，答完从待办消失", async () => {
    const s = server([item("a"), item("b")]);
    mount(s.impl);
    const card = (await screen.findByText("问题 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "同意" }));
    await screen.findByText("问题 b");
    expect(screen.queryByText("问题 a")).not.toBeInTheDocument();
    const w = s.calls.find((c) => c.method === "POST" && c.path === "/v1/items/a/decide")!;
    expect(w.body).toEqual({ choice: "yes" });
    expect(w.csrf).toBe("tok-1");
  });

  it("别的想法：free_text 时可提交文字；空文字不提交；不允许自由文本时不出现", async () => {
    const s = server([item("a", { ask: { free_text: true } }), item("b")]);
    mount(s.impl);
    const card = (await screen.findByText("问题 a")).closest("article")!;
    expect(within(card).getByRole("button", { name: "提交想法" })).toBeDisabled();
    await userEvent.type(within(card).getByLabelText("别的想法"), "再等一周");
    await userEvent.click(within(card).getByRole("button", { name: "提交想法" }));
    await screen.findByText("问题 b");
    expect(s.calls.find((c) => c.path === "/v1/items/a/decide")!.body).toEqual({ text: "再等一周" });
    const b = screen.getByText("问题 b").closest("article")!;
    expect(within(b).queryByLabelText("别的想法")).not.toBeInTheDocument();
  });

  it("为什么在这：按需读 /why 并显示原因", async () => {
    const s = server([item("a")]);
    mount(s.impl);
    const card = (await screen.findByText("问题 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "为什么在这" }));
    expect(await within(card).findByText("合并前需要本人确认")).toBeInTheDocument();
  });

  it("后到的回答（409）：显示服务端原因并刷新列表", async () => {
    const s = server([item("a")], () => json(409, { code: "conflict", why: "the decision was already resolved or is not waiting for an answer", next: "read the item's ask.resolution" }));
    mount(s.impl);
    const card = (await screen.findByText("问题 a")).closest("article")!;
    await userEvent.click(within(card).getByRole("button", { name: "同意" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("already resolved");
    expect(s.calls.filter((c) => c.path === "/v1/items?phase=needs_input").length).toBeGreaterThan(1);
  });

  it("空列表与加载失败各有状态", async () => {
    mount(server([]).impl);
    expect(await screen.findByText("没有待办决策")).toBeInTheDocument();
  });

  it("列表读取失败：显示错误并可重试", async () => {
    let fail = true;
    const s = server([item("a")]);
    const impl = ((url: string, init?: RequestInit) =>
      fail && url.startsWith("/v1/items") ? Promise.resolve(json(500, { code: "internal", why: "boom", next: "retry" })) : s.impl(url, init)) as typeof fetch;
    mount(impl);
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    fail = false;
    await userEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("问题 a")).toBeInTheDocument();
  });
});

describe("web_only 决策（flow-web-only、flow-life-renewal、flow-first-day）", () => {
  const wo = item("w", { title: "续费 1000 元", ask: { question: "续费吗？", risk_class: "web_only" } });

  it("标明只能在 Web 确认，且可在此回答", async () => {
    const s = server([wo]);
    mount(s.impl);
    const card = (await screen.findByText("续费吗？")).closest("article")!;
    expect(within(card).getByText("仅限 Web 确认")).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: "同意" }));
    await screen.findByText("没有待办决策");
    expect(s.calls.find((c) => c.path === "/v1/items/w/decide")!.body).toEqual({ choice: "yes" });
  });

  it("非 web_only 不显示该标记", async () => {
    mount(server([item("a")]).impl);
    await screen.findByText("问题 a");
    expect(screen.queryByText("仅限 Web 确认")).not.toBeInTheDocument();
  });
});

describe("被拒绝的代答（ACT-AUTH-03、flow-decision-answer）", () => {
  it("标明被拒绝的代答：操作者、原因与授权单 id；没有拒绝的卡片不带标注", async () => {
    mount(server([item("a"), item("b")], undefined, { a: rej() }).impl);
    const a = (await screen.findByRole("article", { name: "事项 a" }));
    const note = within(a).getByRole("note", { name: "被拒绝的代答" });
    expect(note).toHaveTextContent("DEL");
    expect(note).toHaveTextContent("the grant does not cover this action");
    expect(note).toHaveTextContent("授权单 grt_bad");
    expect(within(screen.getByRole("article", { name: "事项 b" })).queryByRole("note")).toBeNull();
  });

  it("没有授权单时不显示授权单 id", async () => {
    mount(server([item("a")], undefined, { a: rej({ grant: undefined }) }).impl);
    const note = await screen.findByRole("note", { name: "被拒绝的代答" });
    expect(note).not.toHaveTextContent("授权单");
  });
});
