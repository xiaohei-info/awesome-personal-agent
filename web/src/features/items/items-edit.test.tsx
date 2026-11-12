import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";

// 测试边界替身：按 api/items 的契约应答（POST /items 201、PATCH 带 If-Match 推进 version、POST correlation-keys），
// 并像服务端一样把每次写入记成 Event（Web 会话 = human/web）。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const BODY = "## 行程\n- 周五出发\n[航司](https://example.com)\n<script>window.__wbn=1</script>\n![外图](https://example.org/a.png)";

function ledger() {
  const items: any[] = [];
  const keys: { key: string }[] = [];
  const events: { seq: number; type: string; object: string; actor: any; at: string; chain_depth: number }[] = [];
  const calls: { method: string; path: string; body?: any; ifMatch: string | null }[] = [];
  const ev = (type: string, object: string) =>
    events.push({ seq: events.length + 1, type, object, actor: { type: "human", principal: "owner", via: "web" }, at: "2030-01-02T00:00:00Z", chain_depth: 0 });
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(init.body as string) : undefined;
    const h = (init.headers ?? {}) as Record<string, string>;
    calls.push({ method, path: url, body, ifMatch: h["If-Match"] ?? null });
    if (url === "/v1/projects") return json(200, { projects: [{ id: "acc-home", name: "家" }, { id: "acc-work" }] });
    if (url === "/v1/items" && method === "POST") {
      const it = { id: "item_new", version: 1, kind: body.kind, ...body };
      items.push(it);
      ev("item.created", it.id);
      return json(201, { id: it.id, version: 1 });
    }
    if (url === "/v1/items" || url.startsWith("/v1/items?")) return json(200, { items });
    if (/\/messages$/.test(url)) return json(200, { messages: [] });
    const why = url.match(/^\/v1\/why\/(.+)$/);
    if (why) return json(200, { id: why[1], steps: events.filter((e) => e.object === why[1]) });
    const m = url.match(/^\/v1\/items\/([^/]+)(\/correlation-keys)?$/);
    const it = items.find((i) => i.id === m?.[1]);
    if (!m || !it) return json(404, { code: "not_found", why: "item not found", next: "check the id" });
    if (method === "GET") return json(200, { item: it, relations: [], correlation_keys: keys });
    if (Number(h["If-Match"]) !== it.version) return json(409, { code: "conflict", why: "item version changed", next: "re-read" });
    if (m[2]) {
      keys.push({ key: body.key });
      it.version++;
      ev("item.updated", it.id);
    } else {
      Object.assign(it, body);
      it.version++;
      ev("item.updated", it.id);
    }
    return json(200, { id: it.id, version: it.version });
  }) as unknown as typeof fetch;
  return { impl, items, calls };
}

function mount(l: ReturnType<typeof ledger>, path = "/items") {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(<MemoryRouter initialEntries={[path]}><App features={[feature]} fetchImpl={l.impl} /></MemoryRouter>);
}
const field = (name: string) => screen.findByLabelText(name);

beforeEach(() => sessionStorage.clear());

describe("Web 新建与编辑事项", () => {
  it("ACT-WBN-02 新建事项直接 open，同时写入项目、截止、计划日期与正文，并跳到详情", async () => {
    const l = ledger();
    const u = userEvent.setup();
    mount(l);
    await u.click(await screen.findByRole("button", { name: "新建" }));
    await u.type(await field("标题"), "WB-NEW 订机票");
    await u.selectOptions(await field("项目"), "acc-home");
    await u.type(await field("截止"), "2030-03-05T18:00");
    await u.type(await field("计划日期"), "2030-03-04");
    await u.click(await field("正文"));
    await u.paste(BODY);
    await u.click(screen.getByRole("button", { name: "创建" }));

    await screen.findByRole("heading", { name: "WB-NEW 订机票", level: 1 });
    const post = l.calls.find((c) => c.method === "POST" && c.path === "/v1/items")!;
    expect(post.body).toMatchObject({ kind: "task", phase: "open", title: "WB-NEW 订机票", project: "acc-home", planned_for: "2030-03-04", body: BODY });
    expect(post.body.deadline).toBe(new Date("2030-03-05T18:00").toISOString());
    expect((await timelineText())).toContain("item.created");
  });

  it("新建缺标题时不发请求", async () => {
    const l = ledger();
    const u = userEvent.setup();
    mount(l);
    await u.click(await screen.findByRole("button", { name: "新建" }));
    await u.click(await screen.findByRole("button", { name: "创建" }));
    expect(l.calls.some((c) => c.method === "POST")).toBe(false);
    expect(await screen.findByRole("alert")).toHaveTextContent("标题");
  });

  it("ACT-WBN-03 正文按 Markdown 渲染并净化：无 script，外图点开前不加载", async () => {
    const l = ledger();
    l.items.push({ id: "item_x", phase: "open", version: 1, kind: "task", title: "t", body: BODY });
    const u = userEvent.setup();
    const { container } = mount(l, "/items/item_x");
    expect(await screen.findByRole("heading", { name: "行程", level: 2 })).toBeInTheDocument();
    expect(screen.getByText("周五出发").closest("li")).not.toBeNull();
    const a = screen.getByRole("link", { name: "航司" });
    expect(a).toHaveAttribute("href", "https://example.com");
    expect(a).toHaveAttribute("rel", expect.stringContaining("noopener"));
    expect(container.querySelector("script")).toBeNull();
    expect((window as any).__wbn).toBeUndefined();
    expect(container.querySelector("img")).toBeNull(); // 点开前没有任何图片请求
    await u.click(screen.getByRole("button", { name: /外图/ }));
    expect(container.querySelector("img")).toHaveAttribute("src", "https://example.org/a.png");
  });

  it("javascript: 链接不渲染成可点击链接", async () => {
    const l = ledger();
    l.items.push({ id: "item_x", phase: "open", version: 1, kind: "task", title: "t", body: "[点我](javascript:alert(1))" });
    mount(l, "/items/item_x");
    await screen.findByText(/点我/);
    expect(screen.queryByRole("link", { name: "点我" })).toBeNull();
  });

  it("ACT-WBN-04 编辑标题与正文：带 If-Match，version 加 1，时间线出现 item.updated", async () => {
    const l = ledger();
    l.items.push({ id: "item_x", phase: "open", version: 3, kind: "task", title: "旧", body: "- a" });
    const u = userEvent.setup();
    mount(l, "/items/item_x");
    await u.click(await screen.findByRole("button", { name: "编辑" }));
    const title = await field("标题");
    await u.clear(title);
    await u.type(title, "新标题");
    const body = await field("正文");
    await u.clear(body);
    await u.click(body);
    await u.paste("- a\n- b");
    await u.click(screen.getByRole("button", { name: "保存" }));
    await screen.findByRole("heading", { name: "新标题", level: 1 });
    const patch = l.calls.find((c) => c.method === "PATCH")!;
    expect(patch.ifMatch).toBe("3");
    expect(patch.body).toEqual({ title: "新标题", body: "- a\n- b" });
    expect(await screen.findByText("版本：4")).toBeInTheDocument();
    expect(await timelineText()).toContain("item.updated");
  });

  it("版本冲突时显示服务端原因，不留本地假状态", async () => {
    const l = ledger();
    l.items.push({ id: "item_x", phase: "open", version: 1, kind: "task", title: "旧" });
    const u = userEvent.setup();
    mount(l, "/items/item_x");
    await u.click(await screen.findByRole("button", { name: "编辑" }));
    l.items[0].version = 2;
    const title = await field("标题");
    await u.clear(title);
    await u.type(title, "新");
    await u.click(screen.getByRole("button", { name: "保存" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("item version changed");
    expect(l.items[0].title).toBe("旧");
  });

  it("ACT-MSG-16 登记关联来源线程：详情显示该键，时间线有 item.updated", async () => {
    const l = ledger();
    l.items.push({ id: "item_x", phase: "open", version: 1, kind: "task", title: "t" });
    const u = userEvent.setup();
    mount(l, "/items/item_x");
    await u.type(await field("关联来源线程"), "fm-thread-5");
    await u.click(screen.getByRole("button", { name: "登记关联" }));
    const list = await screen.findByRole("list", { name: "关联键" });
    await waitFor(() => expect(within(list).getByText("fm-thread-5")).toBeInTheDocument());
    const post = l.calls.find((c) => c.path === "/v1/items/item_x/correlation-keys")!;
    expect(post.body).toEqual({ key: "fm-thread-5" });
    expect(post.ifMatch).toBe("1");
    expect(await timelineText()).toContain("item.updated");
  });
});

async function timelineText() {
  await waitFor(() => expect(screen.getByRole("list", { name: "时间线" }).textContent).not.toBe(""));
  return screen.getByRole("list", { name: "时间线" }).textContent ?? "";
}
