import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { pick } from "./TypeViews";

// 事项列表的类型筛选（原「能力包视图」页）。替身按 api/packs 的 GET /v1/packs（含 views）与 api/items 的 GET /v1/items 契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const packs = [
  { name: "brief", views: { brief: { kind: "brief", list: { columns: ["title", "project", "created_at"], filters: ["project", "tag"] }, card: { fields: ["summary", "spec.period"] } } } },
  // 第二个包：页面代码里没有任何关于它的知识，只靠声明显示。
  { name: "newpack", views: { memo: { kind: "memo", list: { columns: ["title", "priority"], filters: [] }, card: { fields: ["spec.owner"] } } } },
  { name: "no-views", views: null },
];
const items = [
  { id: "i1", kind: "brief", phase: "open", title: "晨报", project: "home", tags: ["a"], summary: "S1", spec: { period: "daily" }, created_at: "2030-01-01T00:00:00Z" },
  { id: "i2", kind: "brief", phase: "open", title: "周报", project: "work", tags: ["b", "a"], summary: "S2", spec: { period: "weekly" }, created_at: "2030-01-02T00:00:00Z" },
  { id: "i3", kind: "memo", phase: "open", title: "备忘", priority: "high", spec: { owner: "me" } },
  { id: "i4", kind: "task", phase: "open", title: "无关事项" },
];

function server(o: { packs?: unknown } = {}) {
  return (async (url: string) => {
    if (url === "/v1/projects") return json(200, { projects: [{ id: "home", name: "家务" }, { id: "work", name: "工作" }] });
    if (url === "/v1/packs") return json(200, { packs: o.packs ?? packs });
    if (url.startsWith("/v1/items")) return json(200, { items });
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
}
function At() {
  const { pathname, search } = useLocation();
  return <span data-testid="at">{pathname + search}</span>;
}
function mount(f: typeof fetch, at = "/items") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={[at]}><App features={[feature]} fetchImpl={f} /><At /></MemoryRouter>);
}
const table = async (name = "简报列表") => within(await screen.findByRole("table", { name }));

beforeEach(() => sessionStorage.clear());

describe("事项 › 类型筛选", () => {
  it("ACT-PACK-05: 选「简报」后按包声明的列显示，列名是中文，只含该类型的事项", async () => {
    mount(server());
    await userEvent.click(await screen.findByRole("button", { name: "简报" }));
    const t = await table();
    expect(t.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["标题", "项目", "创建时间"]);
    expect(t.getByText("晨报")).toBeInTheDocument();
    expect(t.getByText("家务")).toBeInTheDocument(); // 项目显示名字
    expect(screen.queryByText("备忘")).not.toBeInTheDocument();
    expect(screen.queryByText("无关事项")).not.toBeInTheDocument();
    expect(screen.getByTestId("at")).toHaveTextContent("/items?view=brief%2Fbrief");
  });

  it("ACT-PACK-05: 按声明的筛选项过滤（标量与数组字段），可恢复", async () => {
    mount(server(), "/items?view=brief/brief");
    await table();
    await userEvent.selectOptions(screen.getByLabelText("项目"), "work");
    expect(screen.queryByText("晨报")).not.toBeInTheDocument();
    expect(screen.getAllByText("周报").length).toBeGreaterThan(0);
    await userEvent.selectOptions(screen.getByLabelText("项目"), "");
    await userEvent.selectOptions(screen.getByLabelText("标签"), "b");
    expect(screen.queryByText("晨报")).not.toBeInTheDocument();
    expect(screen.getAllByText("周报").length).toBeGreaterThan(0);
  });

  it("ACT-PACK-05: 卡片显示声明的字段，支持 spec.period 点路径", async () => {
    mount(server(), "/items?view=brief/brief");
    await table();
    const card = within(await screen.findByRole("article", { name: "周报" }));
    expect(card.getByText("S2")).toBeInTheDocument();
    expect(card.getByText("weekly")).toBeInTheDocument();
    expect(card.queryByText("工作")).not.toBeInTheDocument(); // 未声明的字段不显示
  });

  it("ACT-PACK-05: 新增包只靠声明出现为新类型，无需改前端；查不到的字段名显示原名", async () => {
    mount(server());
    await userEvent.click(await screen.findByRole("button", { name: "memo" }));
    const t = await table("memo列表");
    expect(t.getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["标题", "优先级"]);
    expect(t.getByText("high")).toBeInTheDocument();
    expect(within(screen.getByRole("article", { name: "备忘" })).getByText("spec.owner")).toBeInTheDocument();
  });

  it("「全部」显示原来的事项列表；没有包声明视图时不出现类型筛选", async () => {
    mount(server({ packs: [{ name: "x", views: null }, { name: "y" }] }));
    expect(await screen.findByText("无关事项")).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "类型" })).not.toBeInTheDocument();
  });

  it("该类型下没有事项时显示空状态，不渲染空表格", async () => {
    mount(server({ packs: [{ name: "e", views: { v: { kind: "none", list: { columns: ["title"], filters: [] }, card: { fields: [] } } } }] }), "/items?view=e/v");
    expect(await screen.findByText("没有符合条件的事项")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("pick 取点路径，缺失为 undefined", () => {
    expect(pick({ spec: { a: 1 } }, "spec.a")).toBe(1);
    expect(pick({ spec: null }, "spec.a")).toBeUndefined();
    expect(pick({}, "x.y")).toBeUndefined();
    expect(pick({ tags: ["a"] }, "tag")).toEqual(["a"]);
  });
});
