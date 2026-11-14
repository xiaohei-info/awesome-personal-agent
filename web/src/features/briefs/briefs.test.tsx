import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { browserZone } from "./schedule";

// 测试边界替身：按 GET /v1/items?kind=brief、GET /v1/instances、GET /v1/packs 与 POST /v1/instances 契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const items = [
  { id: "b1", kind: "brief", title: "周一简报", created_at: "2030-01-01T00:00:00Z", spec: { period: "2030-W01", sections: [{ project: "home", body: "家里进展" }, { project: "work", body: "工作进展" }] } },
  { id: "b2", kind: "brief", title: "周复盘", created_at: "2030-01-07T00:00:00Z", spec: { period: "2030-W02-review", sections: [{ project: "work", body: "复盘工作" }] } },
  { id: "b3", kind: "brief", title: "空简报", created_at: "2030-01-08T00:00:00Z", spec: {} },
];
const packs = [{ name: "brief", instance_params: {} }, { name: "review", instance_params: {} }, { name: "follow-up", instance_params: {} }];
const routines = [{ id: "ins_r", pack: "routine", params: { schedule: "CRON_TZ=UTC 0 9 * * 1", mode: "create_item", title: "{date} 周报" }, enabled: true }];
const instances = [{ id: "ins_1", pack: "brief", params: { schedule: "0 8 * * 1-5" }, enabled: true }, { id: "ins_2", pack: "follow-up", params: {}, enabled: true }];

interface Calls { posts: { body: unknown; csrf: string | null }[]; urls: string[]; patches?: { url: string; body: unknown }[] }
function server(o: { fail?: boolean; items?: unknown[]; reject?: boolean; routine?: boolean; host?: string; caps?: Record<string, string> } = {}, calls: Calls = { posts: [], urls: [] }) {
  const f = (async (url: string, init?: RequestInit) => {
    calls.urls.push(url);
    if (url === "/v1/projects") return json(200, { projects: [{ id: "home", name: "家务" }, { id: "work", name: "工作" }] });
    if (o.fail) return json(500, { code: "internal", why: "ledger unavailable", next: "retry" });
    if (url === "/v1/items?kind=brief") return json(200, { items: o.items ?? items });
    if (url === "/v1/packs") return json(200, { packs: o.routine ? [...packs, { name: "routine", instance_params: {} }] : packs });
    if (url === "/v1/settings/policy") return json(200, { version: 1, default_harness: o.host ?? "" });
    if (url === "/v1/settings/access-limits") return json(200, { version: 1, permission_cap: { default: "read", by_harness: o.caps ?? {} } });
    const ip = url.match(/^\/v1\/instances\/([^/]+)$/);
    if (ip && init?.method === "PATCH") {
      const body = JSON.parse(init.body as string);
      (calls.patches ??= []).push({ url, body });
      const cur = [...instances, ...routines].find((x) => x.id === decodeURIComponent(ip[1]))!;
      return json(200, { ...cur, enabled: body.enabled });
    }
    if (url === "/v1/instances" && init?.method === "POST") {
      calls.posts.push({ body: JSON.parse(init.body as string), csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] ?? null });
      if (o.reject) return json(400, { code: "invalid_request", why: "schedule is not a valid cron expression", next: "fix the request and retry" });
      const b = JSON.parse(init.body as string);
      return json(201, { id: "ins_3", pack: b.pack, ...(b.project ? { project: b.project } : {}), params: b.params, enabled: true });
    }
    if (url === "/v1/instances") return json(200, { instances: o.routine ? [...instances, ...routines] : instances });
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return f;
}
function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={["/briefs"]}><App features={[feature]} fetchImpl={f} /></MemoryRouter>);
}

beforeEach(() => sessionStorage.clear());

describe("简报页", () => {
  it("flow-brief: 每份简报按项目分节显示正文", async () => {
    mount(server());
    const a = within(await screen.findByLabelText("简报 周一简报"));
    // 分节标题是项目名：名字异步取到后替换 id，等名字出现再断言，避免与加载先后赛跑。
    expect(await a.findByRole("heading", { name: "家务" })).toBeInTheDocument();
    expect(a.getByText("家里进展")).toBeInTheDocument();
    expect(await a.findByRole("heading", { name: "工作" })).toBeInTheDocument();
    expect(a.getByText("工作进展")).toBeInTheDocument();
    expect(a.getByText(/2030-W01/)).toBeInTheDocument();
  });

  it("简报与复盘（同为 brief 事项）合并在同一列表，不按猜测的后缀分栏", async () => {
    mount(server());
    const list = within(await screen.findByRole("region", { name: "简报" }));
    expect(list.getByLabelText("简报 周一简报")).toBeInTheDocument();
    expect(list.getByLabelText("简报 周复盘")).toBeInTheDocument();
    expect(list.getByText("复盘工作")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "复盘" })).not.toBeInTheDocument();
  });

  it("没有分节内容的简报显示空状态", async () => {
    mount(server());
    expect(within(await screen.findByLabelText("简报 空简报")).getByText("暂无分节内容")).toBeInTheDocument();
  });

  it("没有任何简报时显示空状态", async () => {
    mount(server({ items: [] }));
    expect(await screen.findByText("还没有简报")).toBeInTheDocument();
  });

  it("flow-brief: 只列出简报类能力包的实例；不带时区的定时原样显示", async () => {
    mount(server());
    const l = within(await screen.findByRole("list", { name: "已安排的" }));
    expect(l.getAllByRole("listitem")).toHaveLength(1);
    expect(l.getByText("0 8 * * 1-5")).toBeInTheDocument();
  });

  it("flow-brief: 建能力包实例：POST /v1/instances 带包、定时与项目，成功后列出新实例", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({}, calls));
    await screen.findByRole("list", { name: "已安排的" });
    await userEvent.selectOptions(screen.getByLabelText("做什么"), "review");
    await userEvent.selectOptions(screen.getByLabelText("哪几天"), "每周五");
    const at = screen.getByLabelText("几点");
    await userEvent.clear(at);
    await userEvent.type(at, "09:00");
    await userEvent.click(await screen.findByLabelText("家务"));
    await userEvent.click(screen.getByLabelText("工作"));
    await userEvent.click(screen.getByRole("button", { name: "安排" }));
    // 定时带上这台设备的时区，避免 APA 主机时区不同时错开（§6.8）
    const zone = browserZone();
    expect(calls.posts).toEqual([{ body: { pack: "review", params: { schedule: `CRON_TZ=${zone} 0 9 * * 5`, projects: ["home", "work"] } }, csrf: "tok-1" }]);
    const l = within(screen.getByRole("list", { name: "已安排的" }));
    expect(await l.findByText(/每周五 09:00 · 复盘 · 家务、工作/)).toBeInTheDocument();
  });

  it("建实例被服务端拒绝时显示原因，不伪造新实例", async () => {
    mount(server({ reject: true }));
    await screen.findByRole("list", { name: "已安排的" });
    await userEvent.type(screen.getByLabelText("cron 或时长"), "bad"); // 高级里直接写的定时原样提交
    await userEvent.click(screen.getByRole("button", { name: "安排" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("schedule is not a valid cron expression");
    expect(screen.queryByText(/ins_3/)).not.toBeInTheDocument();
  });

  it("还没有简报也没安排时，给出「每天早上给我一份」：只预填，本人再点「安排」才创建", async () => {
    const calls: Calls = { posts: [], urls: [] };
    const f = (async (url: string, init?: RequestInit) =>
      url === "/v1/items?kind=brief" ? json(200, { items: [] }) : url === "/v1/instances" && init?.method !== "POST" ? json(200, { instances: [] }) : server({}, calls)(url, init)) as unknown as typeof fetch;
    mount(f);
    await userEvent.click(await screen.findByRole("button", { name: "每天早上给我一份" }));
    expect(calls.posts).toEqual([]);
    expect(screen.getByLabelText("哪几天")).toHaveValue("*");
    expect(screen.getByLabelText("几点")).toHaveValue("08:00");
    await userEvent.click(screen.getByRole("button", { name: "安排" }));
    expect(calls.posts).toEqual([{ body: { pack: "brief", params: { schedule: `CRON_TZ=${browserZone()} 0 8 * * *` } }, csrf: "tok-1" }]);
  });

  it("没选时间时不发请求", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({}, calls));
    await screen.findByRole("list", { name: "已安排的" });
    await userEvent.clear(screen.getByLabelText("几点"));
    await userEvent.click(screen.getByRole("button", { name: "安排" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("请选好时间");
    expect(calls.posts).toEqual([]);
  });

  it("启停：每个实例一个开关，只发 enabled，界面按服务端返回更新", async () => {
    const calls: Calls = { posts: [], urls: [], patches: [] };
    mount(server({ routine: true }, calls));
    const l = within(await screen.findByRole("list", { name: "已安排的" }));
    await userEvent.click(l.getByRole("button", { name: "启停 ins_1" }));
    expect(calls.patches).toEqual([{ url: "/v1/instances/ins_1", body: { enabled: false } }]);
    expect(await l.findByText(/（已停用）/)).toBeInTheDocument();
    expect(l.getByRole("button", { name: "启停 ins_1" })).toHaveTextContent("启用");
    const r = within(screen.getByRole("list", { name: "已安排的例行任务" }));
    await userEvent.click(r.getByRole("button", { name: "启停 {date} 周报" }));
    expect(calls.patches!.at(-1)).toEqual({ url: "/v1/instances/ins_r", body: { enabled: false } });
  });

  it("例行任务：默认只记一条事项，POST 只带 schedule、mode 与填了的标题、项目", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({ routine: true }, calls));
    const form = within(await screen.findByRole("form", { name: "新的例行任务" }));
    expect(form.queryByLabelText("要它做的事")).not.toBeInTheDocument();
    await userEvent.type(form.getByLabelText("标题（可写 {date}）"), "交水电费");
    await userEvent.selectOptions(form.getByLabelText("哪几天做"), "1");
    await userEvent.selectOptions(await form.findByRole("combobox", { name: "归到哪个项目" }), "home");
    await userEvent.click(form.getByRole("button", { name: "安排例行任务" }));
    expect(calls.posts).toEqual([{ body: { pack: "routine", project: "home", params: { schedule: `CRON_TZ=${browserZone()} 0 9 * * 1`, mode: "create_item", title: "交水电费" } }, csrf: "tok-1" }]);
    expect(await within(screen.getByRole("list", { name: "已安排的例行任务" })).findByText(/交水电费 · 每周一 09:00 · 家务/)).toBeInTheDocument();
  });

  it("例行任务让 agent 去做：任务说明必填；带上权限档与结果去处；超出默认宿主上限时提示后果", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({ routine: true, host: "hermes", caps: { hermes: "edit" } }, calls));
    const form = within(await screen.findByRole("form", { name: "新的例行任务" }));
    await userEvent.click(form.getByRole("radio", { name: "让 agent 去做" }));
    await userEvent.click(form.getByRole("button", { name: "安排例行任务" }));
    expect(await form.findByRole("alert")).toHaveTextContent("请写清要它做的事");
    expect(calls.posts).toEqual([]);
    await userEvent.type(form.getByLabelText("要它做的事"), "按本周完成的事写周报草稿");
    await userEvent.selectOptions(form.getByLabelText("后台权限"), "full");
    expect(await form.findByText(/超出宿主的上限/)).toBeInTheDocument();
    await userEvent.selectOptions(form.getByLabelText("后台权限"), "edit");
    expect(form.queryByText(/超出宿主的上限/)).not.toBeInTheDocument();
    await userEvent.selectOptions(form.getByLabelText("结果写到哪"), "item_and_material");
    await userEvent.click(form.getByRole("button", { name: "安排例行任务" }));
    expect(calls.posts).toEqual([{ body: { pack: "routine", params: { schedule: `CRON_TZ=${browserZone()} 0 9 * * 1`, mode: "run", instruction: "按本周完成的事写周报草稿", access: "edit", output: "item_and_material" } }, csrf: "tok-1" }]);
  });

  it("没设默认宿主时，选「让 agent 去做」提示到点会停下来问你，并给去设置的入口", async () => {
    mount(server({ routine: true }));
    const form = within(await screen.findByRole("form", { name: "新的例行任务" }));
    await userEvent.click(form.getByRole("radio", { name: "让 agent 去做" }));
    expect(await form.findByText(/还没设默认宿主/)).toBeInTheDocument();
    expect(form.getAllByRole("link", { name: "去设置" })[0]).toHaveAttribute("href", "/settings/access");
  });

  it("服务端失败时显示真实原因", async () => {
    mount(server({ fail: true }));
    expect(await screen.findByRole("alert")).toHaveTextContent("ledger unavailable");
  });
});
