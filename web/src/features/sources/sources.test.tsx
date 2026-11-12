import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/sources 的契约应答（/sources、PATCH enabled、webhook 密钥、ingests、replay、
// 来源事件按 disposition 分页、拒收记录分页、转建议；错误体见附录 B.3）。业务规则在服务端，这里只复现它的可观察行为。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "fix the request" });

interface Src { outbound?: { local: string; external: string | null }; id: string; type: string; command?: string; schedule?: string; cursor?: string; enabled: boolean; stats?: Record<string, unknown> }
interface Rj { run_id: string; line: number; reason: string; raw: string; at: string }
interface Ev { score?: number; id: string; type: string; subject?: string; occurred_at: string; disposition: string; item?: string; unjudged_reason?: string }

function backend(o: { sources?: Src[]; events?: Ev[]; rejects?: Rj[]; ingests?: any[]; webOnly?: boolean; restorable?: { source: string; id: string }[]; outboundFails?: boolean } = {}) {
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
        const all = events.filter((e) => (d ?? "").split(",").includes(e.disposition));
        if (u.searchParams.get("sort") === "score") all.sort((a, b) => (b.score ?? -1) - (a.score ?? -1)); // 没有分数的排最后
        const page = all.slice(after, after + 2);
        return json(200, after + 2 < all.length ? { events: page, next_after: after + 2 } : { events: page });
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

beforeEach(() => sessionStorage.clear());

describe("来源登记、游标与失败统计", () => {
  it("显示类型、周期、游标、最近成功、连续失败、超时和最近错误；空时给出空状态", async () => {
    const b = backend({ sources: [rss] });
    mount(b.impl);
    const r = await row("src_rss");
    expect(r.getByText(/游标 cur-42/)).toBeInTheDocument();
    expect(r.getByText(/最近成功 2030-01-01T00:00:00Z/)).toBeInTheDocument();
    expect(r.getByText(/连续失败 3/)).toBeInTheDocument();
    expect(r.getByText(/超时 1/)).toBeInTheDocument();
    expect(r.getByText(/最近错误 boom/)).toBeInTheDocument();
  });

  it("没有来源时显示空状态", async () => {
    mount(backend().impl);
    expect(await screen.findByText("还没有来源")).toBeInTheDocument();
  });

  it("启停：PATCH enabled 带 CSRF，界面以服务端回读为准", async () => {
    const b = backend({ sources: [{ ...rss }] });
    mount(b.impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "停用" }));
    const patch = b.calls.find((c) => c.method === "PATCH")!;
    expect(patch.path).toBe("/v1/sources/src_rss");
    expect(patch.body).toEqual({ enabled: false });
    expect(patch.csrf).toBe("tok-1");
    expect(await screen.findByText(/已停用/)).toBeInTheDocument();
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "启用" }));
    expect(await screen.findByText(/已启用/)).toBeInTheDocument();
  });
});

describe("读取失败", () => {
  it("来源列表读取失败：显示服务端原因，不显示空状态", async () => {
    mount((async (url: string) => (url === "/v1/projects" ? json(200, { projects: [] }) : err(500, "internal", "store is unavailable"))) as unknown as typeof fetch);
    expect(await screen.findByRole("alert")).toHaveTextContent("store is unavailable");
    expect(screen.queryByText("还没有来源")).not.toBeInTheDocument();
  });
});

describe("webhook 密钥与入站记录重放", () => {
  it("创建后只显示一次明文；再次点击是轮换并提示旧密钥短期有效；换来源后明文不残留", async () => {
    const b = backend({ sources: [hook, rss] });
    mount(b.impl);
    await userEvent.click((await row("src_hook")).getByRole("button", { name: "详情" }));
    await userEvent.click(await screen.findByRole("button", { name: "创建或轮换密钥" }));
    expect(await screen.findByTestId("webhook-secret")).toHaveTextContent("apa_whk_SECRET123");
    expect(screen.getByText(/已创建/)).toBeInTheDocument();
    expect(b.calls.find((c) => c.path === "/v1/sources/src_hook/webhook")!.csrf).toBe("tok-1");
    await userEvent.click(screen.getByRole("button", { name: "创建或轮换密钥" }));
    expect(await screen.findByText(/已轮换，旧密钥短期内仍有效/)).toBeInTheDocument();
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    expect(screen.queryByTestId("webhook-secret")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "创建或轮换密钥" })).not.toBeInTheDocument();
  });

  it("入站记录列出校验结果并可看原始内容；重放后状态以服务端为准", async () => {
    const b = backend({ sources: [hook], ingests: [{ id: "ing_1", received_at: "2030-01-01T00:00:00Z", state: "rejected", signed: false, events: 0, reason: "bad signature", body: "{\"a\":1}" }] });
    mount(b.impl);
    await userEvent.click((await row("src_hook")).getByRole("button", { name: "详情" }));
    const region = within(await screen.findByRole("region", { name: "入站记录" }));
    expect(await region.findByText(/bad signature/)).toBeInTheDocument();
    expect(region.getByText(/未验签/)).toBeInTheDocument();
    await userEvent.click(region.getByRole("button", { name: "查看" }));
    expect(screen.getByTestId("ingest-body-ing_1")).toHaveTextContent('{"a":1}');
    await userEvent.click(region.getByRole("button", { name: "重放" }));
    expect(await region.findByText(/replayed/)).toBeInTheDocument();
    expect(b.calls.find((c) => c.path === "/v1/ingests/ing_1/replay")!.csrf).toBe("tok-1");
    expect(region.getByRole("status")).toHaveTextContent("已重放 ing_1");
  });
});

describe("未判定事件转建议（flow-triage-failure / ACT-TRI-02）", () => {
  it("只列未判定事件；转为建议后该事件离开列表且服务端处置为 proposed", async () => {
    const b = backend({ sources: [rss], events: [ev("e1"), ev("e2", "ignored"), ev("e3")] });
    mount(b.impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    const region = within(await screen.findByRole("region", { name: "未判定事件" }));
    expect(await region.findByText(/subject-e1/)).toBeInTheDocument();
    expect(region.getByText(/subject-e3/)).toBeInTheDocument();
    expect(region.queryByText(/subject-e2/)).not.toBeInTheDocument();
    await userEvent.click(region.getAllByRole("button", { name: "转为建议" })[0]);
    await region.findByText(/subject-e3/);
    expect(region.queryByText(/subject-e1/)).not.toBeInTheDocument();
    expect(b.events.find((e) => e.id === "e1")).toMatchObject({ disposition: "proposed", item: "item_e1" });
    const post = b.calls.find((c) => c.method === "POST" && c.path === "/v1/source-events/e1/propose")!;
    expect(post.csrf).toBe("tok-1");
  });

  it("没有未判定事件时显示空状态；分页用 next_after 加载更多", async () => {
    const b = backend({ sources: [rss, hook], events: [ev("e1"), ev("e2"), ev("e3")] });
    mount(b.impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    const region = within(await screen.findByRole("region", { name: "未判定事件" }));
    await region.findByText(/subject-e1/);
    expect(region.queryByText(/subject-e3/)).not.toBeInTheDocument();
    await userEvent.click(region.getByRole("button", { name: "加载更多" }));
    expect(await region.findByText(/subject-e3/)).toBeInTheDocument();
    expect(b.calls.some((c) => c.path.includes("after=2"))).toBe(true);
    expect(region.queryByRole("button", { name: "加载更多" })).not.toBeInTheDocument();
  });

  it("转换被拒绝（事件已不是 unjudged）：显示服务端原因，不假装成功", async () => {
    const b = backend({ sources: [rss], events: [ev("e1")] });
    mount(b.impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    const region = within(await screen.findByRole("region", { name: "未判定事件" }));
    await region.findByText(/subject-e1/);
    b.events[0].disposition = "ignored"; // 别处已处置
    await userEvent.click(region.getByRole("button", { name: "转为建议" }));
    expect(await region.findByRole("alert")).toHaveTextContent("event is ignored, not unjudged");
  });

  it("勾选按分数排序：请求带 sort=score，列表按服务端顺序显示每条的分数", async () => {
    const b = backend({ sources: [rss], events: [ev("e1"), { ...ev("e2"), score: 0.05 }, { ...ev("e3"), score: 0.9 }] });
    mount(b.impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    const region = within(await screen.findByRole("region", { name: "未判定事件" }));
    await region.findByText(/subject-e1/);
    expect(b.calls.some((c) => c.path.includes("sort=score"))).toBe(false);
    await userEvent.click(region.getByRole("checkbox", { name: "按分数排序" }));
    await region.findByText("分数 0.90");
    expect(b.calls.some((c) => c.path.includes("sort=score"))).toBe(true);
    const subjects = region.getAllByText(/^subject-/).map((x) => x.textContent);
    expect(subjects).toEqual(["subject-e3", "subject-e2"]); // 第一页两条：0.90、0.05；没有分数的 e1 在后面
    expect(region.getByText("分数 0.05")).toBeInTheDocument();
  });

  it("状态显示中文；可逐条忽略，也可两步确认后忽略本来源全部（含已过期的）", async () => {
    const b = backend({
      sources: [rss],
      events: [ev("e1"), { ...ev("e2"), unjudged_reason: "undecided" }, ev("e3", "expired"), ev("e4")],
    });
    mount(b.impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    const region = within(await screen.findByRole("region", { name: "未判定事件" }));
    expect(await region.findAllByText("等模型恢复后自动重判")).toHaveLength(1); // 第一页两条：e1、e2
    expect(region.getByText("判断未定，等你处理")).toBeInTheDocument();
    expect(region.queryByText(/unjudged|expired/)).not.toBeInTheDocument();
    await userEvent.click(within(region.getByRole("group", { name: "处理 e1" })).getByRole("button", { name: "忽略" }));
    await region.findByText("已过期，不再自动重判");
    expect(b.events.find((e) => e.id === "e1")!.disposition).toBe("ignored");
    await userEvent.click(region.getByRole("button", { name: "忽略本来源的全部未判定事件" }));
    expect(b.calls.some((c) => c.path === "/v1/source-events/ignore")).toBe(false); // 第一次点击只展开确认
    await userEvent.click(region.getByRole("button", { name: "确认忽略本来源的全部未判定事件" }));
    expect(await region.findByText("没有未判定事件")).toBeInTheDocument();
    const post = b.calls.find((c) => c.path === "/v1/source-events/ignore")!;
    expect(post.body).toEqual({ source: "src_rss" });
    expect(b.events.every((e) => e.disposition === "ignored")).toBe(true);
  });

  it("空状态", async () => {
    mount(backend({ sources: [rss] }).impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    expect(await screen.findByText("没有未判定事件")).toBeInTheDocument();
  });
});

describe("被拒收的事件（ACT-SRC-04）", () => {
  const rj = (n: number): Rj => ({ run_id: "run_1", line: n, reason: "reason-" + n, raw: "{bad-" + n, at: "2030-01-01T00:00:00Z" });

  it("列出原因与原始行；分页用 next_after 加载更多", async () => {
    const b = backend({ sources: [rss], rejects: [rj(1), rj(2), rj(3)] });
    mount(b.impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    const region = within(await screen.findByRole("region", { name: "被拒收的事件" }));
    expect(await region.findByText(/reason-1/)).toBeInTheDocument();
    expect(region.getByText("{bad-1")).toBeInTheDocument();
    expect(region.getByText(/运行 run_1 · 第 2 行/)).toBeInTheDocument();
    expect(region.queryByText(/reason-3/)).not.toBeInTheDocument();
    await userEvent.click(region.getByRole("button", { name: "加载更多" }));
    expect(await region.findByText(/reason-3/)).toBeInTheDocument();
    expect(b.calls.some((c) => c.path === "/v1/sources/src_rss/rejects?after=2")).toBe(true);
    expect(region.queryByRole("button", { name: "加载更多" })).not.toBeInTheDocument();
  });

  it("没有拒收记录时显示空状态", async () => {
    mount(backend({ sources: [rss] }).impl);
    await userEvent.click((await row("src_rss")).getByRole("button", { name: "详情" }));
    expect(await screen.findByText("没有被拒收的事件")).toBeInTheDocument();
  });
});

describe("登记自定义命令（flow-command-registration / ACT-CMD-04）", () => {
  async function fill(id: string, cmd: string) {
    const f = await screen.findByRole("region", { name: "登记自定义命令来源" });
    await userEvent.type(within(f).getByLabelText("来源 id"), id);
    await userEvent.type(within(f).getByLabelText("命令"), cmd);
    return f;
  }

  it("在 Web 登记 script 来源：POST 带 type=script、命令、周期与 CSRF，成功后出现在登记表", async () => {
    const b = backend();
    mount(b.impl);
    const f = await fill("src_check", "/opt/check.sh");
    await userEvent.type(within(f).getByLabelText("超时（秒）"), "30");
    await userEvent.selectOptions(within(f).getByLabelText("外部端点外发范围"), "excerpt");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    const post = b.calls.find((c) => c.method === "POST" && c.path === "/v1/sources")!;
    expect(post.body).toEqual({ id: "src_check", type: "script", command: "/opt/check.sh", schedule: "15m", timeout: 30 });
    expect(post.csrf).toBe("tok-1");
    const r = await row("src_check");
    expect(r.getByText(/\/opt\/check\.sh/)).toBeInTheDocument();
    expect(within(f).getByLabelText("来源 id")).toHaveValue("");
  });

  it("服务端拒绝时显示原因并保留输入，列表不出现该来源", async () => {
    const b = backend({ webOnly: true });
    mount(b.impl);
    const f = await fill("src_x", "/tmp/evil.sh");
    await userEvent.selectOptions(within(f).getByLabelText("外部端点外发范围"), "none");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    expect(await within(f).findByRole("alert")).toHaveTextContent("only be registered in the Web UI");
    expect(within(f).getByLabelText("命令")).toHaveValue("/tmp/evil.sh");
    expect(screen.queryByRole("listitem", { name: "src_x" })).not.toBeInTheDocument();
  });

  it("缺必填项时不能提交", async () => {
    mount(backend().impl);
    const f = await screen.findByRole("region", { name: "登记自定义命令来源" });
    expect(within(f).getByRole("button", { name: "登记" })).toBeDisabled();
  });
});

describe("外发范围（flow-outbound-scope / ACT-OBD-02、ACT-OBD-15）", () => {
  it("登记必须选外部范围：不选被表单拒绝且不发请求；本机范围预填全文；选后先登记再写范围，来源页显示两个范围", async () => {
    const b = backend();
    mount(b.impl);
    const f = await screen.findByRole("region", { name: "登记自定义命令来源" });
    await userEvent.type(within(f).getByLabelText("来源 id"), "src_obd");
    await userEvent.type(within(f).getByLabelText("来源类型（source_type，可选）"), "acc-obd");
    await userEvent.type(within(f).getByLabelText("命令"), "/opt/obd.sh");
    expect(within(f).getByLabelText("本机端点外发范围")).toHaveValue("full");
    expect(within(f).getByLabelText("外部端点外发范围")).toHaveValue(""); // 外部范围没有缺省值
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    expect(await within(f).findByRole("alert")).toHaveTextContent("必须选择外部外发范围");
    expect(b.calls.some((c) => c.method === "POST" && c.path === "/v1/sources")).toBe(false);
    expect(b.sources).toHaveLength(0);

    await userEvent.selectOptions(within(f).getByLabelText("外部端点外发范围"), "full");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    const r = await row("src_obd");
    expect(b.calls.find((c) => c.method === "POST" && c.path === "/v1/sources")!.body).toMatchObject({ source_type: "acc-obd" });
    const put = b.calls.find((c) => c.method === "PUT")!;
    expect(put.path).toBe("/v1/settings/sources/src_obd/outbound");
    expect(put.body).toEqual({ local: "full", external: "full" });
    expect(put.csrf).toBe("tok-1");
    await waitFor(() => expect(r.getByLabelText("本机范围")).toHaveValue("full"));
    expect(r.getByLabelText("外部范围")).toHaveValue("full");
  });

  it("来源页显示服务端的两个范围；未选择的外部范围显示未选择；改动即写回并以服务端回读为准", async () => {
    const b = backend({ sources: [{ ...rss, outbound: { local: "full", external: null } }, { ...hook, outbound: { local: "meta", external: "excerpt" } }] });
    mount(b.impl);
    const a = await row("src_rss");
    expect(a.getByLabelText("外部范围")).toHaveValue("");
    expect(a.getByRole("option", { name: "未选择" })).toBeInTheDocument();
    const h = await row("src_hook");
    expect(h.getByLabelText("本机范围")).toHaveValue("meta");
    expect(h.getByLabelText("外部范围")).toHaveValue("excerpt");
    await userEvent.selectOptions(h.getByLabelText("外部范围"), "full");
    await waitFor(() => expect(b.sources.find((x) => x.id === "src_hook")!.outbound).toEqual({ local: "meta", external: "full" }));
    expect((await row("src_hook")).getByLabelText("外部范围")).toHaveValue("full");
  });

  it("来源登记成功但范围写入失败：显示原因并提示重选，来源仍列出", async () => {
    const b = backend({ outboundFails: true });
    mount(b.impl);
    const f = await screen.findByRole("region", { name: "登记自定义命令来源" });
    await userEvent.type(within(f).getByLabelText("来源 id"), "src_p");
    await userEvent.type(within(f).getByLabelText("命令"), "/opt/p.sh");
    await userEvent.selectOptions(within(f).getByLabelText("外部端点外发范围"), "none");
    await userEvent.click(within(f).getByRole("button", { name: "登记" }));
    expect(await within(f).findByRole("alert")).toHaveTextContent(/来源已登记，但外发范围没有保存.*store is unavailable/);
    expect(await row("src_p")).toBeTruthy();
  });
});

describe("放回旧级联忽略的事件（flow-backlog-restore / ACT-RST-07）", () => {
  const fill = async (src?: string) => {
    const r = within(await screen.findByRole("region", { name: "把旧级联忽略的事件放回积压" }));
    await userEvent.type(r.getByLabelText("起（含）"), "2030-01-01T00:00");
    await userEvent.type(r.getByLabelText("止（不含）"), "2030-01-02T00:00");
    if (src) await userEvent.selectOptions(r.getByLabelText("来源"), src);
    return r;
  };

  it("先预览（条数与按来源分布，不写入）再执行；执行条数与预览一致，带 Idempotency-Key 与 CSRF", async () => {
    const b = backend({ sources: [rss, hook], restorable: [{ source: "src_rss", id: "a" }, { source: "src_rss", id: "b" }, { source: "src_hook", id: "c" }] });
    mount(b.impl);
    const r = await fill();
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled(); // 没预览不能执行
    await userEvent.click(r.getByRole("button", { name: "预览" }));
    const pv = within(await r.findByRole("status", { name: "放回预览" }));
    expect(pv.getByText("可放回 3 条")).toBeInTheDocument();
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
    expect(r.queryByRole("status", { name: "放回预览" })).not.toBeInTheDocument();
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled();
  });

  it("可限定来源；改条件后预览作废，必须重新预览", async () => {
    const b = backend({ sources: [rss, hook], restorable: [{ source: "src_rss", id: "a" }, { source: "src_hook", id: "c" }] });
    mount(b.impl);
    const r = await fill("src_hook");
    await userEvent.click(r.getByRole("button", { name: "预览" }));
    await r.findByText("可放回 1 条");
    await userEvent.selectOptions(r.getByLabelText("来源"), "src_rss");
    expect(r.queryByText("可放回 1 条")).not.toBeInTheDocument();
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled();
    await userEvent.click(r.getByRole("button", { name: "预览" }));
    await r.findByText("可放回 1 条");
    await userEvent.click(r.getByRole("button", { name: "放回积压" }));
    await r.findByText("已放回 1 条");
    expect(b.calls.find((c) => c.path === "/v1/source-events/restore")!.body.source).toBe("src_rss");
  });

  it("没有命中时不能执行；预览被服务端拒绝时显示原因", async () => {
    const b = backend({ sources: [rss] });
    mount(b.impl);
    const r = await fill();
    await userEvent.click(r.getByRole("button", { name: "预览" }));
    await r.findByText("可放回 0 条");
    expect(r.getByRole("button", { name: "放回积压" })).toBeDisabled();
    await userEvent.clear(r.getByLabelText("止（不含）"));
    await userEvent.type(r.getByLabelText("止（不含）"), "2029-12-31T00:00");
    await userEvent.click(r.getByRole("button", { name: "预览" }));
    expect(await r.findByRole("alert")).toHaveTextContent("from must be earlier than to");
  });
});
