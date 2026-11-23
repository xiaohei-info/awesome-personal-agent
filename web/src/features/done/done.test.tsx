import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import type { Item } from "../items/ItemsPage";
import { feature } from "./index";

const TODAY = "2030-01-09"; // 周三，本周是 01-07 – 01-13
const mk = (id: string, over: Partial<Item> = {}): Item => ({ id, phase: "done", version: 1, kind: "task", title: "事项 " + id, ...over });
const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

// 测试边界替身：GET /items?phase=done 带 done_at（附录 B.2），范围读法给主机的今天与时区。
// GET /items?progress_from&progress_to 给这一周有进展的事与其进展（moved：周一 → 事项与进展）。
type P = { id: string; text: string; on?: string; at: string };
function server(items: Item[], at: Record<string, string>, moved: Record<string, { items: Item[]; progress: Record<string, P[]> }> = {}, unknown: string[] = []) {
  return (async (url: string) => {
    if (url === "/v1/projects") return json({ projects: [{ id: "p1", name: "家务" }, { id: "p2", name: "工作" }] });
    if (url === "/v1/items?phase=done") return json({ items, last_rejections: {}, done_at: at, done_unknown: unknown });
    const m = /^\/v1\/items\?progress_from=([\d-]+)&progress_to=([\d-]+)$/.exec(url);
    if (m) return json({ last_rejections: {}, ...(moved[m[1]] ?? { items: [], progress: {} }) });
    if (url.startsWith("/v1/items?from=")) return json({ items: [], last_rejections: {}, today: TODAY, utc_offset: "+08:00" });
    return json({});
  }) as unknown as typeof fetch;
}
const mount = (f: typeof fetch, path = "/done") => {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={[path]}><App features={[feature]} fetchImpl={f} /></MemoryRouter>);
};

beforeEach(() => sessionStorage.clear());

describe("已完成页", () => {
  it("本周按完成那天（主机时区）收录、按项目分组、最近完成的在前；简报不算，委派算并标出；可切到上一周", async () => {
    const items = [
      mk("a", { project: "p1", title: "交电费" }),
      mk("b", { project: "p2", title: "发周报", kind: "delegation" }),
      mk("c", { project: "p2", title: "评审方案" }),
      mk("d", { title: "看牙" }),
      mk("e", { kind: "brief", title: "早间简报" }),
      mk("old", { project: "p1", title: "上周的事" }),
    ];
    const at = {
      a: "2030-01-07T02:00:00Z",
      b: "2030-01-09T01:00:00Z",
      c: "2030-01-08T03:00:00Z",
      d: "2030-01-09T00:30:00Z",
      e: "2030-01-09T00:00:00Z",
      old: "2030-01-06T15:59:00Z", // 主机时间 01-06 23:59，算上一周
    };
    mount(server(items, at));
    expect(await screen.findByText("进展 0 条 · 完成 4 项 · 涉及项目 2 个")).toBeInTheDocument();
    const groups = within(screen.getByRole("main")).getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(groups).toEqual(["工作", "家务", "没归项目的"]);
    const work = screen.getByRole("region", { name: "工作" });
    expect(within(work).getByText("进展 0 条 · 完成 2 项")).toBeInTheDocument();
    expect(within(work).getAllByRole("link").map((l) => l.textContent)).toEqual(["工作", "发周报", "评审方案"]);
    expect(within(work).getByText(/周三 01-09 完成 · 委派/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "交电费" })).toHaveAttribute("href", "/items/a");
    expect(screen.queryByText("早间简报")).toBeNull();
    expect(screen.getByRole("button", { name: "下一周" })).toBeDisabled();

    await userEvent.click(screen.getByRole("button", { name: "上一周" }));
    expect(await screen.findByRole("link", { name: "上周的事" })).toBeInTheDocument();
    expect(screen.getByText("进展 0 条 · 完成 1 项 · 涉及项目 1 个")).toBeInTheDocument();
  });

  it("进展维度（同工作台本周回顾）：这周有进展的事也列出、带每天的进展；同一件事同一天的几条算一条；进展日按所指日期，没写的按记录那天（主机时区）", async () => {
    const items = [mk("a", { project: "p2", title: "评审方案" })];
    const at = { a: "2030-01-09T03:00:00Z" };
    const moved = { "2030-01-07": {
      items: [mk("a", { project: "p2", title: "评审方案" }), mk("o", { project: "p2", phase: "open", title: "对接银行" }), mk("x", { phase: "waiting", title: "等报价" }), mk("b", { kind: "brief", title: "早间简报" })],
      progress: {
        a: [{ id: "a.1", text: "初稿给到业务方", at: "2030-01-08T01:00:00Z" }],
        o: [
          { id: "o.1", text: "联调通过\n细节：三个接口", at: "2030-01-09T02:00:00Z" },
          { id: "o.2", text: "背景：上线窗口", at: "2030-01-09T02:00:01Z" }, // 同一天：并成一条
          { id: "o.3", text: "补记周一的会", on: "2030-01-07", at: "2030-01-09T03:00:00Z" },
        ],
        x: [{ id: "x.1", text: "发了询价", at: "2030-01-07T16:30:00Z" }], // 主机时间 01-08 00:30
        b: [{ id: "b.1", text: "简报", at: "2030-01-08T00:00:00Z" }],
      },
    } };
    mount(server(items, at, moved));
    expect(await screen.findByText("进展 4 条 · 完成 1 项 · 涉及项目 1 个")).toBeInTheDocument();
    const work = screen.getByRole("region", { name: "工作" });
    expect(within(work).getByText("进展 3 条 · 完成 1 项")).toBeInTheDocument();
    expect(within(work).getAllByRole("link").map((l) => l.textContent)).toEqual(["工作", "评审方案", "对接银行"]); // 最近动过的在前
    const bank = within(work).getByRole("list", { name: "对接银行 这周的进展" });
    expect(within(bank).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["周一 01-07 补记周一的会", "周三 01-09 联调通过；背景：上线窗口"]);
    expect(within(work).getByText("进行中")).toBeInTheDocument(); // 没完成的写现在的阶段
    const none = screen.getByRole("region", { name: "没归项目的" });
    expect(within(within(none).getByRole("list", { name: "等报价 这周的进展" })).getByRole("listitem")).toHaveTextContent("周二 01-08 发了询价");
    expect(screen.queryByText("早间简报")).toBeNull();
  });

  it("完成日期不详（迁移来、没记完成日）：不算在任何一周、不计入完成数，单独成一节如实标出；这周有进展时行上写不详而不是写阶段", async () => {
    const items = [mk("a", { title: "交电费" }), mk("u", { title: "看牙" }), mk("v", { title: "发版" })];
    const moved = { "2030-01-07": { items: [mk("v", { title: "发版" })], progress: { v: [{ id: "v.1", text: "补记", on: "2030-01-08", at: "2030-01-09T03:00:00Z" }] } } };
    mount(server(items, { a: "2030-01-09T01:00:00Z" }, moved, ["u", "v"]));
    expect(await screen.findByText("进展 1 条 · 完成 1 项 · 涉及项目 0 个")).toBeInTheDocument();
    const sec = screen.getByRole("region", { name: "完成日期不详" });
    expect(within(sec).getByText("2 件从工作台迁移来的事已完成，但没有记下完成那天，不算在任何一周里。")).toBeInTheDocument();
    expect(within(sec).getAllByRole("link").map((l) => l.textContent)).toEqual(["看牙", "发版"]);
    expect(screen.getByRole("link", { name: "看牙" })).toHaveAttribute("href", "/items/u");
    const row = screen.getAllByRole("link", { name: "发版" })[0].closest("li")!;
    expect(row).toHaveTextContent("完成日期不详");
    await userEvent.click(screen.getByRole("button", { name: "上一周" }));
    expect(await screen.findByText("这一周没有完成的事")).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "完成日期不详" })).getAllByRole("link")).toHaveLength(2);
  });

  it("空态：本周没有完成的事时说明会记在哪，并给看上一周的入口；过去的周写这一周没有", async () => {
    mount(server([], {}));
    expect(await screen.findByText(/这周还没有完成的事/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "看上一周" }));
    expect(await screen.findByText("这一周没有完成的事")).toBeInTheDocument();
  });
});
