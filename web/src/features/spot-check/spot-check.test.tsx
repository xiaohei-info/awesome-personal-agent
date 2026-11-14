import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/judgments 的 GET /v1/judgments/spot-check、POST /v1/judgments/{id}/review 与
// api/settings/policy 的 /v1/settings/judgment（按段替换）应答。错误体见附录 B.3。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function backend(o: { pool?: { judgment: string; subject?: string }[] } = {}) {
  let settings = { bands: [{ template: "triage-screen", low: 0.1, high: 0.6 }], shadow: ["triage-screen"], spot_check: { per_brief: 5 } };
  const pool = [...(o.pool ?? [])];
  const calls: { method: string; path: string; body?: any }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/sources") return json(200, { sources: [{ id: "mail" }] });
    if (url === "/v1/judgments/spot-check")
      return json(200, { spot_check: pool.map((p) => ({ ...p, event: "ev_" + p.judgment, source: "mail", template: "triage-screen", judged_at: "2030-01-01T08:00:00Z", web_path: "/judgments/spot-check?judgment=" + p.judgment, results: ["should_see", "correct"] })) });
    const rv = /^\/v1\/judgments\/([^/]+)\/review$/.exec(url);
    if (rv && method === "POST") {
      pool.splice(pool.findIndex((p) => p.judgment === rv[1]), 1);
      return json(200, { judgment: rv[1], result: body.result });
    }
    if (url === "/v1/settings/judgment") {
      if (method === "GET") return json(200, settings);
      settings = { ...settings, ...body };
      return json(200, settings);
    }
    return json(404, { code: "not_found", why: "no such route", next: "" });
  }) as unknown as typeof fetch;
  return { impl, calls, get settings() { return settings; } };
}

function mount(fetchImpl: typeof fetch, at = "/spot-check") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[at]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
beforeEach(() => sessionStorage.clear());

describe("消息来源 › 抽查漏掉的", () => {
  // ACT-JRC-17..19：逐条给结论，结论发给服务端，条目消失。
  it("每条显示标题、来源与本地时间，两个按钮；点「该看」记结论并移走", async () => {
    const be = backend({ pool: [{ judgment: "j1", subject: "续保材料" }, { judgment: "j2", subject: "周末活动" }] });
    mount(be.impl);
    const list = await screen.findByRole("list", { name: "待抽查" });
    const first = within(within(list).getAllByRole("listitem")[0]);
    expect(first.getByText("续保材料")).toBeInTheDocument();
    expect(first.getByText(/^mail · /)).toBeInTheDocument();
    expect(first.queryByText(/triage-screen|2030-01-01T/)).not.toBeInTheDocument();
    await userEvent.click(first.getByRole("button", { name: "该看（转成建议） 续保材料" }));
    await waitFor(() => expect(within(screen.getByRole("list", { name: "待抽查" })).getAllByRole("listitem")).toHaveLength(1));
    expect(be.calls.find((c) => c.method === "POST")).toMatchObject({ path: "/v1/judgments/j1/review", body: { result: "should_see" } });
    expect(screen.getByRole("status")).toHaveTextContent("「续保材料」：该看");
    await userEvent.click(screen.getByRole("button", { name: "确实不用 周末活动" }));
    expect(await screen.findByText("都看过了")).toBeInTheDocument();
  });

  it("从简报的抽检链接进来（?judgment=）时标出那一条", async () => {
    mount(backend({ pool: [{ judgment: "j1", subject: "a" }, { judgment: "j2", subject: "b" }] }).impl, "/spot-check?judgment=j2");
    const list = await screen.findByRole("list", { name: "待抽查" });
    expect(within(list).getAllByRole("listitem").map((li) => li.className)).toEqual(["", "on"]);
  });

  // 只写 spot_check 一段：不确定带与试用不受影响。
  it("每份简报抽几条：只写 spot_check 这一段", async () => {
    const be = backend();
    mount(be.impl);
    const n = await screen.findByLabelText(/每份简报抽/);
    await waitFor(() => expect(n).toHaveValue(5));
    await userEvent.clear(n);
    await userEvent.type(n, "3");
    await userEvent.click(screen.getByRole("button", { name: "保存" }));
    expect(await screen.findByText("已改为每份简报抽 3 条")).toBeInTheDocument();
    expect(be.calls.filter((c) => c.method === "PUT").map((c) => c.body)).toEqual([{ spot_check: { per_brief: 3 } }]);
    expect(be.settings.bands).toHaveLength(1);
    expect(be.settings.shadow).toEqual(["triage-screen"]);
  });

  it("条数不是非负整数时不发请求", async () => {
    const be = backend();
    mount(be.impl);
    const n = await screen.findByLabelText(/每份简报抽/);
    await userEvent.clear(n);
    await userEvent.click(screen.getByRole("button", { name: "保存" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("请填不小于 0 的整数");
    expect(be.calls.filter((c) => c.method === "PUT")).toEqual([]);
  });
});
