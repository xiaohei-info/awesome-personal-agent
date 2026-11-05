import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 GET /v1/items?kind=brief、GET /v1/instances、GET /v1/packs 与 POST /v1/instances 契约应答。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const items = [
  { id: "b1", kind: "brief", title: "周一简报", created_at: "2030-01-01T00:00:00Z", spec: { period: "2030-W01", sections: [{ project: "home", body: "家里进展" }, { project: "work", body: "工作进展" }] } },
  { id: "b2", kind: "brief", title: "周复盘", created_at: "2030-01-07T00:00:00Z", spec: { period: "2030-W02-review", sections: [{ project: "work", body: "复盘工作" }] } },
  { id: "b3", kind: "brief", title: "空简报", created_at: "2030-01-08T00:00:00Z", spec: {} },
];
const packs = [{ name: "brief", instance_params: {} }, { name: "review", instance_params: {} }, { name: "follow-up", instance_params: {} }];
const instances = [{ id: "ins_1", pack: "brief", params: { schedule: "0 8 * * 1-5" }, enabled: true }, { id: "ins_2", pack: "follow-up", params: {}, enabled: true }];

interface Calls { posts: { body: unknown; csrf: string | null }[]; urls: string[] }
function server(o: { fail?: boolean; items?: unknown[]; reject?: boolean } = {}, calls: Calls = { posts: [], urls: [] }) {
  const f = (async (url: string, init?: RequestInit) => {
    calls.urls.push(url);
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (o.fail) return json(500, { code: "internal", why: "ledger unavailable", next: "retry" });
    if (url === "/v1/items?kind=brief") return json(200, { items: o.items ?? items });
    if (url === "/v1/packs") return json(200, { packs });
    if (url === "/v1/instances" && init?.method === "POST") {
      calls.posts.push({ body: JSON.parse(init.body as string), csrf: (init.headers as Record<string, string>)["X-CSRF-Token"] ?? null });
      if (o.reject) return json(400, { code: "invalid_request", why: "schedule is not a valid cron expression", next: "fix the request and retry" });
      return json(201, { id: "ins_3", pack: "review", params: { schedule: "0 9 * * 5" }, enabled: true });
    }
    if (url === "/v1/instances") return json(200, { instances });
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
    expect(a.getByRole("heading", { name: "home" })).toBeInTheDocument();
    expect(a.getByText("家里进展")).toBeInTheDocument();
    expect(a.getByRole("heading", { name: "work" })).toBeInTheDocument();
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

  it("flow-brief: 只列出简报类能力包的实例", async () => {
    mount(server());
    const l = within(await screen.findByRole("list", { name: "实例" }));
    expect(l.getByText(/ins_1/)).toBeInTheDocument();
    expect(l.getByText(/0 8 \* \* 1-5/)).toBeInTheDocument();
    expect(l.queryByText(/ins_2/)).not.toBeInTheDocument();
  });

  it("flow-brief: 建能力包实例：POST /v1/instances 带包、定时与项目，成功后列出新实例", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({}, calls));
    await screen.findByRole("list", { name: "实例" });
    await userEvent.selectOptions(screen.getByLabelText("能力包"), "review");
    await userEvent.type(screen.getByLabelText("定时"), "0 9 * * 5");
    await userEvent.type(screen.getByLabelText("项目（逗号分隔，可空）"), "home, work");
    await userEvent.click(screen.getByRole("button", { name: "创建实例" }));
    expect(await screen.findByText(/ins_3/)).toBeInTheDocument();
    expect(calls.posts).toEqual([{ body: { pack: "review", params: { schedule: "0 9 * * 5", projects: ["home", "work"] } }, csrf: "tok-1" }]);
  });

  it("建实例被服务端拒绝时显示原因，不伪造新实例", async () => {
    mount(server({ reject: true }));
    await screen.findByRole("list", { name: "实例" });
    await userEvent.type(screen.getByLabelText("定时"), "bad");
    await userEvent.click(screen.getByRole("button", { name: "创建实例" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("schedule is not a valid cron expression");
    expect(screen.queryByText(/ins_3/)).not.toBeInTheDocument();
  });

  it("定时为空时不发请求", async () => {
    const calls: Calls = { posts: [], urls: [] };
    mount(server({}, calls));
    await screen.findByRole("list", { name: "实例" });
    await userEvent.click(screen.getByRole("button", { name: "创建实例" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("请填写定时");
    expect(calls.posts).toEqual([]);
  });

  it("服务端失败时显示真实原因", async () => {
    mount(server({ fail: true }));
    expect(await screen.findByRole("alert")).toHaveTextContent("ledger unavailable");
  });
});
