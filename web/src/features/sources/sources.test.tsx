import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { backlogReason } from "./shared";

// 测试边界替身：按 api/sources 的契约应答（/sources、PATCH enabled、webhook 密钥、ingests、replay、
// 来源事件按 disposition 分页、拒收记录分页、转建议；错误体见附录 B.3）。业务规则在服务端，这里只复现它的可观察行为。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix the request" });

interface Src { outbound?: { local: string; external: string | null }; id: string; type: string; command?: string; schedule?: string; cursor?: string; enabled: boolean; stats?: Record<string, unknown> }
interface Rj { run_id: string; line: number; reason: string; raw: string; at: string }
interface Ev { score?: number; id: string; type: string; subject?: string; occurred_at: string; disposition: string; item?: string; unjudged_reason?: string }

interface Models { judge?: boolean; generate?: boolean; paused?: boolean }
function backend(o: { sources?: Src[]; events?: Ev[]; rejects?: Rj[]; ingests?: any[]; webOnly?: boolean; restorable?: { source: string; id: string }[]; outboundFails?: boolean; backlog?: { count: number; outbound_unset?: number }; models?: Models; release?: { requeued: number; deferred: number } } = {}) {
  const sources = o.sources ?? [];
  const events = o.events ?? [];
  const ingests = o.ingests ?? [];
  const rejects = o.rejects ?? [];
  let keyed = false;
  const restorable = o.restorable ?? [];
  const restoreKeys: (string | undefined)[] = [];
  const calls: { method: string; path: string; body?: any; csrf: string | null }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    calls.push({ method, path: url, body, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] ?? null });
    const u = new URL(url, "http://x");
    if (u.pathname === "/v1/projects") return json(200, { projects: [] });
    if (u.pathname === "/v1/health") {
      const b = o.backlog ?? { count: 0 };
      return json(200, { healthy: true, signals: [{ id: "unjudged_backlog", ok: b.count === 0, count: b.count, next: "", detail: { outbound_unset: b.outbound_unset ?? 0 } }] });
    }
    if (u.pathname === "/v1/settings/model-endpoints") {
      const m = o.models ?? {};
      return json(200, { judge_endpoint: m.judge ? { url: "http://127.0.0.1:1/judge" } : null, generate_endpoint: m.generate ? { url: "https://llm.example.com/v1" } : null, models_paused: !!m.paused });
    }
    if (u.pathname === "/v1/source-events/rules" && method === "POST") return json(200, { attached: 1, disposed: 1, remaining: 3 });
    if (u.pathname === "/v1/source-events/score" && method === "POST") {
      if (o.models?.paused) return err(409, "models_paused", "models are paused");
      return json(200, { scored: 3, skipped: 0 });
    }
    if (u.pathname === "/v1/source-events/rejudge" && method === "POST") {
      if (o.models?.paused) return err(409, "models_paused", "models are paused");
      return json(200, o.release ?? { requeued: 3, deferred: 0 });
    }
    const pv = u.pathname.match(/^\/v1\/source-events\/([^/]+)\/outbound-preview$/);
    if (pv) {
      const e = events.find((x) => x.id === pv[1]);
      if (!e) return err(404, "not_found", "event not found");
      const tier = (material: string, link: string) => ({ event_type: e.type, subject: e.subject ?? "", sender: "someone@example.com", occurred_at: e.occurred_at, link, material });
      return json(200, { full: tier("full body " + e.id + " and the rest", "https://example.com/x/1"), excerpt: tier("full body " + e.id, "example.com"), meta: tier("", "example.com") });
    }
    if (u.pathname === "/v1/sources" && method === "GET") return json(200, { sources });
    if (u.pathname === "/v1/sources" && method === "POST") {
      if (o.webOnly) return err(403, "web_only", "custom commands can only be registered in the Web UI");
      if (body.outbound) return err(403, "web_only", "outbound is not part of the source registration");
      sources.push({ id: body.id, type: body.type, command: body.command, schedule: body.schedule, enabled: true, outbound: { local: "full", external: null } });
      return json(201, sources[sources.length - 1]);
    }
    const ob = u.pathname.match(/^\/v1\/settings\/sources\/([^/]+)\/outbound$/);
    if (ob && method === "PUT") {
      if (o.outboundFails) return err(500, "internal", "store is unavailable");
      const s = sources.find((x) => x.id === ob[1]);
      if (!s) return err(404, "not_found", "source not found");
      s.outbound = { local: body.local, external: body.external };
      return json(200, s.outbound);
    }
    if (u.pathname === "/v1/source-events/restorable" && method === "GET") {
      const src = u.searchParams.get("source");
      if (new Date(u.searchParams.get("from")!) >= new Date(u.searchParams.get("to")!)) return err(400, "invalid_request", "from must be earlier than to");
      const hit = restorable.filter((r) => !src || r.source === src);
      const by = [...new Set(hit.map((r) => r.source))].map((x) => ({ source: x, count: hit.filter((r) => r.source === x).length }));
      return json(200, { matched: hit.length, sources: by });
    }
    if (u.pathname === "/v1/source-events/restore" && method === "POST") {
      restoreKeys.push((init.headers as Record<string, string>)["Idempotency-Key"]);
      const hit = restorable.splice(0).filter((r) => !body.source || r.source === body.source);
      return json(200, { restored: hit.length });
    }
    const p = u.pathname.match(/^\/v1\/sources\/([^/]+)(\/.*)?$/);
    if (p) {
      const s = sources.find((x) => x.id === decodeURIComponent(p[1]));
      if (!s) return err(404, "not_found", "source not found");
      if (!p[2] && method === "PATCH") {
        s.enabled = body.enabled;
        return json(200, s);
      }
      if (p[2] === "/webhook" && method === "POST") {
        const rotated = keyed;
        keyed = true;
        return json(rotated ? 200 : 201, { secret: "apa_whk_SECRET123", url: "/ingest/" + s.id, rotated });
      }
      if (p[2] === "/ingests") return json(200, { ingests });
      if (p[2] === "/rejects") {
        const after = Number(u.searchParams.get("after") ?? 0);
        const page = rejects.slice(after, after + 2);
        return json(200, after + 2 < rejects.length ? { rejects: page, next_after: after + 2 } : { rejects: page });
      }
      if (p[2] === "/events") {
        const d = u.searchParams.get("disposition");
        const after = Number(u.searchParams.get("after") ?? 0);
        const all = events.filter((e) => d === null || d.split(",").includes(e.disposition));
        if (u.searchParams.get("sort") === "score") all.sort((a, b) => (b.score ?? -1) - (a.score ?? -1)); // 没有分数的排最后
        const size = d === null ? 500 : 2; // 预览按最近一页取全部事件；积压列表两条一页，好测分页
        const page = all.slice(after, after + size);
        return json(200, after + size < all.length ? { events: page, next_after: after + size } : { events: page });
      }
    }
    const rp = u.pathname.match(/^\/v1\/ingests\/([^/]+)\/replay$/);
    if (rp && method === "POST") {
      const g = ingests.find((x) => x.id === rp[1]);
      if (!g) return err(404, "not_found", "ingest not found");
      g.state = "replayed";
      return json(200, g);
    }
    const pr = u.pathname.match(/^\/v1\/source-events\/([^/]+)\/propose$/);
    if (pr && method === "POST") {
      const e = events.find((x) => x.id === pr[1]);
      if (!e) return err(404, "not_found", "event not found");
      if (e.disposition !== "unjudged") return err(409, "conflict", `event is ${e.disposition}, not unjudged`);
      e.disposition = "proposed";
      e.item = "item_" + e.id;
      return json(200, e);
    }
    const ig = u.pathname.match(/^\/v1\/source-events\/([^/]+)\/ignore$/);
    if (ig && method === "POST") {
      const e = events.find((x) => x.id === ig[1]);
      if (!e) return err(404, "not_found", "event not found");
      if (e.disposition !== "unjudged" && e.disposition !== "expired") return err(409, "conflict", `event is ${e.disposition}, not unjudged`);
      e.disposition = "ignored";
      return json(200, e);
    }
    if (u.pathname === "/v1/source-events/ignore" && method === "POST") {
      const backlog = events.filter((e) => e.disposition === "unjudged" || e.disposition === "expired");
      backlog.forEach((e) => (e.disposition = "ignored")); // 替身只有一个来源，按来源与全部等价
      return json(200, { ignored: backlog.length });
    }
    return err(404, "not_found", "no such route");
  }) as unknown as typeof fetch;
  return { impl, calls, events, sources, ingests, restoreKeys };
}

function mount(fetchImpl: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/sources"]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const hook: Src = { id: "src_hook", type: "webhook", enabled: true };
const rss: Src = { id: "src_rss", type: "pull", schedule: "15m", cursor: "cur-42", enabled: true, stats: { consecutive_failures: 3, timeouts: 1, last_success_at: "2030-01-01T00:00:00Z", last_error: "boom" } };
const ev = (id: string, disposition = "unjudged"): Ev => ({ id, type: "issue.opened", subject: "subject-" + id, occurred_at: "2030-01-01T00:00:00Z", disposition });
const row = async (id: string) => within(await screen.findByRole("listitem", { name: id }));

beforeEach(() => {
  sessionStorage.clear();
  localStorage.removeItem("apa.backlog");
});
const details = async (id: string) => userEvent.click((await row(id)).getByRole("button", { name: "详情" }));
const openAdvanced = async (within_: ReturnType<typeof within> | typeof screen = screen) => {
  for (const s of within_.getAllByText("高级")) if (!(s.parentElement as HTMLDetailsElement).open) await userEvent.click(s);
};

describe("来源列表：类型、周期、状态与读取统计", () => {
  it("行上是类型、周期和一句状态；游标、最近成功、连续失败、超时和最近错误在详情的高级里", async () => {
    const b = backend({ sources: [rss] });
    mount(b.impl);
    const r = await row("src_rss");
    expect(r.getByText("定时拉取")).toBeInTheDocument();
    expect(r.getByText("每 15 分钟")).toBeInTheDocument();
    expect(r.getByText("连续 3 次没读到")).toBeInTheDocument();
    expect(r.getByText("读取失败")).toBeInTheDocument();
    expect(r.queryByText(/cur-42/)).not.toBeInTheDocument();
    await details("src_rss");
    const d = within(await screen.findByRole("region", { name: "来源 src_rss" }));
    await openAdvanced(d);
    expect(d.getByText(/读到 cur-42/)).toBeInTheDocument();
    expect(d.getByText(/连续失败 3 · 超时 1/)).toBeInTheDocument();
    expect(d.getByText(new RegExp("最近成功 " + new Date("2030-01-01T00:00:00Z").toLocaleString("zh-CN").replace(/[/:]/g, "."))))
      .toBeInTheDocument();
    expect(d.getByText(/最近错误 boom/)).toBeInTheDocument();
  });

  it("没有来源时显示空状态和下一步按钮", async () => {
    mount(backend().impl);
    expect(await screen.findByText("还没有来源")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "接入第一个来源" })).toBeInTheDocument();
  });

  it("启停：PATCH enabled 带 CSRF，界面以服务端回读为准", async () => {
    const b = backend({ sources: [{ ...rss }] });
    mount(b.impl);
    await userEvent.click(within((await row("src_rss")).getByRole("group", { name: "操作 src_rss" })).getByRole("button", { name: "停用" }));
    const patch = b.calls.find((c) => c.method === "PATCH")!;
    expect(patch.path).toBe("/v1/sources/src_rss");
    expect(patch.body).toEqual({ enabled: false });
    expect(patch.csrf).toBe("tok-1");
    expect(await (await row("src_rss")).findByText("已停用")).toBeInTheDocument();
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "启用" }));
    await waitFor(async () => expect((await row("src_rss")).queryByText("已停用")).not.toBeInTheDocument());
  });
});

describe("读取失败", () => {
  it("来源列表读取失败：显示服务端原因，不显示空状态", async () => {
    mount((async (url: string) => (url === "/v1/projects" ? json(200, { projects: [] }) : err(500, "internal", "store is unavailable"))) as unknown as typeof fetch);
    expect((await screen.findAllByRole("alert"))[0]).toHaveTextContent("store is unavailable");
    expect(screen.queryByText("还没有来源")).not.toBeInTheDocument();
  });
});

describe("接收密钥与入站记录重放", () => {
  it("创建后只显示一次明文，带地址与复制；再次点击是更换并提示旧密钥短期可用；换来源后明文不残留", async () => {
    const b = backend({ sources: [hook, rss] });
    mount(b.impl);
    await details("src_hook");
    await userEvent.click(await screen.findByRole("button", { name: "创建或更换密钥" }));
    expect(await screen.findByTestId("webhook-secret")).toHaveTextContent("apa_whk_SECRET123");
    expect(screen.getByText("/ingest/src_hook")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("已创建 · 只显示这一次");
    expect(screen.getAllByRole("button", { name: "复制" })).toHaveLength(2);
    expect(b.calls.find((c) => c.path === "/v1/sources/src_hook/webhook")!.csrf).toBe("tok-1");
    await userEvent.click(screen.getByRole("button", { name: "创建或更换密钥" }));
    expect(await screen.findByText(/已更换，旧密钥短期内仍能用/)).toBeInTheDocument();
    await details("src_rss");
    expect(screen.queryByTestId("webhook-secret")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "创建或更换密钥" })).not.toBeInTheDocument();
  });

  it("入站记录在高级里：列出校验结果并可看原始内容；重放后状态以服务端为准", async () => {
    const b = backend({ sources: [hook], ingests: [{ id: "ing_1", received_at: "2030-01-01T00:00:00Z", state: "rejected", signed: false, events: 0, reason: "bad signature", body: "{\"a\":1}" }] });
    mount(b.impl);
    await details("src_hook");
    await openAdvanced(within(await screen.findByRole("region", { name: "来源 src_hook" })));
    const region = within(await screen.findByRole("region", { name: "入站记录" }));
    expect(await region.findByText(/bad signature/)).toBeInTheDocument();
    expect(region.getByText(/已拒收 · 未验签 · 产出 0 条/)).toBeInTheDocument();
    await userEvent.click(region.getByRole("button", { name: "查看" }));
    expect(screen.getByTestId("ingest-body-ing_1")).toHaveTextContent('{"a":1}');
    await userEvent.click(region.getByRole("button", { name: "重放" }));
    expect(await region.findByText("replayed")).toBeInTheDocument(); // 词表外的状态原样显示
    expect(b.calls.find((c) => c.path === "/v1/ingests/ing_1/replay")!.csrf).toBe("tok-1");
    expect(region.getByRole("status")).toHaveTextContent("已重放");
  });
});

describe("积压的消息（flow-triage-failure / ACT-TRI-02）", () => {
  it("只列未判定的；转为建议后该事件离开列表且服务端处置为 proposed", async () => {
    const b = backend({ sources: [rss], events: [ev("e1"), ev("e2", "ignored"), ev("e3")] });
    mount(b.impl);
    await details("src_rss");
    const region = within(await screen.findByRole("region", { name: "积压的消息" }));
    expect(await region.findByText(/subject-e1/)).toBeInTheDocument();
    expect(region.getByText(/subject-e3/)).toBeInTheDocument();
    expect(region.queryByText(/subject-e2/)).not.toBeInTheDocument();
    await userEvent.click(region.getAllByRole("button", { name: "转为建议" })[0]);
    await region.findByText(/subject-e3/);
    expect(region.queryByText(/subject-e1/)).not.toBeInTheDocument();
    expect(b.events.find((e) => e.id === "e1")).toMatchObject({ disposition: "proposed", item: "item_e1" });
    expect(b.calls.find((c) => c.method === "POST" && c.path === "/v1/source-events/e1/propose")!.csrf).toBe("tok-1");
  });

  it("分页用 next_after 加载更多", async () => {
    const b = backend({ sources: [rss, hook], events: [ev("e1"), ev("e2"), ev("e3")] });
    mount(b.impl);
    await details("src_rss");
    const region = within(await screen.findByRole("region", { name: "积压的消息" }));
    await region.findByText(/subject-e1/);
    expect(region.queryByText(/subject-e3/)).not.toBeInTheDocument();
    await userEvent.click(region.getByRole("button", { name: "加载更多" }));
    expect(await region.findByText(/subject-e3/)).toBeInTheDocument();
    expect(b.calls.some((c) => c.path.includes("after=2"))).toBe(true);
    expect(region.queryByRole("button", { name: "加载更多" })).not.toBeInTheDocument();
  });

  it("转换被拒绝（事件已不是未判定）：显示服务端原因，不假装成功", async () => {
    const b = backend({ sources: [rss], events: [ev("e1")] });
    mount(b.impl);
    await details("src_rss");
    const region = within(await screen.findByRole("region", { name: "积压的消息" }));
    await region.findByText(/subject-e1/);
    b.events[0].disposition = "ignored"; // 别处已处置
    await userEvent.click(region.getByRole("button", { name: "转为建议" }));
    expect(await region.findByRole("alert")).toHaveTextContent("event is ignored, not unjudged");
  });

  it("打过分时才出现按预估排序：请求带 sort=score，按服务端顺序显示预估值得看的百分比", async () => {
    const b = backend({ sources: [rss], events: [ev("e1"), { ...ev("e2"), score: 0.05 }, { ...ev("e3"), score: 0.9 }] });
    mount(b.impl);
    await details("src_rss");
    const region = within(await screen.findByRole("region", { name: "积压的消息" }));
    await region.findByText(/subject-e1/);
    expect(b.calls.some((c) => c.path.includes("sort=score"))).toBe(false);
    await userEvent.click(region.getByRole("checkbox", { name: "按预估值得看排序" }));
    await region.findByText("预估值得看 90%");
    expect(b.calls.some((c) => c.path.includes("sort=score"))).toBe(true);
    expect(region.getAllByText(/^subject-/).map((x) => x.textContent)).toEqual(["subject-e3", "subject-e2"]);
    expect(region.getByText("预估值得看 5%")).toBeInTheDocument();
  });

  it("没打过分时没有分数，也没有按预估排序", async () => {
    mount(backend({ sources: [rss], events: [ev("e1")] }).impl);
    await details("src_rss");
    const region = within(await screen.findByRole("region", { name: "积压的消息" }));
    await region.findByText(/subject-e1/);
    expect(region.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(region.queryByText(/预估值得看/)).not.toBeInTheDocument();
  });

  it("可逐条忽略，也可两步确认后忽略本来源全部（含已过期的）", async () => {
    const b = backend({ sources: [rss], events: [ev("e1"), ev("e2"), ev("e3", "expired"), ev("e4")] });
    mount(b.impl);
    await details("src_rss");
    const region = within(await screen.findByRole("region", { name: "积压的消息" }));
    await region.findByText(/subject-e1/);
    expect(region.queryByText(/unjudged|expired/)).not.toBeInTheDocument();
    await userEvent.click(within(region.getByRole("group", { name: "处理 e1" })).getByRole("button", { name: "忽略" }));
    await region.findByText("已过期");
    expect(b.events.find((e) => e.id === "e1")!.disposition).toBe("ignored");
    await userEvent.click(region.getByRole("button", { name: "忽略这个来源的全部积压" }));
    expect(b.calls.some((c) => c.path === "/v1/source-events/ignore")).toBe(false); // 第一次点击只展开确认
    await userEvent.click(region.getByRole("button", { name: "确认忽略这个来源的全部积压" }));
    expect(await region.findByText("没有积压的消息")).toBeInTheDocument();
    expect(b.calls.find((c) => c.path === "/v1/source-events/ignore")!.body).toEqual({ source: "src_rss" });
    expect(b.events.every((e) => e.disposition === "ignored")).toBe(true);
  });

  it("空状态", async () => {
    mount(backend({ sources: [rss] }).impl);
    await details("src_rss");
    expect(await screen.findByText("没有积压的消息")).toBeInTheDocument();
  });
});

describe("积压的状态与未判定原因一一对应", () => {
  it("backlogReason：空原因与 v0.1 的 undecided 按等模型恢复（内核同样对待）；其余原因各自保留；已过期优先", () => {
    const r = (unjudged_reason?: string, disposition = "unjudged") => backlogReason({ disposition, unjudged_reason });
    expect(r()).toBe("model_unavailable");
    expect(r("undecided")).toBe("model_unavailable");
    expect(r("model_unavailable")).toBe("model_unavailable");
    expect(r("models_paused")).toBe("models_paused");
    expect(r("outbound_unset")).toBe("outbound_unset");
    expect(r("restored")).toBe("restored");
    expect(r("outbound_unset", "expired")).toBe("expired");
  });

  it("每种原因显示自己的状态，不再把等外发范围、已放回标成等模型恢复；等外发范围的可点去选", async () => {
    const reasons = ["model_unavailable", "models_paused", "outbound_unset", "restored"];
    const b = backend({ sources: [{ ...rss, outbound: { local: "full", external: "meta" } }], events: [...reasons.map((x, i): Ev => ({ ...ev("r" + i), unjudged_reason: x })), ev("x", "expired")] });
    mount(b.impl);
    await details("src_rss");
    const region = within(await screen.findByRole("region", { name: "积压的消息" }));
    await region.findByText(/subject-r0/);
    while (region.queryByRole("button", { name: "加载更多" })) {
      await userEvent.click(region.getByRole("button", { name: "加载更多" }));
      await new Promise((r) => setTimeout(r, 0));
    }
    await region.findByText(/subject-x/);
    expect(region.getByText("等模型恢复")).toBeInTheDocument();
    expect(region.getByText("模型已暂停")).toBeInTheDocument();
    expect(region.getByText("已放回，等你放开")).toBeInTheDocument();
    expect(region.getByText("已过期")).toBeInTheDocument();
    const r = await row("src_rss");
    expect(r.queryByRole("radiogroup", { name: "发给外部模型时" })).not.toBeInTheDocument();
    await userEvent.click(region.getByRole("button", { name: "等你选外发范围" }));
    expect(await r.findByRole("radiogroup", { name: "发给外部模型时" })).toBeInTheDocument();
  });
});

describe("读取失败的记录（ACT-SRC-04）", () => {
  const rj = (n: number): Rj => ({ run_id: "run_1", line: n, reason: "reason-" + n, raw: "{bad-" + n, at: "2030-01-01T00:00:00Z" });

  it("在高级里列出原因与原始行；分页用 next_after 加载更多", async () => {
    const b = backend({ sources: [rss], rejects: [rj(1), rj(2), rj(3)] });
    mount(b.impl);
    await details("src_rss");
    await openAdvanced(within(await screen.findByRole("region", { name: "来源 src_rss" })));
    const region = within(await screen.findByRole("region", { name: "读取失败的记录" }));
    expect(await region.findByText(/reason-1/)).toBeInTheDocument();
    expect(region.getByText("{bad-1")).toBeInTheDocument();
    expect(region.getByText(/第 2 行 · reason-2/)).toBeInTheDocument();
    expect(region.queryByText(/reason-3/)).not.toBeInTheDocument();
    await userEvent.click(region.getByRole("button", { name: "加载更多" }));
    expect(await region.findByText(/reason-3/)).toBeInTheDocument();
    expect(b.calls.some((c) => c.path === "/v1/sources/src_rss/rejects?after=2")).toBe(true);
    expect(region.queryByRole("button", { name: "加载更多" })).not.toBeInTheDocument();
  });

  it("没有记录时显示空状态", async () => {
    mount(backend({ sources: [rss] }).impl);
    await details("src_rss");
    expect(await screen.findByText("没有读取失败的记录")).toBeInTheDocument();
  });
});

describe("自定义命令来源（flow-command-registration / ACT-CMD-04）", () => {
  async function fill(id: string, cmd: string) {
    await screen.findByText("高级");
    await openAdvanced();
    const f = await screen.findByRole("region", { name: "自定义命令来源" });
    await userEvent.type(within(f).getByLabelText("来源名"), id);
    await userEvent.type(within(f).getByLabelText("命令"), cmd);
    return f;
  }

  it("在 Web 登记 script 来源：POST 带 type=script、命令、由「每 n 分钟」得到的周期与 CSRF，成功后出现在列表", async () => {
    const b = backend();
    mount(b.impl);
    const f = await fill("src_check", "/opt/check.sh");
    await userEvent.type(within(f).getByLabelText("超时（秒）"), "30");
    await userEvent.selectOptions(within(f).getByLabelText("发给外部模型时"), "excerpt");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    const post = b.calls.find((c) => c.method === "POST" && c.path === "/v1/sources")!;
    expect(post.body).toEqual({ id: "src_check", type: "script", command: "/opt/check.sh", schedule: "15m", timeout: 30 });
    expect(post.csrf).toBe("tok-1");
    await row("src_check");
    expect(within(f).getByLabelText("来源名")).toHaveValue("");
  });

  it("周期按小时：每 2 小时得到 2h；间隔不是正整数时不能提交", async () => {
    const b = backend();
    mount(b.impl);
    const f = await fill("src_h", "/opt/h.sh");
    await userEvent.clear(within(f).getByLabelText("间隔"));
    expect(within(f).getByRole("button", { name: "登记" })).toBeDisabled();
    await userEvent.type(within(f).getByLabelText("间隔"), "2");
    await userEvent.selectOptions(within(f).getByLabelText("单位"), "h");
    await userEvent.selectOptions(within(f).getByLabelText("发给外部模型时"), "none");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    await row("src_h");
    expect(b.calls.find((c) => c.method === "POST" && c.path === "/v1/sources")!.body.schedule).toBe("2h");
  });

  it("服务端拒绝时显示原因并保留输入，列表不出现该来源", async () => {
    const b = backend({ webOnly: true });
    mount(b.impl);
    const f = await fill("src_x", "/tmp/evil.sh");
    await userEvent.selectOptions(within(f).getByLabelText("发给外部模型时"), "none");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    expect(await within(f).findByRole("alert")).toHaveTextContent("only be registered in the Web UI");
    expect(within(f).getByLabelText("命令")).toHaveValue("/tmp/evil.sh");
    expect(screen.queryByRole("listitem", { name: "src_x" })).not.toBeInTheDocument();
  });

  it("缺必填项时不能提交", async () => {
    mount(backend().impl);
    await screen.findByText("高级");
    await openAdvanced();
    const f = await screen.findByRole("region", { name: "自定义命令来源" });
    expect(within(f).getByRole("button", { name: "登记" })).toBeDisabled();
  });
});

describe("外发范围（flow-outbound-scope / ACT-OBD-02、ACT-OBD-15）", () => {
  it("登记必须选外部范围：不选被表单拒绝且不发请求；本机预填全部内容；类型可选「其他」自填；选后先登记再写范围", async () => {
    const b = backend();
    mount(b.impl);
    const f = await fill2();
    async function fill2() {
      await screen.findByText("高级");
      await openAdvanced();
      const f = await screen.findByRole("region", { name: "自定义命令来源" });
      await userEvent.type(within(f).getByLabelText("来源名"), "src_obd");
      await userEvent.selectOptions(within(f).getByLabelText("这是什么"), "其他…");
      await userEvent.type(within(f).getByLabelText("类型名"), "acc-obd");
      await userEvent.type(within(f).getByLabelText("命令"), "/opt/obd.sh");
      return f;
    }
    expect(within(f).getByLabelText("发给本机模型时")).toHaveValue("full");
    expect(within(f).getByLabelText("发给外部模型时")).toHaveValue(""); // 外部范围没有缺省值
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    expect(await within(f).findByRole("alert")).toHaveTextContent("请选好「发给外部模型时」");
    expect(b.calls.some((c) => c.method === "POST" && c.path === "/v1/sources")).toBe(false);

    await userEvent.selectOptions(within(f).getByLabelText("发给外部模型时"), "full");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    const r = await row("src_obd");
    expect(b.calls.find((c) => c.method === "POST" && c.path === "/v1/sources")!.body).toMatchObject({ source_type: "acc-obd" });
    const put = b.calls.find((c) => c.method === "PUT")!;
    expect(put.path).toBe("/v1/settings/sources/src_obd/outbound");
    expect(put.body).toEqual({ local: "full", external: "full" });
    expect(put.csrf).toBe("tok-1");
    await waitFor(() => expect(r.getByRole("group", { name: "src_obd 的外发范围" })).toHaveTextContent("发给本机模型时 全部内容 · 发给外部模型时 全部内容"));
  });

  it("显示服务端的两个范围；外部未选的直接展开且标「还没选 · n 条在等」；已选的点修改才展开", async () => {
    const b = backend({ sources: [{ ...rss, outbound: { local: "full", external: null } }, { ...hook, outbound: { local: "meta", external: "excerpt" } }], backlog: { count: 7, outbound_unset: 4 } });
    mount(b.impl);
    const a = await row("src_rss");
    const ext = within(a.getByRole("radiogroup", { name: "发给外部模型时" }));
    expect(ext.getAllByRole("radio").every((x) => !(x as HTMLInputElement).checked)).toBe(true);
    expect(within(a.getByRole("radiogroup", { name: "发给本机模型时" })).getByRole("radio", { name: "全部内容" })).toBeChecked();
    expect(await a.findByText("还没选 · 4 条在等")).toBeInTheDocument();
    const h = await row("src_hook");
    expect(h.getByRole("group", { name: "src_hook 的外发范围" })).toHaveTextContent("发给本机模型时 只有标题 · 发给外部模型时 摘要和开头");
    expect(h.queryByRole("radiogroup")).not.toBeInTheDocument();
  });

  it("改了先预览一条真实消息会发出什么，点「就这样发」才写回，以服务端回读为准", async () => {
    const b = backend({ sources: [{ ...hook, outbound: { local: "meta", external: "excerpt" } }], events: [ev("e1", "ignored"), ev("e2", "proposed")] });
    mount(b.impl);
    const h = await row("src_hook");
    await userEvent.click(h.getByRole("button", { name: "修改" }));
    const ext = within(h.getByRole("radiogroup", { name: "发给外部模型时" }));
    await userEvent.click(ext.getByRole("radio", { name: "摘要和开头" }));
    expect(h.getByRole("button", { name: "就这样发" })).toBeDisabled(); // 与已存的相同
    await userEvent.click(ext.getByRole("radio", { name: "只有标题" }));
    const pv = within(await h.findByLabelText("用一条真实消息预览"));
    expect(pv.getByText(/subject-e2/)).toBeInTheDocument(); // 最近一条
    expect(pv.queryByText(/full body/)).not.toBeInTheDocument();
    expect(pv.getByText("正文、完整链接")).toBeInTheDocument(); // 划线标出不发送的部分
    expect(b.calls.some((c) => c.path === "/v1/source-events/e2/outbound-preview")).toBe(true);
    await userEvent.click(ext.getByRole("radio", { name: "摘要和开头" }));
    await userEvent.click(ext.getByRole("radio", { name: "全部内容" }));
    expect(within(await h.findByLabelText("用一条真实消息预览")).getByText("full body e2 and the rest")).toBeInTheDocument();
    expect(b.calls.filter((c) => c.path.endsWith("/outbound-preview"))).toHaveLength(1); // 预览只取一次，各档在本地切换
    expect(b.calls.some((c) => c.method === "PUT")).toBe(false); // 选了还没写
    await userEvent.click(h.getByRole("button", { name: "就这样发" }));
    await waitFor(() => expect(b.sources[0].outbound).toEqual({ local: "meta", external: "full" }));
    expect(b.calls.filter((c) => c.method === "PUT")).toHaveLength(1);
    await waitFor(async () => expect((await row("src_hook")).getByRole("group", { name: "src_hook 的外发范围" })).toHaveTextContent("发给外部模型时 全部内容"));
  });

  it("选「不发送」时预览说明不会发出去；还没有消息时说没有可预览的", async () => {
    const b = backend({ sources: [{ ...rss, outbound: { local: "full", external: null } }] });
    const first = mount(b.impl);
    const a = await row("src_rss");
    await userEvent.click(within(a.getByRole("radiogroup", { name: "发给外部模型时" })).getByRole("radio", { name: "不发送" }));
    expect(await a.findByText("还没有消息可预览")).toBeInTheDocument();
    first.unmount();
    b.events.push(ev("e9", "ignored"));
    mount(b.impl);
    const a2 = await row("src_rss");
    await userEvent.click(within(a2.getByRole("radiogroup", { name: "发给外部模型时" })).getByRole("radio", { name: "不发送" }));
    expect(await a2.findByText("这个来源的消息不会发出去")).toBeInTheDocument();
  });

  it("来源登记成功但范围写入失败：显示原因并提示重选，来源仍列出", async () => {
    const b = backend({ outboundFails: true });
    mount(b.impl);
    await screen.findByText("高级");
    await openAdvanced();
    const f = await screen.findByRole("region", { name: "自定义命令来源" });
    await userEvent.type(within(f).getByLabelText("来源名"), "src_p");
    await userEvent.type(within(f).getByLabelText("命令"), "/opt/p.sh");
    await userEvent.selectOptions(within(f).getByLabelText("发给外部模型时"), "none");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    expect(await within(f).findByRole("alert")).toHaveTextContent(/来源已登记，外发范围没存上.*store is unavailable/);
    expect(await row("src_p")).toBeTruthy();
  });
});

describe("处理积压四步（§9.3、设计 §6.5）", () => {
  const flow = async () => within(await screen.findByRole("region", { name: /积压的 \d+ 条/ }));
  const posts = (b: ReturnType<typeof backend>) => b.calls.filter((c) => c.method === "POST" && /source-events\/(rules|score|rejudge)$/.test(c.path)).map((c) => c.path.split("/").pop());

  it("没有积压时不出现", async () => {
    mount(backend({ sources: [rss], models: { judge: true } }).impl);
    await row("src_rss");
    expect(screen.queryByRole("region", { name: /积压的/ })).not.toBeInTheDocument();
  });

  it("按顺序走完：规则 → 试打分 → 定线 → 放进建议流；当天放不完时说余下明天", async () => {
    const b = backend({ sources: [rss], backlog: { count: 5 }, models: { judge: true, generate: true }, release: { requeued: 2, deferred: 1 } });
    mount(b.impl);
    const f = await flow();
    expect(f.getByText("剩 5 条")).toBeInTheDocument();
    expect(f.getByText("第 1 步：先按规则处理")).toHaveAttribute("aria-current", "step");
    await userEvent.click(f.getByRole("button", { name: "开始" }));
    expect(await f.findByText("剩 3 条")).toBeInTheDocument();
    expect(f.getByText("规则处理了 2 条，没用模型")).toBeInTheDocument();
    await userEvent.click(f.getByRole("button", { name: "开始试打分" }));
    expect(await f.findByText("打了 3 条分")).toBeInTheDocument();
    expect(f.getByRole("link", { name: "去定这条线" })).toHaveAttribute("href", "/judgment");
    await userEvent.click(f.getByRole("button", { name: "定好了" }));
    await userEvent.click(f.getByRole("button", { name: "放进建议流" }));
    expect(await f.findByText("今天先放 2 条，余下 1 条明天")).toBeInTheDocument();
    expect(posts(b)).toEqual(["rules", "score", "rejudge"]);
    expect(b.calls.find((c) => c.path === "/v1/source-events/rules")!.csrf).toBe("tok-1");
  });

  it("刷新后从上一步的结果接着走", async () => {
    const b = backend({ sources: [rss], backlog: { count: 5 }, models: { judge: true } });
    const first = mount(b.impl);
    await userEvent.click((await flow()).getByRole("button", { name: "开始" }));
    await (await flow()).findByText("剩 3 条");
    first.unmount();
    mount(b.impl);
    const f = await flow();
    expect(f.getByText("剩 3 条")).toBeInTheDocument();
    expect(f.getByText("第 2 步：试打分")).toHaveAttribute("aria-current", "step");
    expect(f.getByRole("button", { name: "开始试打分" })).toBeEnabled();
    expect(posts(b)).toEqual(["rules"]);
  });

  it("没有打分模型：第 2 步可跳过，直接到放进建议流，不调打分", async () => {
    const b = backend({ sources: [rss], backlog: { count: 5 }, models: { generate: true } });
    mount(b.impl);
    const f = await flow();
    await userEvent.click(f.getByRole("button", { name: "开始" }));
    expect(await f.findByText("没有打分模型")).toBeInTheDocument();
    await userEvent.click(f.getByRole("button", { name: "跳过" }));
    expect(f.getByText("第 4 步：放进建议流")).toHaveAttribute("aria-current", "step");
    await userEvent.click(f.getByRole("button", { name: "放进建议流" }));
    expect(await f.findByText("放进了 3 条")).toBeInTheDocument();
    expect(posts(b)).toEqual(["rules", "rejudge"]);
  });

  it("模型已暂停：第 1 步照常，第 2、4 步置灰并给去恢复的入口", async () => {
    const b = backend({ sources: [rss], backlog: { count: 5 }, models: { judge: true, paused: true } });
    const first = mount(b.impl);
    const f = await flow();
    await userEvent.click(f.getByRole("button", { name: "开始" }));
    expect(await f.findByRole("button", { name: "开始试打分" })).toBeDisabled();
    expect(f.getByRole("link", { name: "去恢复" })).toHaveAttribute("href", "/settings/models");
    first.unmount();
    localStorage.setItem("apa.backlog", JSON.stringify({ step: 3, rules: { done: 2, remaining: 3 } }));
    mount(b.impl);
    const f2 = await flow();
    expect(f2.getByRole("button", { name: "放进建议流" })).toBeDisabled();
    expect(f2.getByRole("link", { name: "去恢复" })).toBeInTheDocument();
    expect(posts(b)).toEqual(["rules"]);
  });

  it("全部忽略带条数，两步确认", async () => {
    const b = backend({ sources: [rss], backlog: { count: 5 }, models: { judge: true } });
    mount(b.impl);
    const f = await flow();
    await userEvent.click(f.getByRole("button", { name: "全部忽略（5）" }));
    expect(b.calls.some((c) => c.path === "/v1/source-events/ignore")).toBe(false);
    await userEvent.click(f.getByRole("button", { name: "确认全部忽略（5）" }));
    await waitFor(() => expect(b.calls.find((c) => c.path === "/v1/source-events/ignore")!.body).toEqual({ all: true }));
  });
});

describe("找回升级前被自动忽略的消息（flow-backlog-restore / ACT-RST-07）", () => {
  const fill = async (src?: string) => {
    await screen.findByText("高级");
    await openAdvanced();
    const r = within(await screen.findByRole("region", { name: "找回升级前被自动忽略的消息" }));
    await userEvent.type(r.getByLabelText("从"), "2030-01-01T00:00");
    await userEvent.type(r.getByLabelText("到"), "2030-01-02T00:00");
    if (src) await userEvent.selectOptions(r.getByLabelText("来源"), src);
    return r;
  };

  it("先看条数（按来源分布，不写入）再执行；执行条数与预览一致，带 Idempotency-Key 与 CSRF", async () => {
    const b = backend({ sources: [rss, hook], restorable: [{ source: "src_rss", id: "a" }, { source: "src_rss", id: "b" }, { source: "src_hook", id: "c" }] });
    mount(b.impl);
    await row("src_rss");
    const r = await fill();
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled(); // 没看条数不能执行
    await userEvent.click(r.getByRole("button", { name: "先看有几条" }));
    const pv = within(await r.findByRole("status", { name: "可找回" }));
    expect(pv.getByText("找到 3 条")).toBeInTheDocument();
    expect(pv.getByText("src_rss · 2 条")).toBeInTheDocument();
    expect(pv.getByText("src_hook · 1 条")).toBeInTheDocument();
    const q = new URL(b.calls.find((c) => c.path.startsWith("/v1/source-events/restorable"))!.path, "http://x").searchParams;
    expect(q.get("from")).toBe(new Date("2030-01-01T00:00").toISOString());
    expect(q.get("source")).toBeNull();
    expect(b.calls.some((c) => c.path === "/v1/source-events/restore")).toBe(false);

    await userEvent.click(r.getByRole("button", { name: "放回积压" }));
    expect(await r.findByText("已放回 3 条")).toBeInTheDocument();
    const post = b.calls.find((c) => c.path === "/v1/source-events/restore")!;
    expect(post.body).toEqual({ from: new Date("2030-01-01T00:00").toISOString(), to: new Date("2030-01-02T00:00").toISOString() });
    expect(post.csrf).toBe("tok-1");
    expect(b.restoreKeys[0]).toBeTruthy();
    expect(r.queryByRole("status", { name: "可找回" })).not.toBeInTheDocument();
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled();
  });

  it("可限定来源；改条件后条数作废，必须重看", async () => {
    const b = backend({ sources: [rss, hook], restorable: [{ source: "src_rss", id: "a" }, { source: "src_hook", id: "c" }] });
    mount(b.impl);
    await row("src_rss");
    const r = await fill("src_hook");
    await userEvent.click(r.getByRole("button", { name: "先看有几条" }));
    await r.findByText("找到 1 条");
    await userEvent.selectOptions(r.getByLabelText("来源"), "src_rss");
    expect(r.queryByText("找到 1 条")).not.toBeInTheDocument();
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled();
    await userEvent.click(r.getByRole("button", { name: "先看有几条" }));
    await r.findByText("找到 1 条");
    await userEvent.click(r.getByRole("button", { name: "放回积压" }));
    await r.findByText("已放回 1 条");
    expect(b.calls.find((c) => c.path === "/v1/source-events/restore")!.body.source).toBe("src_rss");
  });

  it("没有命中时不能执行；被服务端拒绝时显示原因", async () => {
    const b = backend({ sources: [rss] });
    mount(b.impl);
    await row("src_rss");
    const r = await fill();
    await userEvent.click(r.getByRole("button", { name: "先看有几条" }));
    await r.findByText("找到 0 条");
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled();
    await userEvent.clear(r.getByLabelText("到"));
    await userEvent.type(r.getByLabelText("到"), "2029-12-31T00:00");
    await userEvent.click(r.getByRole("button", { name: "先看有几条" }));
    expect(await r.findByRole("alert")).toHaveTextContent("from must be earlier than to");
  });
});
