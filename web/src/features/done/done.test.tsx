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
function server(items: Item[], at: Record<string, string>) {
  return (async (url: string) => {
    if (url === "/v1/projects") return json({ projects: [{ id: "p1", name: "家务" }, { id: "p2", name: "工作" }] });
    if (url === "/v1/items?phase=done") return json({ items, last_rejections: {}, done_at: at });
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
    expect(await screen.findByText("共 4 件，分属 3 个项目")).toBeInTheDocument();
    const groups = within(screen.getByRole("main")).getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(groups).toEqual(["工作 2 件", "家务 1 件", "没归项目的 1 件"]);
    const work = screen.getByRole("region", { name: "工作" });
    expect(within(work).getAllByRole("link").map((l) => l.textContent)).toEqual(["工作", "发周报", "评审方案"]);
    expect(within(work).getByText(/周三 01-09 · 委派/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "交电费" })).toHaveAttribute("href", "/items/a");
    expect(screen.queryByText("早间简报")).toBeNull();
    expect(screen.getByRole("button", { name: "下一周" })).toBeDisabled();

    await userEvent.click(screen.getByRole("button", { name: "上一周" }));
    expect(await screen.findByRole("link", { name: "上周的事" })).toBeInTheDocument();
    expect(screen.getByText("共 1 件")).toBeInTheDocument();
  });

  it("空态：本周没有完成的事时说明会记在哪，并给看上一周的入口；过去的周写这一周没有", async () => {
    mount(server([], {}));
    expect(await screen.findByText(/这周还没有完成的事/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "看上一周" }));
    expect(await screen.findByText("这一周没有完成的事")).toBeInTheDocument();
  });
});
