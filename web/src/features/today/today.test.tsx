import { withSession } from "../../app/test-session";
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
      <App features={[feature, items]} fetchImpl={withSession(f)} />
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
    expect(within(todays).getByRole("heading", { level: 2 })).toHaveTextContent("（3）");
    const stale = screen.getByRole("group", { name: "久未推进" });
    expect(stale).not.toHaveAttribute("open");
    expect(within(stale).getByText("事项 WB-H")).toBeInTheDocument();
    expect(writes(s)).toHaveLength(0); // 浏览不改任何事项（不自动改期）
  });

  // 对齐工作台「今日焦点」的行上按钮：排时间、交给助手（APA 里是交给 Agent）、完成，另加记进展。
  // 写入后重读那一件（GET /items/{id}），再静默重取 /today 按新分组就地重排；替身的 /today 按当前状态算（未结束、计划不晚于 TODAY）。
  const TODAY = new Date().toISOString().slice(0, 10);
  function live(list: ReturnType<typeof mk>[], groups: { overdue?: string[]; high?: string[] } = {}) {
    const items = new Map(list.map((i) => [i.id, { ...i } as Record<string, any>]));
    const progress: Record<string, { id: string; text: string; at: string }[]> = {};
    const calls: { method: string; path: string; body?: any; headers: Record<string, string> }[] = [];
    const open = (i: Record<string, any>) => !["done", "dropped"].includes(i.phase);
    const impl = (async (url: string, init: RequestInit = {}) => {
      const method = init.method ?? "GET";
      const body = init.body ? JSON.parse(init.body as string) : undefined;
      calls.push({ method, path: url, body, headers: (init.headers ?? {}) as Record<string, string> });
      if (url === "/v1/projects") return json(200, { projects: [{ id: "p-work", name: "工作" }, { id: "p-home", name: "家务" }] });
      if (url === "/v1/today") {
        const all = [...items.values()].filter(open);
        const pick = (ids?: string[]) => all.filter((i) => ids?.includes(i.id));
        return json(200, { planned: all.filter((i) => i.planned_for && i.planned_for <= TODAY), due: [], pending: [], stale: [], overdue: pick(groups.overdue), high_priority: pick(groups.high) });
      }
      if (url === "/v1/items?phase=done") return json(200, { items: [], done_at: {} });
      if (url.startsWith("/v1/items?progress_from=")) {
        const ids = Object.keys(progress);
        return json(200, { items: ids.map((id) => items.get(id)), progress });
      }
      if (url === "/v1/rules") return json(200, { rules: [{ key: "background-run/event_rules/handle-labelled", mode: "auto" }] });
      if (url.startsWith("/v1/agents?")) return json(200, { agents: [], selected: { agent_id: "legacy:hx", agent_name: "主 Agent" }, reason: "" });
      if (url.startsWith("/v1/runs?target=")) return json(200, { runs: [] });
      const m = /^\/v1\/items\/([^/]+)(\/transition|\/progress|\/start)?$/.exec(url);
      const it = m && items.get(m[1]);
      if (!it) return json(404, { code: "not_found", why: "no route", next: "x" });
      if (method === "GET") return json(200, { item: it, relations: [] });
      if (m![2] === "/start") return json(202, { id: it.id });
      if (m![2] === "/progress") {
        (progress[it.id] ??= []).push({ id: it.id + ".p" + (progress[it.id]?.length ?? 0), text: body.text, at: new Date().toISOString() });
        return json(201, { id: it.id });
      }
      if (m![2] === "/transition") it.phase = body.to;
      else for (const [k, v] of Object.entries(body)) if (k !== "reason") it[k] = v;
      it.version++;
      return json(200, { id: it.id, version: it.version });
    }) as unknown as typeof fetch;
    return { impl, calls };
  }
  const todayReads = (s: { calls: { path: string }[] }) => s.calls.filter((c) => c.path === "/v1/today").length;
  const rowOf = async (title: string) => (await screen.findByRole("link", { name: title })).closest("li")!;

  it("ACT-WBN-08 排时间：行上直接选日期与时段，写计划日期与时段两项（带 If-Match），这一行原位显示新时段；改到以后就离开今天并说明去了哪", async () => {
    const s = live([mk("WB-A", { version: 3, planned_for: "2000-01-01" }), mk("WB-B", { planned_for: TODAY })]);
    mount(s.impl);
    const user = userEvent.setup();
    let row = await rowOf("事项 WB-B");
    await user.click(within(row).getByRole("button", { name: "排时间" }));
    let form = within(row).getByRole("form", { name: "排时间：事项 WB-B" });
    await user.click(within(form).getByLabelText("不定时"));
    await user.clear(within(form).getByLabelText("开始"));
    await user.type(within(form).getByLabelText("开始"), "10:00");
    await user.clear(within(form).getByLabelText("结束"));
    await user.type(within(form).getByLabelText("结束"), "11:30");
    await user.click(within(form).getByRole("button", { name: "确定" }));
    await waitFor(() => expect(writes(s)).toHaveLength(1));
    expect(writes(s)[0]).toEqual({ method: "PATCH", path: "/v1/items/WB-B", body: { planned_for: TODAY, planned_slot: "10:00 - 11:30", reason: "在今天页排时间" }, headers: expect.objectContaining({ "If-Match": "1" }) });
    row = await rowOf("事项 WB-B");
    expect(await within(row).findByText("10:00 - 11:30")).toBeInTheDocument();
    expect(within(row).queryByRole("form")).toBeNull(); // 存好后收起
    await waitFor(() => expect(todayReads(s)).toBe(2)); // 首次 + 写后静默重取一次，页面没有回到加载中
    expect(screen.queryByRole("status", { name: /加载中/ })).toBeNull();

    row = await rowOf("事项 WB-A");
    await user.click(within(row).getByRole("button", { name: "排时间" }));
    form = within(row).getByRole("form", { name: "排时间：事项 WB-A" });
    await user.clear(within(form).getByLabelText("计划日期"));
    await user.type(within(form).getByLabelText("计划日期"), "2999-05-06");
    await user.click(within(form).getByRole("button", { name: "确定" }));
    expect(await screen.findByText("「事项 WB-A」已改到 2999-05-06，不在今天的事里了")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "事项 WB-A" })).toBeNull();
  });

  it("ACT-WBN-08 交给 Agent：行上一键加交给 Agent 的标记，在行下展开事项详情同一块 Agent 处理，可直接现在开始；不是任务的没有这个按钮", async () => {
    const s = live([mk("WB-B", { tags: ["home"], planned_for: TODAY }), mk("WB-C", { kind: "commitment", planned_for: TODAY })]);
    mount(s.impl);
    const user = userEvent.setup();
    await user.click(within(await rowOf("事项 WB-B")).getByRole("button", { name: "交给 Agent" }));
    await waitFor(() => expect(writes(s)).toHaveLength(1));
    expect(writes(s)[0]).toMatchObject({ method: "PATCH", path: "/v1/items/WB-B", body: { tags: ["home", "agent-ok"], reason: "交给 Agent 处理" } });
    expect(writes(s)[0].headers["If-Match"]).toBe("1");
    const row = await rowOf("事项 WB-B");
    const block = await within(row).findByRole("group", { name: "Agent 处理：事项 WB-B" });
    expect(within(block).getByRole("checkbox", { name: "交给 Agent 处理", checked: true })).toBeInTheDocument();
    expect(within(row).getByRole("button", { name: "Agent 处理" })).toBeInTheDocument(); // 已交出：按钮改为收放这一块
    await user.click(within(block).getByRole("button", { name: "现在开始" }));
    await waitFor(() => expect(writes(s).map((c) => c.path)).toEqual(["/v1/items/WB-B", "/v1/items/WB-B/start"]));
    expect(within(row).queryByText(/agent-ok/)).toBeNull();
    const other = await rowOf("事项 WB-C");
    expect(within(other).queryByRole("button", { name: "交给 Agent" })).toBeNull();
    expect(within(other).getByRole("button", { name: "完成" })).toBeInTheDocument();
  });

  it("ACT-WBN-08 完成：一次写入；做完的这一件按服务端的新分组离开今天的事，块顶说明它已完成，其余行不动", async () => {
    const s = live([mk("WB-A", { planned_for: TODAY }), mk("WB-B", { planned_for: TODAY })]);
    mount(s.impl);
    const user = userEvent.setup();
    await user.click(within(await rowOf("事项 WB-B")).getByRole("button", { name: "完成" }));
    await waitFor(() => expect(writes(s)).toHaveLength(1));
    expect(writes(s)[0]).toMatchObject({ method: "POST", path: "/v1/items/WB-B/transition", body: { to: "done", reason: "在今天页完成" } });
    expect(await screen.findByRole("link", { name: "「事项 WB-B」已完成，不在今天的事里了" })).toHaveAttribute("href", "/items/WB-B");
    expect(screen.queryByRole("link", { name: "事项 WB-B" })).toBeNull();
    expect(within(await rowOf("事项 WB-A")).getAllByRole("button").map((b) => b.textContent)).toEqual(["排时间", "交给 Agent", "完成", "记进展", "更多"]);
    expect(within(screen.getByRole("region", { name: "今天的事" })).getByRole("heading", { level: 2 })).toHaveTextContent("（1）");
  });

  it("记进展：行上按钮就地展开一个输入框与保存，写一次进展；这一行留在原处，底部本周进展条数随即加一（同事同日算一条）", async () => {
    const s = live([mk("WB-A", { planned_for: TODAY }), mk("WB-B", { planned_for: TODAY })]);
    mount(s.impl);
    const user = userEvent.setup();
    const row = await rowOf("事项 WB-B");
    await user.click(within(row).getByRole("button", { name: "记进展" }));
    const box = within(row).getByRole("group", { name: "记进展：事项 WB-B" });
    await user.type(within(box).getByLabelText("进展"), "已订酒店");
    await user.click(within(box).getByRole("button", { name: "保存" }));
    await waitFor(() => expect(writes(s)).toEqual([expect.objectContaining({ method: "POST", path: "/v1/items/WB-B/progress", body: { text: "已订酒店" } })]));
    await waitFor(() => expect(within(row).queryByRole("group", { name: "记进展：事项 WB-B" })).toBeNull()); // 存好后收起，行还在原处
    expect(screen.getByRole("link", { name: "事项 WB-B" }).closest("li")).toBe(row);
    expect(await screen.findByText(/本周进展 1 条/)).toBeInTheDocument();
    await user.click(within(row).getByRole("button", { name: "记进展" }));
    await user.type(within(within(row).getByRole("group", { name: "记进展：事项 WB-B" })).getByLabelText("进展"), "又一条");
    await user.click(within(row).getByRole("button", { name: "保存" }));
    await waitFor(() => expect(writes(s)).toHaveLength(2));
    expect(screen.getByText(/本周进展 1 条/)).toBeInTheDocument(); // 同一件事同一天两条仍算一条
  });

  it("按项目分组；逾期标原截止日、高优先级标紧急或高、计划过了的标已过，三类各用各的标记，同一件只列一次", async () => {
    const s = live([
      mk("A", { project: "p-home", title: "交电费", planned_for: TODAY }),
      mk("B", { project: "p-work", title: "交报告", deadline: "2026-11-20T18:00:00+08:00" }),
      mk("C", { project: "p-work", title: "修线上问题", priority: "urgent", planned_for: "2000-01-02" }),
      mk("D", { title: "看牙", priority: "high" }),
    ], { overdue: ["B"], high: ["C", "D"] });
    mount(s.impl);
    const todays = await screen.findByRole("region", { name: "今天的事" });
    await waitFor(() => expect(within(todays).getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual(["工作 2", "家务 1", "没归项目的 1"]));
    expect(within(todays).getByRole("heading", { level: 2 })).toHaveTextContent("（4）");
    const marks = (title: string) => [...screen.getByRole("link", { name: title }).closest("li")!.querySelectorAll(".reasons > span")].map((x) => `${x.className}|${x.textContent}`);
    expect(marks("交报告")).toEqual(["badge bad|逾期 · 原截止 11-20"]);
    expect(marks("修线上问题")).toEqual(["badge warn|紧急", "badge plain|计划 01-02，已过"]);
    expect(marks("看牙")).toEqual(["badge warn|高优先级"]);
    expect(marks("交电费")).toEqual(["badge plain|计划今天"]);
    expect(within(within(todays).getByRole("region", { name: "工作" })).getAllByRole("link").map((l) => l.textContent)).toEqual(["工作", "修线上问题", "交报告"]);
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

describe("今天页降噪：每一行都是本人要知道或要动手的", () => {
  it("简报成功不占「Agent 交回的」，失败的列出并写明下一步；已关掉的失败不再列；简报类投递不进「提醒」", async () => {
    const at = new Date(Date.now() + 60_000).toISOString();
    const run = (id: string, target: string, state: string) => ({ id, target, category: "task", state, finished_at: at });
    const s = server({ planned: [], due: [], pending: [], stale: [] }, () => false, {
      "/v1/runs?category=background,task&state=succeeded,failed,timeout&limit=100": { runs: [
        run("r1", "B-OK", "succeeded"), run("r2", "B-FAIL", "failed"), run("r3", "T-OK", "succeeded"), run("r4", "B-GONE", "failed")] },
      "/v1/items/B-OK": { item: mk("B-OK", { kind: "brief", phase: "done", title: "brief 11-23" }) },
      "/v1/items/B-FAIL": { item: mk("B-FAIL", { kind: "brief", title: "brief 11-24" }) },
      "/v1/items/T-OK": { item: mk("T-OK", { title: "委派的活" }) },
      "/v1/items/B-GONE": { item: mk("B-GONE", { kind: "brief", phase: "dropped", title: "brief 11-22" }) },
      "/v1/deliveries/unread": { deliveries: [
        { id: "brief:B-OK", item: "B-OK", class: "brief", content: { title: "brief 11-23" } },
        { id: "remind:1", item: "T-OK", class: "reminder", content: { title: "截止已过：交材料" } }] },
    });
    mount(s.impl);
    const back = await screen.findByRole("region", { name: "Agent 交回的" });
    await waitFor(() => expect(within(back).getAllByRole("listitem")).toHaveLength(2));
    expect(within(back).queryByText("brief 11-23")).not.toBeInTheDocument();
    expect(within(back).queryByText("brief 11-22")).not.toBeInTheDocument();
    const rows = within(back).getAllByRole("listitem").map((li) => li.textContent ?? "");
    expect(rows.filter((x) => x.includes("没做完") && x.includes("打开看原因，决定重跑、改派还是放弃"))).toHaveLength(1);
    expect(rows.filter((x) => x.includes("做完了"))).toHaveLength(1); // 非模板运行（委派的活）照旧列出
    const reminders = await screen.findByRole("region", { name: "提醒" });
    expect(within(reminders).getByText("截止已过：交材料")).toBeInTheDocument();
    expect(within(reminders).queryByText("brief 11-23")).not.toBeInTheDocument();
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
