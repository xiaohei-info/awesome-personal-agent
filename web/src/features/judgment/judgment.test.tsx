import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
function backend(o: { pool?: { judgment: string; subject: string }[]; bands?: Band[]; reject?: boolean } = {}) {
  let settings = { bands: [...(o.bands ?? [])] as Band[], shadow: [] as string[], spot_check: { per_brief: 5 } };
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
      if (o.reject) return err(400, "invalid_request", "band for an unknown template");
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
const TRIAGE = "来源消息值不值得看";
const card = async (name = TRIAGE) => within(await screen.findByRole("region", { name }));
const slide = (c: ReturnType<typeof within>, which: "下线" | "上线", v: number) => fireEvent.change(c.getByLabelText(which), { target: { value: String(v) } });
const puts = (be: ReturnType<typeof backend>) => be.calls.filter((c) => c.method === "PUT").map((c) => c.body);
beforeEach(() => sessionStorage.clear());

describe("自动规则 › 筛选门槛", () => {
  // 360px 不横向溢出依赖 <main> 上的手机样式（宽表格在自身内滚动）；页面根必须是 main。
  it("每个问题一张卡：中文问题、两个数、分数分布；内置问题没有数据时也在（可以先开试用）", async () => {
    mount(backend().impl);
    const c = await card();
    expect(document.querySelector("main")).toContainElement(c.getByRole("img", { name: `分数分布：${TRIAGE}` }));
    expect(c.getByText("50.0%")).toBeInTheDocument(); // 你接受的
    expect(c.getByText(/漏掉该看的 · 已抽查 0 条/)).toBeInTheDocument();
    expect(within(await screen.findByRole("region", { name: "要不要你亲自确认" })).getByText("还没有打分记录")).toBeInTheDocument();
    expect(screen.queryByText("triage-screen")).not.toBeInTheDocument();
  });

  // ACT-JRC-10：四项比例与按后端分组收在「详细数据」，窗口与来源切换会重查。
  it("详细数据里有四项比例与按谁答的分组；窗口切换重查", async () => {
    const be = backend();
    mount(be.impl);
    await card();
    const rates = screen.getByRole("table", { name: "各级调用" });
    expect(within(rates).getByText("打分模型")).toBeInTheDocument();
    expect(within(rates).getByText("写作模型")).toBeInTheDocument();
    expect(within(screen.getByRole("table", { name: "各问题的比例" })).getByText("25.0%")).toBeInTheDocument();
    expect((await card()).getByRole("status")).toHaveTextContent("最近 30 天：约丢掉 10 · 交给写作模型 0 · 直接起草 3");
    await userEvent.selectOptions(screen.getByLabelText("看最近"), "7");
    await waitFor(async () => expect((await card()).getByRole("status")).toHaveTextContent("最近 7 天：约丢掉 4"));
    expect(be.calls.some((c) => c.path.includes("since=7d"))).toBe(true);
    await userEvent.selectOptions(screen.getByLabelText("来源"), "src_b");
    expect(await (await card()).findByText("还没有打分记录")).toBeInTheDocument();
  });

  it("拖线时实时估算（按桶，标「约」）；不改就不能保存", async () => {
    mount(backend().impl);
    const c = await card();
    expect(c.getByRole("button", { name: "保存这两条线" })).toBeDisabled();
    expect(c.getByText(/下线 0.15 · 上线 0.60（默认）/)).toBeInTheDocument();
    slide(c, "下线", 0.01);
    expect(c.getByRole("status")).toHaveTextContent("约丢掉 0 · 交给写作模型 10 · 直接起草 3");
    expect(c.getByRole("button", { name: "保存这两条线" })).toBeEnabled();
  });

  // 写入语义与改版前一致：PUT 只带 bands，整份列表 = 已有的 + 这一条；与旧页「添加一条再保存」的请求体逐字段相同。
  it("ACT-JRC-11：保存一条线的请求体与改版前相同（全局与按来源各一次）", async () => {
    const existing = { template: "web-only-screening", low: 0.3, high: 1 };
    const be = backend({ bands: [existing] });
    mount(be.impl);
    let c = await card();
    slide(c, "下线", 0.1);
    slide(c, "上线", 0.9);
    await userEvent.click(c.getByRole("button", { name: "保存这两条线" }));
    await screen.findByText("已保存");
    // 旧页：添加一行 template=triage-screen、来源空、0.10/0.90 后保存，请求体是 { bands: [已有的, 新的] }。
    expect(puts(be)).toEqual([{ bands: [existing, { template: "triage-screen", low: 0.1, high: 0.9 }] }]);
    await userEvent.selectOptions(screen.getByLabelText("来源"), "src_a");
    c = await card();
    slide(c, "下线", 0.25);
    slide(c, "上线", 0.5);
    await userEvent.click(c.getByRole("button", { name: "保存这两条线" }));
    await waitFor(() => expect(puts(be)).toHaveLength(2));
    expect(puts(be)[1]).toEqual({ bands: [existing, { template: "triage-screen", low: 0.1, high: 0.9 }, { template: "triage-screen", source: "src_a", low: 0.25, high: 0.5 }] });
    expect(be.settings.bands).toHaveLength(3);
  });

  it("用默认值：去掉这一条覆盖，其余不动", async () => {
    const be = backend({ bands: [{ template: "triage-screen", low: 0.2, high: 0.7 }, { template: "web-only-screening", low: 0.3, high: 1 }] });
    mount(be.impl);
    const c = await card();
    expect(c.getByText(/下线 0.20 · 上线 0.70/)).toBeInTheDocument();
    await userEvent.click(c.getByRole("button", { name: "用默认值" }));
    await waitFor(() => expect(puts(be)).toEqual([{ bands: [{ template: "web-only-screening", low: 0.3, high: 1 }] }]));
  });

  // ACT-JRC-12：下线高于上线在页面上就拦下；服务端的拒绝照样显示，已有的带不变。
  it("下线高于上线不能保存；服务端拒绝时显示原因", async () => {
    const be = backend({ reject: true });
    mount(be.impl);
    const c = await card();
    slide(c, "下线", 0.7);
    slide(c, "上线", 0.4);
    expect(c.getByRole("alert")).toHaveTextContent("下线不能高于上线");
    expect(c.getByRole("button", { name: "保存这两条线" })).toBeDisabled();
    slide(c, "上线", 0.8);
    await userEvent.click(c.getByRole("button", { name: "保存这两条线" }));
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(be.settings.bands).toEqual([]);
  });

  // ACT-JRC-14 的设置一半：试用（影子）开关，请求体与改版前相同（整份 shadow 列表）。
  it("试用开关保存到服务端", async () => {
    const be = backend();
    mount(be.impl);
    const box = (await card()).getByRole("checkbox", { name: "试用：只打分，不影响结果" });
    await userEvent.click(box);
    await waitFor(() => expect(be.settings.shadow).toEqual(["triage-screen"]));
    expect(puts(be)).toEqual([{ shadow: ["triage-screen"] }]);
    await userEvent.click((await card()).getByRole("checkbox", { name: "试用：只打分，不影响结果" }));
    await waitFor(() => expect(be.settings.shadow).toEqual([]));
  });
});
