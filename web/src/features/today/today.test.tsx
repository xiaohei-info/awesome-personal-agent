import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../../app/App";
import { feature } from "./index";
import { feature as items } from "../items";

// 测试边界替身：按 api/items 的契约应答 GET /today、PATCH /items/{id}、POST /items/{id}/progress|transition。
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const mk = (id: string, over: object = {}) => ({ id, phase: "open", version: 1, kind: "task", title: "事项 " + id, ...over });

function server(today: Record<string, unknown[]>, fail: (path: string) => boolean = () => false, extra: Record<string, unknown> = {}) {
  const calls: { method: string; path: string; body?: any; headers: Record<string, string> }[] = [];
  const impl = (async (url: string, init: RequestInit = {}) => {
    const method = init.method ?? "GET";
    calls.push({ method, path: url, body: init.body ? JSON.parse(init.body as string) : undefined, headers: (init.headers ?? {}) as Record<string, string> });
    if (url === "/v1/projects") return json(200, { projects: [] });
    if (url === "/v1/today") return json(200, today);
    if (method === "GET" && url in extra) return json(200, extra[url]);
    if (method !== "GET" && fail(url)) return json(409, { code: "conflict", why: "version moved", next: "reload" });
    if (method !== "GET") return json(200, { id: "x", version: 2 });
    return json(404, { code: "not_found", why: "no route", next: "x" });
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function mount(f: typeof fetch) {
  sessionStorage.setItem("apa.csrf", "tok-1");
  return render(
    <MemoryRouter initialEntries={["/today"]}>
      <App features={[feature, items]} fetchImpl={f} />
    </MemoryRouter>,
  );
}
const writes = (s: ReturnType<typeof server>) => s.calls.filter((c) => c.method !== "GET");
beforeEach(() => sessionStorage.clear());

describe("今天页（flow-web-workbench）", () => {
  it("ACT-WBN-08：按服务端取材展示；计划今天或更早与今天到期合并成「今天的事」，过期的计划事项仍在；久未推进默认折叠", async () => {
    const s = server({
      planned: [mk("WB-A", { planned_for: "2000-01-01" }), mk("WB-B", { planned_for: "2000-01-02" })],
      due: [mk("WB-D", { deadline: "2030-01-01T00:00:00Z" }), mk("WB-A")],
      pending: [mk("WB-G", { phase: "needs_input" })],
      stale: [mk("WB-H")],
    });
    mount(s.impl);
    const todays = await screen.findByRole("region", { name: "今天的事" });
    expect(within(todays).getByText("事项 WB-A")).toBeInTheDocument(); // 既在计划又在到期的只列一次
    expect(within(todays).getByText("事项 WB-B")).toBeInTheDocument();
    expect(within(todays).getByText("事项 WB-D")).toBeInTheDocument();
    expect(within(todays).getByRole("heading")).toHaveTextContent("（3）");
    const stale = screen.getByRole("group", { name: "久未推进" });
    expect(stale).not.toHaveAttribute("open");
    expect(within(stale).getByText("事项 WB-H")).toBeInTheDocument();
    expect(writes(s)).toHaveLength(0); // 浏览不改任何事项（不自动改期）
  });

  it("ACT-WBN-08：改计划日期、写进展、完成各发一次写入，之后重读分组", async () => {
    const s = server({ planned: [mk("WB-A", { version: 3 }), mk("WB-B")], due: [], pending: [], stale: [] });
    mount(s.impl);
    const user = userEvent.setup();
    const a = (await screen.findByText("事项 WB-A")).closest("li")!;
    await user.type(within(a).getByLabelText("改计划日期 事项 WB-A"), "2031-05-06");
    await user.click(within(a).getByRole("button", { name: "改计划日期" }));
    await waitFor(() => expect(writes(s)).toHaveLength(1));
    expect(writes(s)[0]).toMatchObject({ method: "PATCH", path: "/v1/items/WB-A", body: { planned_for: "2031-05-06" } });
    expect(writes(s)[0].headers["If-Match"]).toBe("3");

    const b = (await screen.findByText("事项 WB-B")).closest("li")!;
    await user.type(within(b).getByLabelText("进展"), "已订酒店");
    await user.click(within(b).getByRole("button", { name: "写进展" }));
    await waitFor(() => expect(writes(s)).toHaveLength(2));
    expect(writes(s)[1]).toMatchObject({ method: "POST", path: "/v1/items/WB-B/progress", body: { text: "已订酒店" } });

    await user.click(within((await screen.findByText("事项 WB-B")).closest("li")!).getByRole("button", { name: "完成" }));
    await waitFor(() => expect(writes(s)).toHaveLength(3));
    expect(writes(s)[2]).toMatchObject({ method: "POST", path: "/v1/items/WB-B/transition", body: { to: "done" } });
    await waitFor(() => expect(s.calls.filter((c) => c.path === "/v1/today").length).toBe(4)); // 首次 + 三次写后重读
  });

  it("服务端拒绝写入时显示原因并重读，不留下本地假状态", async () => {
    const s = server({ planned: [mk("WB-A")], due: [], pending: [], stale: [] }, (p) => p.endsWith("/transition"));
    mount(s.impl);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "完成" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("version moved");
    expect(screen.getByText("事项 WB-A")).toBeInTheDocument();
  });

  it("久未推进：批量选继续、改期、放弃，逐条走既有接口；单条失败不影响其余", async () => {
    const s = server({ planned: [], due: [], pending: [], stale: [mk("S1"), mk("S2")] }, (p) => p === "/v1/items/S2/progress");
    mount(s.impl);
    const user = userEvent.setup();
    await user.click(await screen.findByLabelText("选择 事项 S1"));
    await user.click(screen.getByLabelText("选择 事项 S2"));
    await user.click(screen.getByRole("button", { name: /应用到所选/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("事项 S2：内容已被改动（version moved）");
    expect(writes(s).map((c) => c.path)).toEqual(["/v1/items/S1/progress", "/v1/items/S2/progress"]);

    await user.click(screen.getByLabelText("选择 事项 S1"));
    await user.selectOptions(screen.getByLabelText("处理方式"), "reschedule");
    expect(screen.getByRole("button", { name: /应用到所选/ })).toBeDisabled(); // 没填日期不能提交
    await user.type(screen.getByLabelText("批量改期日期"), "2031-01-02");
    await user.click(screen.getByRole("button", { name: /应用到所选/ }));
    await waitFor(() => expect(writes(s)).toHaveLength(3));
    expect(writes(s)[2]).toMatchObject({ method: "PATCH", body: { planned_for: "2031-01-02" } });

    await user.click(screen.getByLabelText("选择 事项 S1"));
    await user.selectOptions(screen.getByLabelText("处理方式"), "drop");
    await user.click(screen.getByRole("button", { name: /应用到所选/ }));
    await waitFor(() => expect(writes(s)).toHaveLength(4));
    expect(writes(s)[3]).toMatchObject({ path: "/v1/items/S1/transition", body: { to: "dropped" } });
  });

  it("要你处理：与导航同一份汇总，等你决定与新建议各一行带数字和按钮；提醒扣除决策类与系统类，可「知道了」（界面设计 §5.2）", async () => {
    const asked = (id: string) => ({ ...mk(id, { phase: "needs_input" }), ask: { question: "q" } });
    const s = server({ planned: [], due: [], pending: [], stale: [] }, () => false, {
      "/v1/items?phase=needs_input": { items: [asked("P1"), asked("P2")] },
      "/v1/items?phase=proposed": { items: [mk("N1", { phase: "proposed" })] },
      "/v1/deliveries/unread": { deliveries: [
        { id: "d1", item: "P1", class: "decision", content: { title: "决策提醒" } },
        { id: "d2", item: "X", class: "health", content: { title: "系统提醒" } },
        { id: "d3", item: "R", class: "reminder", content: { title: "交物业费" }, suppressed: "daily_cap" },
      ] },
    });
    mount(s.impl);
    const top = await screen.findByRole("region", { name: "要你处理" });
    const decide = (await within(top).findByText("2 件事等你决定")).closest("li")!;
    expect(within(decide).getByRole("link", { name: "去回答" })).toHaveAttribute("href", "/decisions");
    expect(within(within(top).getByText("1 条新建议").closest("li")!).getByRole("link", { name: "去看建议" })).toHaveAttribute("href", "/proposals");
    const reminders = await screen.findByRole("region", { name: "提醒" });
    expect(within(reminders).getAllByRole("listitem")).toHaveLength(1);
    expect(within(reminders).getByText("交物业费")).toBeInTheDocument();
    await userEvent.click(within(reminders).getByRole("button", { name: "知道了" }));
    await waitFor(() => expect(writes(s).map((c) => c.path)).toEqual(["/v1/deliveries/d3/read"]));
  });

  it("底部一行完成计数并指向已完成页；做完的事不在今天页重复列出", async () => {
    const now = new Date().toISOString();
    const done = { items: [mk("WB-X", { phase: "done" })], done_at: { "WB-X": now } };
    mount(server({ planned: [], due: [], pending: [], stale: [] }, () => false, { "/v1/items?phase=done": done }).impl);
    expect(await screen.findByText(/今天完成 1 件 · 本周完成 1 件/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "看已完成" })).toHaveAttribute("href", "/done");
    expect(screen.queryByText("事项 WB-X")).toBeNull();
  });

  it("全空时只有一句话", async () => {
    mount(server({ planned: [], due: [], pending: [], stale: [] }).impl);
    expect(await screen.findByText("现在没有等你的事")).toBeInTheDocument();
    expect(screen.queryByRole("region")).not.toBeInTheDocument();
  });
});

// 原「未读提醒」并进今天页的提醒区（界面设计 §3.1；flow-hermes-context / ACT-HCTX-06）。
describe("今天 › 提醒", () => {
  const d = (id: string, over: object = {}) => ({ id, item: "itm_" + id, class: "reminder", version: 1, state: "pending", content: { title: "投递 " + id }, ...over });
  function srv(list: unknown[], read?: () => Response) {
    let rows = list as { id: string }[];
    const calls: { method: string; path: string; csrf?: string }[] = [];
    const impl = (async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? "GET";
      calls.push({ method, path: url, csrf: (init.headers as Record<string, string> | undefined)?.["X-CSRF-Token"] });
      if (url === "/v1/projects") return json(200, { projects: [] });
      if (url === "/v1/today") return json(200, { planned: [], due: [], pending: [], stale: [] });
      if (url === "/v1/deliveries/unread") return json(200, { deliveries: rows });
      const m = /^\/v1\/deliveries\/(.+)\/read$/.exec(url);
      if (m && method === "POST") {
        if (read) return read();
        rows = rows.filter((x) => x.id !== m[1]);
        return json(200, { id: m[1] });
      }
      return json(404, { code: "not_found", why: "no route", next: "x" });
    }) as unknown as typeof fetch;
    return { impl, calls };
  }
  const region = () => screen.findByRole("region", { name: /提醒/ });

  it("标题、摘要与打开链接；最新的在最前；打开页面本身不确认任何提醒", async () => {
    const s = srv([d("old", { content: { title: "较早的" } }), d("new", { content: { title: "较新的", summary: "到期前处理", web_url: "/items/x" } })]);
    mount(s.impl);
    const r = within(await region());
    expect(r.getAllByRole("listitem").map((li) => li.getAttribute("aria-label"))).toEqual(["较新的", "较早的"]);
    const row = within(r.getByRole("listitem", { name: "较新的" }));
    expect(row.getByText("到期前处理")).toBeInTheDocument();
    expect(row.getByRole("link", { name: "打开" })).toHaveAttribute("href", "/items/x");
    expect(s.calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("已在聊天里提过的照样可见，且不会因此发出确认；被日上限或暂停压下的标出原因", async () => {
    const s = srv([d("a", { injections: [{ injected_to_session: "s1", at: "2030-01-01T00:00:00Z" }, { injected_to_session: "s2", at: "2030-01-01T00:01:00Z" }] }), d("b", { suppressed: "daily_cap" }), d("c", { suppressed: "muted" }), d("e")]);
    mount(s.impl);
    const r = within(await region());
    expect(within(r.getByRole("listitem", { name: "投递 a" })).getByText(/已在 2 次聊天里提过/)).toBeInTheDocument();
    expect(r.getByRole("listitem", { name: "投递 b" })).toHaveTextContent("超过今天上限");
    expect(r.getByRole("listitem", { name: "投递 c" })).toHaveTextContent("已暂停提醒");
    expect(within(r.getByRole("listitem", { name: "投递 e" })).queryByText(/超过今天上限|已暂停提醒/)).not.toBeInTheDocument();
    expect(s.calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("本人点「知道了」才确认：POST read 带 CSRF，成功后该条消失、其余仍在", async () => {
    const s = srv([d("a"), d("b")]);
    mount(s.impl);
    await userEvent.click(within((await region()).querySelector("[aria-label='投递 a']") as HTMLElement).getByRole("button", { name: "知道了" }));
    await waitFor(() => expect(screen.queryByRole("listitem", { name: "投递 a" })).not.toBeInTheDocument());
    expect(screen.getByRole("listitem", { name: "投递 b" })).toBeInTheDocument();
    expect(s.calls.filter((c) => c.method === "POST")).toEqual([{ method: "POST", path: "/v1/deliveries/a/read", csrf: "tok-1" }]);
  });

  it("确认失败（409）：显示服务端原因，该条仍在，并重新读取", async () => {
    const s = srv([d("a")], () => json(409, { code: "conflict", why: "delivery a is acknowledged, not an unread web item", next: "refresh" }));
    mount(s.impl);
    await userEvent.click(within(await region()).getByRole("button", { name: "知道了" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("not an unread web item");
    expect(screen.getByRole("listitem", { name: "投递 a" })).toBeInTheDocument();
    await waitFor(() => expect(s.calls.filter((c) => c.path === "/v1/deliveries/unread").length).toBeGreaterThan(1));
  });
});

describe("发布门槛（flow-release-gates）", () => {
  it("ACT-GATE-09：§4.1 写明 360 像素走查结论，§21.3 的 SPIKE-WEB-MOBILE 行已关闭", async () => {
    const mods = import.meta.glob<string>("../../../../docs/solution-design.md", { query: "?raw", import: "default", eager: true });
    const doc = Object.values(mods)[0];
    const s41 = doc.slice(doc.indexOf("### 4.1 Web 中台"), doc.indexOf("### 4.2"));
    expect(s41).toMatch(/SPIKE-WEB-MOBILE.*已关闭/);
    expect(s41).toMatch(/scrollWidth.*360/);
    const row = doc.split("\n").find((l: string) => l.startsWith("| `SPIKE-WEB-MOBILE`"))!;
    expect(row).toContain("已关闭");
  });
});
