import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 事项列表 = 工作台任务库的对等页：按项目分组、先阶段后优先级的默认排序、行上排时间 / 交给 Agent / 完成，写完原位反映。
// 替身按 api/items 的契约应答：GET /items、GET /items/{id}、PATCH（If-Match）、POST transition。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
type It = Record<string, any>;
const mk = (id: string, over: It = {}): It => ({ id, phase: "open", version: 1, kind: "task", title: "事项 " + id, ...over });

function server(list: It[]) {
  const items = new Map(list.map((i) => [i.id, { ...i }]));
  const calls: { method: string; path: string; body?: any; ifMatch?: string }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, ifMatch: (init.headers as Record<string, string> | undefined)?.["If-Match"] });
    if (url === "/v1/projects") return json(200, { projects: [{ id: "p-work", name: "工作" }, { id: "p-home", name: "家务" }] });
    if (url === "/v1/items" || url.startsWith("/v1/items?")) return json(200, { items: [...items.values()] });
    const m = /^\/v1\/items\/([^/]+)(\/transition|\/progress)?$/.exec(url);
    const it = m && items.get(m[1]);
    if (!it) return json(404, { code: "not_found", why: "no route", next: "x" });
    if (method === "GET") return json(200, { item: it, relations: [] });
    if (m![2] === "/progress") return json(201, { id: it.id });
    if (m![2]) it.phase = body.to;
    else for (const [k, v] of Object.entries(body)) if (k !== "reason") it[k] = v;
    it.version++;
    return json(200, { id: it.id, version: it.version });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function Where() {
  return <output data-testid="where">{useLocation().search}</output>;
}
function mount(f: typeof fetch, path = "/items") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={[path]}><App features={[feature]} fetchImpl={f} /><Where /></MemoryRouter>);
}
const writes = (s: ReturnType<typeof server>) => s.calls.filter((c) => c.method !== "GET");
const titles = (region: HTMLElement) => within(region).getAllByRole("listitem").map((li) => li.querySelector("a")!.textContent);
beforeEach(() => sessionStorage.clear());

describe("事项列表（对齐工作台任务库）", () => {
  it("按项目分组，组按项目顺序、没归项目的排最后；组内先阶段（进行中→等你决定→在等→建议→已完成）再优先级", async () => {
    mount(server([
      mk("h1", { project: "p-home", title: "交电费" }),
      mk("w-done", { project: "p-work", phase: "done", title: "已交的周报" }),
      mk("w-low", { project: "p-work", priority: "low", title: "整理书签" }),
      mk("w-wait", { project: "p-work", phase: "waiting", title: "等报价" }),
      mk("w-high", { project: "p-work", priority: "high", title: "修线上问题" }),
      mk("w-ask", { project: "p-work", phase: "needs_input", title: "选供应商" }),
      mk("w-new", { project: "p-work", phase: "proposed", title: "新建议" }),
      mk("n1", { title: "看牙" }),
    ]).impl);
    const list = await screen.findByRole("group", { name: "事项列表" });
    await waitFor(() => expect(within(list).getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["工作（6）", "家务（1）", "未归项目（1）"]));
    expect(titles(within(list).getByRole("region", { name: "工作" }))).toEqual(["修线上问题", "整理书签", "选供应商", "等报价", "新建议", "已交的周报"]);
  });

  it("选了别的排序：仍按项目分组，组内照服务端给的顺序", async () => {
    mount(server([mk("a", { project: "p-work", priority: "low", title: "甲" }), mk("b", { project: "p-work", priority: "high", title: "乙" })]).impl, "/items?sort=updated");
    const work = await screen.findByRole("region", { name: "工作" });
    expect(titles(work)).toEqual(["甲", "乙"]);
  });

  it("行上完成：发 transition（带 If-Match 与在事项列表完成的理由），只重读这一件、不重取列表；按新阶段就地重排到组尾，显示已完成、按钮消失", async () => {
    const s = server([mk("a", { project: "p-work", version: 4, title: "甲" }), mk("b", { project: "p-work", title: "乙" })]);
    mount(s.impl);
    const user = userEvent.setup();
    const row = (await screen.findByRole("link", { name: "甲" })).closest("li")!;
    await user.click(within(row).getByRole("button", { name: "完成" }));
    await waitFor(() => expect(writes(s)).toEqual([{ method: "POST", path: "/v1/items/a/transition", body: { to: "done", reason: "在事项列表完成" }, ifMatch: "4" }]));
    await waitFor(() => expect(titles(screen.getByRole("region", { name: "工作" }))).toEqual(["乙", "甲"])); // 已完成排到组尾
    const done = screen.getByRole("link", { name: "甲" }).closest("li")!;
    expect(within(done).getByText("已完成")).toBeInTheDocument();
    expect(within(done).queryByRole("button")).toBeNull();
    expect(s.calls.filter((c) => c.path === "/v1/items")).toHaveLength(1);
    expect(s.calls.filter((c) => c.path === "/v1/items/a")).toEqual([{ method: "GET", path: "/v1/items/a", body: undefined, ifMatch: undefined }]);
  });

  it("行上记进展：与今天页同一个按钮与输入框，写一次进展、只重读这一件，行还在", async () => {
    const s = server([mk("a", { project: "p-work", title: "甲" })]);
    mount(s.impl);
    const user = userEvent.setup();
    const row = (await screen.findByRole("link", { name: "甲" })).closest("li")!;
    expect(within(row).getAllByRole("button").map((b) => b.textContent)).toEqual(["排时间", "交给 Agent", "完成", "记进展"]);
    await user.click(within(row).getByRole("button", { name: "记进展" }));
    await user.type(within(within(row).getByRole("group", { name: "记进展：甲" })).getByLabelText("进展"), "联调通过");
    await user.click(within(row).getByRole("button", { name: "保存" }));
    await waitFor(() => expect(writes(s)).toEqual([{ method: "POST", path: "/v1/items/a/progress", body: { text: "联调通过" }, ifMatch: undefined }]));
    await waitFor(() => expect(within(row).queryByRole("group", { name: "记进展：甲" })).toBeNull());
    expect(s.calls.filter((c) => c.path === "/v1/items")).toHaveLength(1);
  });

  it("行上排时间：只填日期不定时，写计划日期一项；行上原位显示新计划；建议与已结束的事没有行内操作", async () => {
    const s = server([mk("a", { project: "p-work", title: "甲" }), mk("p", { project: "p-work", phase: "proposed", title: "建议" }), mk("d", { project: "p-work", phase: "dropped", title: "不做的" })]);
    mount(s.impl);
    const user = userEvent.setup();
    const row = (await screen.findByRole("link", { name: "甲" })).closest("li")!;
    await user.click(within(row).getByRole("button", { name: "排时间" }));
    const form = within(row).getByRole("form", { name: "排时间：甲" });
    await user.clear(within(form).getByLabelText("计划日期"));
    await user.type(within(form).getByLabelText("计划日期"), "2031-02-03");
    await user.click(within(form).getByRole("button", { name: "确定" }));
    await waitFor(() => expect(writes(s)).toEqual([{ method: "PATCH", path: "/v1/items/a", body: { planned_for: "2031-02-03", reason: "在事项列表排时间" }, ifMatch: "1" }]));
    expect(await within(row).findByText(/计划 2031-02-03/)).toBeInTheDocument();
    for (const name of ["建议", "不做的"]) expect(within(screen.getByRole("link", { name }).closest("li")!).queryByRole("button")).toBeNull();
  });

  it("版本对不上（409）：行上显示服务端原因并重读这一件，不留本地假状态", async () => {
    const s = server([mk("a", { project: "p-work", title: "甲" })]);
    const conflict = (async (url: string, init: RequestInit = {}) =>
      (init.method === "POST" ? json(409, { code: "conflict", why: "version moved", next: "reload" }) : s.impl(url, init))) as unknown as typeof fetch;
    mount(conflict);
    const row = (await screen.findByRole("link", { name: "甲" })).closest("li")!;
    await userEvent.click(within(row).getByRole("button", { name: "完成" }));
    expect(await within(row).findByRole("alert")).toHaveTextContent("version moved");
    expect(within(row).getByRole("button", { name: "完成" })).toBeInTheDocument();
  });

  it("筛选与页码写在地址里：从地址带进来的筛选生效，改筛选写回地址并回到第一页", async () => {
    mount(server([mk("a", { project: "p-work", title: "甲" }), mk("b", { project: "p-home", title: "乙" })]).impl, "/items?project=p-home&page=2");
    expect(await screen.findByRole("link", { name: "乙" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "甲" })).toBeNull();
    await userEvent.selectOptions(screen.getByLabelText("项目"), "");
    expect(screen.getByTestId("where").textContent).toBe("");
    await userEvent.selectOptions(screen.getByLabelText("项目"), "p-work");
    expect(screen.getByTestId("where").textContent).toBe("?project=p-work");
  });
});
