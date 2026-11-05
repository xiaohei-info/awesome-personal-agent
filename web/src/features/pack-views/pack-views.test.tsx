import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { pick } from "./PackViewsPage";

// 测试边界替身：按 api/packs 的 GET /v1/packs（含 views）与 api/items 的 GET /v1/items 契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const ERR = { code: "internal", why: "ledger unavailable", next: "retry" };

const packs = [
  { name: "brief", views: { brief: { kind: "brief", list: { columns: ["title", "project", "created_at"], filters: ["project", "tag"] }, card: { fields: ["summary", "spec.period"] } } } },
  // 第二个包：页面代码里没有任何关于它的知识，只靠声明显示。
  { name: "newpack", views: { memo: { kind: "memo", list: { columns: ["title", "priority"], filters: [] }, card: { fields: ["spec.owner"] } } } },
  { name: "no-views", views: null },
];
const items = [
  { id: "i1", kind: "brief", title: "晨报", project: "home", tags: ["a"], summary: "S1", spec: { period: "daily" }, created_at: "2030-01-01T00:00:00Z" },
  { id: "i2", kind: "brief", title: "周报", project: "work", tags: ["b", "a"], summary: "S2", spec: { period: "weekly" }, created_at: "2030-01-02T00:00:00Z" },
  { id: "i3", kind: "memo", title: "备忘", priority: "high", spec: { owner: "me" } },
  { id: "i4", kind: "task", title: "无关事项" },
];

function server(o: { fail?: boolean; packs?: unknown } = {}) {
  return (async (url: string) => {
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (o.fail) return json(500, ERR);
    if (url === "/v1/packs") return json(200, { packs: o.packs ?? packs });
    if (url === "/v1/items") return json(200, { items });
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
}
function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={["/pack-views"]}><App features={[feature]} fetchImpl={f} /></MemoryRouter>);
}
const table = async () => within(await screen.findByRole("table", { name: "视图列表" }));

beforeEach(() => sessionStorage.clear());

describe("能力包视图页", () => {
  it("ACT-PACK-05: 列表按包声明的列显示，只含该视图 kind 的事项", async () => {
    mount(server());
    const t = await table();
    expect(t.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["title", "project", "created_at"]);
    expect(t.getByText("晨报")).toBeInTheDocument();
    expect(t.getByText("周报")).toBeInTheDocument();
    expect(screen.queryByText("备忘")).not.toBeInTheDocument();
    expect(screen.queryByText("无关事项")).not.toBeInTheDocument();
  });

  it("ACT-PACK-05: 按声明的筛选项过滤（标量与数组字段），可恢复", async () => {
    mount(server());
    await table();
    await userEvent.selectOptions(screen.getByLabelText("筛选 project"), "work");
    expect(screen.queryByText("晨报")).not.toBeInTheDocument();
    expect(screen.getAllByText("周报").length).toBeGreaterThan(0);
    await userEvent.selectOptions(screen.getByLabelText("筛选 project"), "");
    await userEvent.selectOptions(screen.getByLabelText("筛选 tag"), "b");
    expect(screen.queryByText("晨报")).not.toBeInTheDocument();
    expect(screen.getAllByText("周报").length).toBeGreaterThan(0);
    expect(screen.queryByLabelText("筛选 priority")).not.toBeInTheDocument();
  });

  it("ACT-PACK-05: 卡片显示声明的字段，支持 spec.period 点路径", async () => {
    mount(server());
    await table();
    const card = within(await screen.findByLabelText("卡片 周报"));
    expect(card.getByText("S2")).toBeInTheDocument();
    expect(card.getByText("weekly")).toBeInTheDocument();
    expect(card.queryByText("work")).not.toBeInTheDocument(); // 未声明的字段不显示
  });

  it("ACT-PACK-05: 新增包只靠声明出现为新视图，无需改前端", async () => {
    mount(server());
    await table();
    await userEvent.selectOptions(screen.getByLabelText("视图"), "newpack/memo");
    const t = await table();
    expect(t.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["title", "priority"]);
    expect(t.getByText("high")).toBeInTheDocument();
    expect(within(screen.getByLabelText("卡片 备忘")).getByText("me")).toBeInTheDocument();
  });

  it("没有任何包声明视图时给出空状态", async () => {
    mount(server({ packs: [{ name: "x", views: null }, { name: "y" }] }));
    expect(await screen.findByText("没有能力包声明视图")).toBeInTheDocument();
  });

  it("视图下没有事项时显示空状态，不渲染空表格", async () => {
    mount(server({ packs: [{ name: "e", views: { v: { kind: "none", list: { columns: ["title"], filters: [] }, card: { fields: [] } } } }] }));
    expect(await screen.findByText("没有符合条件的事项")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("服务端失败时显示真实原因，不伪造数据", async () => {
    mount(server({ fail: true }));
    expect(await screen.findByRole("alert")).toHaveTextContent("ledger unavailable");
  });

  it("pick 取点路径，缺失为 undefined", () => {
    expect(pick({ spec: { a: 1 } }, "spec.a")).toBe(1);
    expect(pick({ spec: null }, "spec.a")).toBeUndefined();
    expect(pick({}, "x.y")).toBeUndefined();
    expect(pick({ tags: ["a"] }, "tag")).toEqual(["a"]);
  });
});
