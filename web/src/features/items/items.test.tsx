import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { traceRoute } from "../trace/testing";

// 测试边界替身：按 api/items、api/why 的契约应答（版本冲突 409、阶段进入约束 400、错误体见附录 B.3），
// 写入后追加 Event（Web 会话 = human/web），与 CLI 共用同一份"账本"。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const err = (status: number, code: string, why: string) => json(status, { code, why, next: "re-read the item and retry" });

interface Ev { seq: number; type: string; object: string; actor: { type: string; principal: string; via: string }; at: string; reason?: string; chain_depth: number; to?: string; fields?: string[] }
interface It { id: string; phase: string; version: number; kind: string; title: string; status_label?: string; tags?: string[]; deadline?: string | null; planned_for?: string | null; planned_slot?: string | null; waiting_on?: { kind: string; ref: string } }

const AGENT = { type: "agent", principal: "del-1", via: "cli" };
const HUMAN = { type: "human", principal: "owner", via: "web" };

function ledger() {
  const items: It[] = [
    { id: "item_a", phase: "open", version: 1, kind: "task", title: "item-a", tags: ["home", "urgent"] },
    { id: "item_b", phase: "open", version: 1, kind: "task", title: "item-b", tags: ["work"] },
  ];
  const rels: { type: string; from: string; to: string }[] = [];
  const events: Ev[] = [
    { seq: 1, type: "item.created", object: "item_a", actor: AGENT, at: "2030-01-01T00:00:00Z", reason: "created by agent", chain_depth: 0 },
    { seq: 2, type: "item.created", object: "item_b", actor: AGENT, at: "2030-01-01T00:01:00Z", chain_depth: 0 },
  ];
  const calls: { method: string; path: string; body?: any; csrf: string | null; ifMatch: string | null }[] = [];
  const push = (type: string, object: string, reason?: string, more: Partial<Ev> = {}) =>
    events.push({ seq: events.length + 1, type, object, actor: HUMAN, at: "2030-01-02T00:00:00Z", reason, chain_depth: 0, ...more });

  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    const h = (init.headers ?? {}) as Record<string, string>;
    calls.push({ method, path: url, body, csrf: h["X-CSRF-Token"] ?? null, ifMatch: h["If-Match"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/items" || url.startsWith("/v1/items?")) {
      const sp = new URL(url, "http://x").searchParams;
      const phase = sp.get("phase"), q = sp.get("q");
      return json(200, { items: items.filter((i) => (!phase || i.phase === phase) && (!q || i.title.includes(q))) });
    }
    if (/^\/v1\/items\/[^/]+\/messages$/.test(url)) return json(200, { messages: [] });
    const tr = traceRoute(url, events);
    if (tr) return tr;
    const m = url.match(/^\/v1\/items\/([^/]+)(?:\/(transition|relations))?$/);
    if (!m) return err(404, "not_found", "no such route");
    const it = items.find((i) => i.id === m[1]);
    if (!it) return err(404, "not_found", "item not found");
    if (method === "GET") return json(200, { item: it, relations: rels.filter((r) => r.from === it.id || r.to === it.id) });
    if (h["If-Match"] && Number(h["If-Match"]) !== it.version) return err(409, "conflict", "item version changed");
    if (m[2] === "relations") {
      rels.push({ type: body.type, from: it.id, to: body.to });
      push("item.related", it.id);
      return json(200, { id: it.id, version: it.version });
    }
    if (m[2] === "transition") {
      if (body.to === "waiting" && !body.waiting_on) return err(400, "invalid_request", "waiting phase requires waiting_on");
      it.phase = body.to;
      if (body.waiting_on) it.waiting_on = body.waiting_on;
      it.version++;
      push("item.phase_changed", it.id, body.reason, { to: body.to });
      return json(200, { id: it.id, version: it.version });
    }
    Object.assign(it, body);
    it.version++;
    push("item.updated", it.id, undefined, { fields: Object.keys(body).filter((k) => k !== "reason") });
    return json(200, { id: it.id, version: it.version });
  }) as unknown as typeof fetch;
  return { impl, items, rels, calls, events, bump: (id: string) => items.find((i) => i.id === id)!.version++ };
}

function mount(fetchImpl: typeof fetch, path = "/items") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App features={[feature]} fetchImpl={fetchImpl} />
    </MemoryRouter>,
  );
}

const timeline = async () => within(await screen.findByRole("list", { name: "处理经过" }));

beforeEach(() => sessionStorage.clear());

describe("事项列表与详情（ACT-ITEM-01）", () => {
  it("CLI 建的事项在 Web 可见：标题、阶段 open，经过里一句「Agent（凭据名）· 新建了」", async () => {
    mount(ledger().impl);
    await userEvent.click(await screen.findByRole("link", { name: "item-a" }));
    expect(await screen.findByText(/现在：进行中/)).toBeInTheDocument();
    const tl = await timeline();
    expect(tl.getAllByRole("listitem")).toHaveLength(1);
    expect(tl.getByRole("listitem")).toHaveTextContent(/^Agent（del-1）新建了/);
    expect(tl.getByRole("listitem").textContent).not.toMatch(/item\.created|item_a/); // 不露事件名与 id
  });

  it("读取失败时显示服务端的原因，不显示空列表", async () => {
    const fail = (async (u: string) => (u === "/v1/projects" ? json(200, {}) : err(403, "permission_denied", "credential kind cannot read items"))) as unknown as typeof fetch;
    mount(fail);
    expect(await screen.findByRole("alert")).toHaveTextContent("credential kind cannot read items");
  });
});

describe("改 status_label（ACT-ITEM-02）", () => {
  it("带 If-Match 版本和 CSRF 写入，version 加一，时间线多一条本人 web 记录；重新读取与账本一致", async () => {
    const l = ledger();
    mount(l.impl, "/items/item_a");
    const input = await screen.findByLabelText("状态标签");
    await userEvent.type(input, "review");
    await userEvent.click(screen.getByRole("button", { name: "保存状态标签" }));

    expect(await screen.findByText("状态标签：review")).toBeInTheDocument(); // 重新读取后显示服务端状态
    const patch = l.calls.find((c) => c.method === "PATCH")!;
    expect(patch).toMatchObject({ path: "/v1/items/item_a", body: { status_label: "review" }, csrf: "tok-1", ifMatch: "1" });
    expect(l.items[0]).toMatchObject({ status_label: "review", version: 2 }); // CLI 读同一份账本
    const tl = await timeline();
    expect(tl.getAllByRole("listitem")).toHaveLength(2);
    expect(tl.getAllByRole("listitem")[1]).toHaveTextContent(/^你改了状态标签/);
  });

  it("别处已改过（409）：显示冲突原因，页面状态不被覆盖", async () => {
    const l = ledger();
    mount(l.impl, "/items/item_a");
    const input = await screen.findByLabelText("状态标签");
    l.bump("item_a"); // 另一入口先写了
    await userEvent.type(input, "review");
    await userEvent.click(screen.getByRole("button", { name: "保存状态标签" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("item version changed");
    expect(l.items[0].status_label).toBeUndefined();
  });
});

describe("转阶段", () => {
  it("waiting 缺 waiting_on 被服务端拒绝并显示原因，阶段不变；补上后成功", async () => {
    const l = ledger();
    mount(l.impl, "/items/item_a");
    await userEvent.selectOptions(await screen.findByLabelText("目标阶段"), "waiting");
    await userEvent.type(screen.getByLabelText("原因"), "等报价");
    await userEvent.click(screen.getByRole("button", { name: "转阶段" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("waiting phase requires waiting_on");
    expect(screen.getByText(/现在：进行中/)).toBeInTheDocument();

    await userEvent.selectOptions(screen.getByLabelText("等待对象类别"), "external");
    await userEvent.type(screen.getByLabelText("等待对象"), "报价单");
    await userEvent.click(screen.getByRole("button", { name: "转阶段" }));
    expect(await screen.findByText(/现在：等待中/)).toBeInTheDocument();
    expect(l.calls.filter((c) => c.path.endsWith("/transition")).at(-1)!.body).toEqual({ to: "waiting", reason: "等报价", waiting_on: { kind: "external", ref: "报价单" } });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});

describe("设截止（ACT-LIF-03）", () => {
  it("在排期区块写入截止（带时刻）、只到日的显示日期，可清除", async () => {
    const l = ledger();
    mount(l.impl, "/items/item_a");
    const block = within(await screen.findByRole("region", { name: "排期" }));
    fireEvent.change(block.getByLabelText("截止"), { target: { value: "2030-03-01" } });
    await userEvent.click(block.getByLabelText("具体时刻"));
    fireEvent.change(block.getByLabelText("截止时刻"), { target: { value: "09:30" } });
    await userEvent.click(block.getByRole("button", { name: "保存排期" }));

    await waitFor(() => expect(Date.parse(l.items[0].deadline!)).toBe(new Date("2030-03-01T09:30").getTime()));
    expect(l.calls.find((c) => c.method === "PATCH")).toMatchObject({ ifMatch: "1" });
    expect(await screen.findByText(/截止 2030-03-01 09:30/)).toBeInTheDocument();

    await userEvent.click(block.getByLabelText("具体时刻"));
    await userEvent.click(block.getByRole("button", { name: "保存排期" }));
    await waitFor(() => expect(Date.parse(l.items[0].deadline!)).toBe(new Date("2030-03-01T23:59:59").getTime()));
    expect(await screen.findByText(/截止 2030-03-01(?! )/)).toBeInTheDocument(); // 只到日：不写 23:59

    fireEvent.change(block.getByLabelText("截止"), { target: { value: "" } });
    await userEvent.click(block.getByRole("button", { name: "保存排期" }));
    await waitFor(() => expect(l.items[0].deadline).toBeNull());
  });
});

describe("排期区块的并发（W26）", () => {
  it("别处先改过：服务端返回冲突，载入最新值、保留本人填的并标出不同的项，确认后带新版本号重存；清日期一起清时段", async () => {
    const l = ledger();
    mount(l.impl, "/items/item_a");
    const block = within(await screen.findByRole("region", { name: "排期" }));
    fireEvent.change(block.getByLabelText("计划日期"), { target: { value: "2030-03-02" } });
    await userEvent.click(block.getByLabelText("不定时"));
    fireEvent.change(block.getByLabelText("开始"), { target: { value: "14:00" } });
    fireEvent.change(block.getByLabelText("结束"), { target: { value: "15:30" } });
    Object.assign(l.items[0], { planned_for: "2030-03-09", version: 2 }); // 另一处（如今天页的改期）先写了
    await userEvent.click(block.getByRole("button", { name: "保存排期" }));

    expect(await block.findByText("这件事刚被别处改过，已载入最新值")).toBeInTheDocument();
    expect(block.getByLabelText(/^计划日期/)).toHaveValue("2030-03-02"); // 本人填的还在
    expect(block.getByText("最新：2030-03-09")).toBeInTheDocument(); // 标出与最新值不同的项
    expect(l.items[0].planned_slot).toBeUndefined(); // 冲突的那次没写进去
    await userEvent.click(block.getByRole("button", { name: "确认并保存" }));
    await waitFor(() => expect(l.items[0]).toMatchObject({ planned_for: "2030-03-02", planned_slot: "14:00 - 15:30" }));
    expect(l.calls.filter((c) => c.method === "PATCH").map((c) => c.ifMatch)).toEqual(["1", "2"]);

    fireEvent.change(block.getByLabelText(/^计划日期/), { target: { value: "" } });
    await userEvent.click(block.getByRole("button", { name: "保存排期" }));
    await waitFor(() => expect(l.items[0].planned_for).toBeNull());
    expect(l.calls.filter((c) => c.method === "PATCH").at(-1)!.body).toEqual({ planned_for: null, reason: "在事项页改排期" }); // 时段由内核随日期一起清
  });
});

describe("关联与按 tag 筛选（ACT-ITEM-12）", () => {
  it("在两个事项间建 relates 边，详情两端都能看到（关系显示名字与对方标题）", async () => {
    const l = ledger();
    mount(l.impl, "/items/item_a");
    await userEvent.selectOptions(await screen.findByLabelText("关联事项"), "item_b");
    await userEvent.selectOptions(screen.getByLabelText("关系类型"), "relates");
    await userEvent.click(screen.getByRole("button", { name: "建立关联" }));
    const rels = within(await screen.findByRole("list", { name: "关系" }));
    expect(rels.getByText(/相关 →/)).toBeInTheDocument(); // 关系类型走词表，不露 relates
    expect(await rels.findByRole("link", { name: "item-b" })).toHaveAttribute("href", "/items/item_b"); // 显示对方标题，不露 id
    expect(screen.getByRole("link", { name: /事项列表/ })).toHaveAttribute("href", "/items");
    expect(l.calls.find((c) => c.path.endsWith("/relations"))!.body).toEqual({ to: "item_b", type: "relates" });
  });

  it("材料作为关系终点：显示中文类型，链接指向材料页而不是事项页", async () => {
    const l = ledger();
    mount(l.impl, "/items/item_a");
    await screen.findByRole("list", { name: "关系" });
    l.rels.push({ type: "basis", from: "item_a", to: "mat_x" });
    await userEvent.selectOptions(screen.getByLabelText("关联事项"), "item_b");
    await userEvent.click(screen.getByRole("button", { name: "建立关联" }));
    // 指向材料的依据关系进材料区的“依据”组（读不到材料时按 id 显示），链接指向材料页。
    const basis = within(await screen.findByRole("group", { name: "依据" }));
    expect(basis.getByRole("link", { name: "mat_x" })).toHaveAttribute("href", "/materials?id=mat_x");
  });

  it("按 tag 筛选只留下带该 tag 的事项；按阶段筛选走服务端", async () => {
    const l = ledger();
    l.items[1].phase = "done";
    mount(l.impl);
    expect(await screen.findByRole("link", { name: "item-b" })).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("标签筛选"), "urgent");
    expect(screen.getByRole("link", { name: "item-a" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "item-b" })).not.toBeInTheDocument();

    await userEvent.selectOptions(screen.getByLabelText("标签筛选"), "");
    await userEvent.selectOptions(screen.getByLabelText("阶段筛选"), "done");
    await waitFor(() => expect(screen.queryByRole("link", { name: "item-a" })).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "item-b" })).toBeInTheDocument();
    expect(l.calls.some((c) => c.path === "/v1/items?phase=done")).toBe(true);
  });

  it("搜索框、排序与计划日期筛选都走服务端参数", async () => {
    const l = ledger();
    mount(l.impl);
    await screen.findByRole("link", { name: "item-b" });
    await userEvent.type(screen.getByLabelText("搜索"), "item-a");
    await waitFor(() => expect(screen.queryByRole("link", { name: "item-b" })).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "item-a" })).toBeInTheDocument();
    await userEvent.selectOptions(screen.getByLabelText("排序"), "planned_for");
    await userEvent.type(screen.getByLabelText("计划日期"), "2026-05-01");
    await waitFor(() => expect(l.calls.some((c) => c.path === "/v1/items?q=item-a&sort=planned_for&planned_for=2026-05-01")).toBe(true));
  });
});
