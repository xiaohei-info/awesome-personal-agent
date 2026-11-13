import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/judgments 与 api/settings/policy 的契约应答（stats 的窗口与过滤、抽检池与 review、
// /settings/judgment 的整段替换与 low>high 的 400）。业务规则在服务端，这里只复现它的可观察行为。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix the request" });

interface Band { template: string; source?: string; low: number; high: number }
function backend(o: { pool?: { judgment: string; subject: string }[] } = {}) {
  let settings = { bands: [] as Band[], shadow: [] as string[], spot_check: { per_brief: 5 } };
  const pool = [...(o.pool ?? [])];
  const reviewed: Record<string, string> = {};
  const calls: { method: string; path: string; body?: any }[] = [];
  const statsFor = (since: string) => {
    const n = since === "7d" ? 4 : since === "30d" ? 10 : 40;
    const rev = Object.values(reviewed);
    return {
      rows: [
        { use: "inbound_screen", template: "triage-screen", source: "src_a", backend: "classifier", calls: n, decided: n - 2, uncertain: 2, errors: 0, reused: 0 },
        { use: "inbound_screen", template: "triage-screen", source: "src_a", backend: "chat", calls: 2, decided: 2, uncertain: 0, errors: 0, reused: 0 },
      ],
      rates: [{ use: "inbound_screen", template: "triage-screen", source: "src_a", escalation: 0.25, undecided: 0.1, acceptance: 0.5, false_drop: rev.length ? rev.filter((r) => r === "should_see").length / rev.length : 0, reviewed: rev.length }],
      histogram: [{ use: "inbound_screen", template: "triage-screen", source: "src_a", low: 0.0, high: 0.1, count: n }, { use: "inbound_screen", template: "triage-screen", source: "src_a", low: 0.9, high: 1.0, count: 3 }],
    };
  };
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body });
    const u = new URL(url, "http://x");
    if (u.pathname === "/v1/projects") return json(200, { projects: [] });
    if (u.pathname === "/v1/sources") return json(200, { sources: [{ id: "src_a" }, { id: "src_b" }] });
    if (u.pathname === "/v1/judgments/stats") {
      const s = statsFor(u.searchParams.get("since")!);
      const src = u.searchParams.get("source");
      return json(200, src && src !== "src_a" ? { rows: [], rates: [], histogram: [] } : s);
    }
    if (u.pathname === "/v1/judgments/spot-check") return json(200, { spot_check: pool.map((p) => ({ ...p, event: "ev_" + p.judgment, source: "src_a", template: "triage-screen", judged_at: "2030-01-01T00:00:00Z", results: ["should_see", "correct"] })) });
    const rv = u.pathname.match(/^\/v1\/judgments\/([^/]+)\/review$/);
    if (rv && method === "POST") {
      const i = pool.findIndex((p) => p.judgment === rv[1]);
      if (i < 0) return err(404, "not_found", "judgment not found");
      reviewed[rv[1]] = body.result;
      pool.splice(i, 1);
      return json(200, { id: rv[1] });
    }
    if (u.pathname === "/v1/settings/judgment") {
      if (method === "GET") return json(200, settings);
      for (const b of body.bands ?? []) if (b.low > b.high) return err(400, "invalid_request", "band low must not be greater than high");
      settings = { ...settings, ...body };
      return json(200, settings);
    }
    return err(404, "not_found", "no such route " + u.pathname);
  }) as unknown as typeof fetch;
  return { impl, calls, get settings() { return settings; }, reviewed };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/judgment"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}
beforeEach(() => sessionStorage.clear());

describe("判断与校准页", () => {
  // ACT-JRC-10：四项指标、分数分布、按后端分组，窗口切换生效。
  it("shows the four rates, histogram and per-backend rows, and the window switch re-queries", async () => {
    const be = backend();
    mount(be.impl);
    const rates = await screen.findByRole("table", { name: "指标" });
    expect(within(rates).getByText("25.0%")).toBeInTheDocument();
    expect(within(rates).getByText("10.0%")).toBeInTheDocument();
    expect(within(rates).getAllByText("50.0%").length).toBe(1);
    const rows = screen.getByRole("table", { name: "各级调用" });
    expect(within(rows).getByText("classifier")).toBeInTheDocument();
    expect(within(rows).getByText("chat")).toBeInTheDocument();
    expect(await screen.findByRole("figure")).toHaveTextContent("低端误丢率");
    expect(screen.getByLabelText("0.9–1.0")).toHaveAttribute("value", "3");
    expect(screen.getByLabelText("0.0–0.1")).toHaveAttribute("value", "10"); // 缺省 30 天

    await userEvent.selectOptions(screen.getByLabelText("窗口"), "7d");
    await waitFor(() => expect(screen.getByLabelText("0.0–0.1")).toHaveAttribute("value", "4"));
    expect(be.calls.some((c) => c.path.includes("since=7d"))).toBe(true);
    await userEvent.selectOptions(screen.getByLabelText("来源"), "src_b");
    expect(await screen.findByText("这个窗口里还没有判断记录。")).toBeInTheDocument();
  });

  // ACT-JRC-11 / ACT-BKL-07：按来源覆盖的不确定带由本人保存，读回两条。
  it("saves a global band and a per-source band as one PUT and reads both back", async () => {
    const be = backend();
    mount(be.impl);
    await screen.findByRole("table", { name: "不确定带" });
    for (const [low, high, src] of [["0.10", "0.90", ""], ["0.25", "0.50", "src_a"]]) {
      await userEvent.click(screen.getByText("添加一条"));
      const row = within(screen.getByRole("table", { name: "不确定带" })).getAllByRole("row").at(-1)!;
      await userEvent.type(within(row).getByLabelText("带模板"), "triage-screen");
      if (src) await userEvent.selectOptions(within(row).getByLabelText("带来源"), src);
      await userEvent.type(within(row).getByLabelText("下限"), low);
      await userEvent.type(within(row).getByLabelText("上限"), high);
    }
    await userEvent.click(screen.getByText("保存不确定带"));
    expect(await screen.findByText("不确定带已保存")).toBeInTheDocument();
    expect(be.settings.bands).toEqual([
      { template: "triage-screen", low: 0.1, high: 0.9 },
      { template: "triage-screen", source: "src_a", low: 0.25, high: 0.5 },
    ]);
    expect(be.calls.filter((c) => c.method === "PUT")).toHaveLength(1);
  });

  // ACT-JRC-12：服务端拒绝 low>high，页面显示原因，已有的带不变。
  it("shows the server's 400 for low > high and keeps the saved bands", async () => {
    const be = backend();
    mount(be.impl);
    await screen.findByRole("table", { name: "不确定带" });
    await userEvent.click(screen.getByText("添加一条"));
    await userEvent.type(screen.getByLabelText("带模板"), "triage-screen");
    await userEvent.type(screen.getByLabelText("下限"), "0.70");
    await userEvent.type(screen.getByLabelText("上限"), "0.40");
    await userEvent.click(screen.getByText("保存不确定带"));
    expect(await screen.findByRole("alert")).toHaveTextContent("band low must not be greater than high");
    expect(be.settings.bands).toEqual([]);
  });

  // ACT-JRC-14 的设置一半：影子开关保存到服务端。
  it("toggles shadow mode for a template and persists it", async () => {
    const be = backend();
    mount(be.impl);
    const box = await screen.findByRole("checkbox", { name: "triage-screen" });
    await userEvent.click(box);
    await waitFor(() => expect(be.settings.shadow).toEqual(["triage-screen"]));
    expect(await screen.findByRole("checkbox", { name: "triage-screen" })).toBeChecked();
    await userEvent.click(screen.getByRole("checkbox", { name: "triage-screen" }));
    await waitFor(() => expect(be.settings.shadow).toEqual([]));
  });

  // ACT-JRC-17..19：抽检池逐条处理，结论发给服务端，条目消失、误丢率刷新。
  it("lists the spot-check pool and records should_see / correct", async () => {
    const be = backend({ pool: [{ judgment: "j1", subject: "first" }, { judgment: "j2", subject: "second" }] });
    mount(be.impl);
    const list = await screen.findByRole("list", { name: "待抽检" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
    await userEvent.click(within(within(list).getAllByRole("listitem")[0]).getByText("该看"));
    await waitFor(() => expect(be.reviewed).toEqual({ j1: "should_see" }));
    await waitFor(() => expect(within(screen.getByRole("list", { name: "待抽检" })).getAllByRole("listitem")).toHaveLength(1));
    expect(await within(screen.getByRole("table", { name: "指标" })).findByText("100.0%")).toBeInTheDocument();
    await userEvent.click(screen.getByText("确实不用"));
    await waitFor(() => expect(be.reviewed).toEqual({ j1: "should_see", j2: "correct" }));
    expect(await screen.findByText("现在没有待抽检的条目。")).toBeInTheDocument();
  });
});
