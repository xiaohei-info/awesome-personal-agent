import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { feature as items } from "../items";

// 测试边界替身：按 api/items 的契约应答 GET /today、PATCH /items/{id}、POST /items/{id}/progress|transition。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const mk = (id: string, over: object = {}) => ({ id, phase: "open", version: 1, kind: "task", title: "事项 " + id, ...over });

function server(today: Record<string, unknown[]>, fail: (path: string) => boolean = () => false) {
  const calls: { method: string; path: string; body?: any; headers: Record<string, string> }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    calls.push({ method, path: url, body: init.body ? JSON.parse(init.body as string) : undefined, headers: (init.headers ?? {}) as Record<string, string> });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/today") return json(200, today);
    if (method !== "GET" && fail(url)) return json(409, { code: "conflict", why: "version moved", next: "reload" });
    if (method !== "GET") return json(200, { id: "x", version: 2 });
    return json(404, { code: "not_found", why: "no route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/today"]}>
      <App features={[feature, items]} fetchImpl={f} />
    </MemoryRouter>,
  );
}
const writes = (s: ReturnType<typeof server>) => s.calls.filter((c) => c.method !== "GET");
beforeEach(() => sessionStorage.clear());

describe("今天页（flow-web-workbench）", () => {
  it("ACT-WBN-08：四组按服务端取材展示，过期的计划事项仍留在 planned 组", async () => {
    const s = server({
      planned: [mk("WB-A", { planned_for: "2000-01-01" }), mk("WB-B", { planned_for: "2000-01-02" })],
      due: [mk("WB-D", { deadline: "2030-01-01T00:00:00Z" })],
      pending: [mk("WB-G", { phase: "needs_input" })],
      stale: [mk("WB-H")],
    });
    mount(s.impl);
    const planned = await screen.findByRole("region", { name: "计划今天或更早" });
    expect(within(planned).getByText("事项 WB-A")).toBeInTheDocument();
    expect(within(planned).getByText("事项 WB-B")).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "今天到期" })).getByText("事项 WB-D")).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "等你决定" })).getByText("事项 WB-G")).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "久未推进" })).getByText("事项 WB-H")).toBeInTheDocument();
    expect(writes(s)).toHaveLength(0); // 浏览不改任何事项（不自动改期）
  });

  it("ACT-WBN-08：改计划日期、写进展、完成各发一次写入，之后重读分组", async () => {
    const s = server({ planned: [mk("WB-A", { version: 3 }), mk("WB-B")], due: [], pending: [], stale: [] });
    mount(s.impl);
    const user = userEvent.setup();
    const a = (await screen.findByText("事项 WB-A")).closest("li")!;
    await user.type(within(a).getByLabelText("改计划日期 事项 WB-A"), "2031-05-06");
    await user.click(within(a).getByRole("button", { name: "改计划日期" }));
    await waitFor(() => expect(writes(s)).toHaveLength(1));
    expect(writes(s)[0]).toMatchObject({ method: "PATCH", path: "/v1/items/WB-A", body: { planned_for: "2031-05-06" } });
    expect(writes(s)[0].headers["If-Match"]).toBe("3");

    const b = (await screen.findByText("事项 WB-B")).closest("li")!;
    await user.type(within(b).getByLabelText("进展"), "已订酒店");
    await user.click(within(b).getByRole("button", { name: "写进展" }));
    await waitFor(() => expect(writes(s)).toHaveLength(2));
    expect(writes(s)[1]).toMatchObject({ method: "POST", path: "/v1/items/WB-B/progress", body: { text: "已订酒店" } });

    await user.click(within((await screen.findByText("事项 WB-B")).closest("li")!).getByRole("button", { name: "完成" }));
    await waitFor(() => expect(writes(s)).toHaveLength(3));
    expect(writes(s)[2]).toMatchObject({ method: "POST", path: "/v1/items/WB-B/transition", body: { to: "done" } });
    await waitFor(() => expect(s.calls.filter((c) => c.path === "/v1/today").length).toBe(4)); // 首次 + 三次写后重读
  });

  it("服务端拒绝写入时显示原因并重读，不留下本地假状态", async () => {
    const s = server({ planned: [mk("WB-A")], due: [], pending: [], stale: [] }, (p) => p.endsWith("/transition"));
    mount(s.impl);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "完成" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("version moved");
    expect(screen.getByText("事项 WB-A")).toBeInTheDocument();
  });

  it("久未推进：批量选继续、改期、放弃，逐条走既有接口；单条失败不影响其余", async () => {
    const s = server({ planned: [], due: [], pending: [], stale: [mk("S1"), mk("S2")] }, (p) => p === "/v1/items/S2/progress");
    mount(s.impl);
    const user = userEvent.setup();
    await user.click(await screen.findByLabelText("选择 事项 S1"));
    await user.click(screen.getByLabelText("选择 事项 S2"));
    await user.click(screen.getByRole("button", { name: /应用到所选/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("事项 S2：version moved");
    expect(writes(s).map((c) => c.path)).toEqual(["/v1/items/S1/progress", "/v1/items/S2/progress"]);

    await user.click(screen.getByLabelText("选择 事项 S1"));
    await user.selectOptions(screen.getByLabelText("处理方式"), "reschedule");
    expect(screen.getByRole("button", { name: /应用到所选/ })).toBeDisabled(); // 没填日期不能提交
    await user.type(screen.getByLabelText("批量改期日期"), "2031-01-02");
    await user.click(screen.getByRole("button", { name: /应用到所选/ }));
    await waitFor(() => expect(writes(s)).toHaveLength(3));
    expect(writes(s)[2]).toMatchObject({ method: "PATCH", body: { planned_for: "2031-01-02" } });

    await user.click(screen.getByLabelText("选择 事项 S1"));
    await user.selectOptions(screen.getByLabelText("处理方式"), "drop");
    await user.click(screen.getByRole("button", { name: /应用到所选/ }));
    await waitFor(() => expect(writes(s)).toHaveLength(4));
    expect(writes(s)[3]).toMatchObject({ path: "/v1/items/S1/transition", body: { to: "dropped" } });
  });

  it("待决组只列最紧的几条并链到决策待办；空组有提示", async () => {
    const many = Array.from({ length: 7 }, (_, i) => mk("P" + i, { phase: "needs_input" }));
    const s = server({ planned: [], due: [], pending: many, stale: [] });
    mount(s.impl);
    const region = await screen.findByRole("region", { name: "等你决定" });
    expect(within(region).getAllByRole("listitem")).toHaveLength(5);
    expect(within(region).getByRole("heading")).toHaveTextContent("（7）");
    expect(within(region).getByRole("link", { name: "去决策待办处理" })).toHaveAttribute("href", "/decisions");
    expect(within(screen.getByRole("region", { name: "今天到期" })).getByText("这一组没有事项。")).toBeInTheDocument();
  });
});

describe("发布门槛（flow-release-gates）", () => {
  it("ACT-GATE-09：§4.1 写明 360 像素走查结论，§21.3 的 SPIKE-WEB-MOBILE 行已关闭", async () => {
    const mods = import.meta.glob<string>("../../../../docs/solution-design.md", { query: "?raw", import: "default", eager: true });
    const doc = Object.values(mods)[0];
    const s41 = doc.slice(doc.indexOf("### 4.1 Web 中台"), doc.indexOf("### 4.2"));
    expect(s41).toMatch(/SPIKE-WEB-MOBILE.*已关闭/);
    expect(s41).toMatch(/scrollWidth.*360/);
    const row = doc.split("\n").find((l: string) => l.startsWith("| `SPIKE-WEB-MOBILE`"))!;
    expect(row).toContain("已关闭");
  });
});
